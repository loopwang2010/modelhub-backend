// Postgres-backed exercise of every PostgresDialect SQL string.
//
// The shape contracts in coverage_test.go check that each Postgres SQL
// string contains the expected tokens. This file actually executes them
// against a real Postgres 16 instance via the F7 testcontainer harness so
// we know:
//
//  1. The SQL parses cleanly under Postgres' planner (catches typos that
//     a string-substring match would miss).
//  2. The parameter binding shape matches what the Repo passes at runtime.
//  3. SELECT … FOR UPDATE SKIP LOCKED actually skips contended rows
//     instead of blocking.
//  4. RETURNING in the claim path returns the full canonical column list
//     so scanTask can decode the row.
//
// All tests in this file rely on testutil.NewPostgresDB which calls
// t.Skip when Docker is unavailable. The Windows host running this
// branch has no Docker — so every test here SKIPs locally and runs for
// real on a Docker-enabled CI runner.

package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/testutil"
)

// newPostgresRepo wires NewPostgresDB to a Repo with PostgresDialect.
// The harness applies all goose migrations so the schema matches prod.
func newPostgresRepo(t *testing.T) (*sql.DB, *Repo) {
	t.Helper()
	db := testutil.NewPostgresDB(t) // skips on no-Docker
	return db, NewRepo(db, PostgresDialect{})
}

// TestPostgresDialect_InsertAndSelectRoundTrip walks one task through the
// trivial create + lookup operations against real Postgres so every
// SELECT / INSERT in the dialect parses and returns the canonical column
// shape scanTask expects.
func TestPostgresDialect_InsertAndSelectRoundTrip(t *testing.T) {
	t.Parallel()
	_, repo := newPostgresRepo(t)
	ctx := context.Background()

	p := NewTaskParams{
		AccountID:      "acct-pg-roundtrip",
		ModelKey:       adapter.ModelKey("test-model"),
		ProviderKey:    adapter.ProviderKey("mock-async"),
		ParamsJSON:     json.RawMessage(`{"prompt":"hello pg"}`),
		IdempotencyKey: "key-pg-roundtrip",
		HeldAmount:     adapter.CostUSD(60_000),
		SLATimeout:     5 * time.Minute,
	}
	task, err := repo.Create(ctx, p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.State != StateCreated {
		t.Errorf("state = %q; want created", task.State)
	}
	// FindByID
	got, err := repo.FindByID(ctx, task.ID)
	if err != nil || got == nil {
		t.Fatalf("FindByID: %v / %v", err, got)
	}
	if got.AccountID != p.AccountID {
		t.Errorf("AccountID round-trip mismatch: %q vs %q", got.AccountID, p.AccountID)
	}
	// FindByWebhookToken
	byTok, err := repo.FindByWebhookToken(ctx, task.WebhookToken)
	if err != nil || byTok == nil || byTok.ID != task.ID {
		t.Errorf("FindByWebhookToken mismatch: %v %v", err, byTok)
	}
	// FindByIdempotencyKey
	byKey, err := repo.FindByIdempotencyKey(ctx, p.IdempotencyKey)
	if err != nil || byKey == nil || byKey.ID != task.ID {
		t.Errorf("FindByIdempotencyKey mismatch: %v %v", err, byKey)
	}
	// FindByID for missing → (nil, nil)
	missing, err := repo.FindByID(ctx, "gen_doesnotexist")
	if err != nil || missing != nil {
		t.Errorf("FindByID missing = %v / %v; want nil/nil", missing, err)
	}
	// FindByWebhookToken empty → (nil, nil)
	if r, err := repo.FindByWebhookToken(ctx, ""); err != nil || r != nil {
		t.Errorf("FindByWebhookToken empty = %v / %v", r, err)
	}
	// FindByIdempotencyKey empty → (nil, nil)
	if r, err := repo.FindByIdempotencyKey(ctx, ""); err != nil || r != nil {
		t.Errorf("FindByIdempotencyKey empty = %v / %v", r, err)
	}

	// ListEvents — at least the initial Created event is present.
	evs, err := repo.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) == 0 || evs[0].ToState != string(StateCreated) {
		t.Errorf("ListEvents = %+v; want at least one created event", evs)
	}
}

// TestPostgresDialect_TransitionsExerciseEveryUpdateSQL drives a single
// task through every state-mutation SQL string the dialect exposes:
//
//	Created → Held (UpdateState + UpdateHeldAmount)
//	Held → Submitted (UpdateState + UpdateSubmitted)
//	Submitted → Running (UpdateState only)
//	SchedulePoll (UpdateSchedule)
//	Running → Succeeded (UpdateState + UpdateActualCostAndTerminal)
//
// Then a sibling task Created → Held → Failed exercises
// UpdateErrorAndTerminal, and a third Created → Held → Cancelled
// (then re-attempt) exercises UpdateTerminalAt + ErrTerminalState.
func TestPostgresDialect_TransitionsExerciseEveryUpdateSQL(t *testing.T) {
	t.Parallel()
	_, repo := newPostgresRepo(t)
	ctx := context.Background()

	makeParams := func(seed string) NewTaskParams {
		return NewTaskParams{
			AccountID:      "acct-" + seed,
			ModelKey:       adapter.ModelKey("test-model"),
			ProviderKey:    adapter.ProviderKey("mock-async"),
			ParamsJSON:     json.RawMessage(`{"prompt":"x"}`),
			IdempotencyKey: "key-" + seed,
			HeldAmount:     adapter.CostUSD(60_000),
			SLATimeout:     5 * time.Minute,
		}
	}

	// Happy path through Succeeded.
	t1, err := repo.Create(ctx, makeParams("succ"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.MarkHeld(ctx, t1.ID, 60_000); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	if err := repo.MarkSubmittedAsync(ctx, t1.ID, "ref-1", "https://poll.example/1", time.Now().UTC()); err != nil {
		t.Fatalf("MarkSubmittedAsync: %v", err)
	}
	if err := repo.MarkRunning(ctx, t1.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := repo.SchedulePoll(ctx, t1.ID, time.Now().UTC().Add(30*time.Second), 1); err != nil {
		t.Fatalf("SchedulePoll: %v", err)
	}
	if err := repo.MarkSucceeded(ctx, t1.ID, 50_000); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}
	out, err := repo.FindByID(ctx, t1.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if out.State != StateSucceeded || out.ActualCost != 50_000 {
		t.Errorf("succeeded task: state=%q cost=%d", out.State, out.ActualCost)
	}
	if out.TerminalAt == nil {
		t.Errorf("TerminalAt should be set for succeeded")
	}
	if out.UpstreamRef != "ref-1" || out.PollingURL != "https://poll.example/1" {
		t.Errorf("submitted fields not persisted: %+v", out)
	}

	// Failure path.
	t2, err := repo.Create(ctx, makeParams("fail"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.MarkHeld(ctx, t2.ID, 60_000); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	if err := repo.MarkFailed(ctx, t2.ID, adapter.ErrClassUpstream, "boom", []byte("upstream-500")); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	out2, _ := repo.FindByID(ctx, t2.ID)
	if out2.State != StateFailed || out2.LastErrorClass != string(adapter.ErrClassUpstream) || string(out2.RawError) != "upstream-500" {
		t.Errorf("failed fields wrong: %+v rawError=%q", out2, out2.RawError)
	}

	// Cancellation path. After cancel, a second cancel must report terminal.
	t3, err := repo.Create(ctx, makeParams("canc"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.MarkHeld(ctx, t3.ID, 60_000); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	if err := repo.MarkCancelled(ctx, t3.ID, "user_request"); err != nil {
		t.Fatalf("MarkCancelled: %v", err)
	}
	if err := repo.MarkCancelled(ctx, t3.ID, "again"); !isAlreadyTerminal(err) {
		t.Errorf("second cancel = %v; want already-terminal", err)
	}

	// TimedOut path.
	t4, err := repo.Create(ctx, makeParams("to"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.MarkHeld(ctx, t4.ID, 60_000); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	if err := repo.MarkTimedOut(ctx, t4.ID, "sla_exceeded"); err != nil {
		t.Fatalf("MarkTimedOut: %v", err)
	}
	out4, _ := repo.FindByID(ctx, t4.ID)
	if out4.State != StateTimedOut || out4.TerminalAt == nil {
		t.Errorf("timed_out task: %+v", out4)
	}
}

// TestPostgresDialect_ClaimNextTask_SkipLockedSemantics is the heart of
// the production worker pool: SELECT FOR UPDATE SKIP LOCKED must let
// concurrent workers each grab a distinct row without one blocking the
// other. Two concurrent claims against the same provider with two ready
// rows must return two different task IDs.
func TestPostgresDialect_ClaimNextTask_SkipLockedSemantics(t *testing.T) {
	t.Parallel()
	_, repo := newPostgresRepo(t)
	ctx := context.Background()

	const provider = adapter.ProviderKey("mock-async")
	mk := func(seed string) NewTaskParams {
		return NewTaskParams{
			AccountID:      "acct-" + seed,
			ModelKey:       adapter.ModelKey("test-model"),
			ProviderKey:    provider,
			ParamsJSON:     json.RawMessage(`{"p":1}`),
			IdempotencyKey: "claim-" + seed,
			HeldAmount:     adapter.CostUSD(1_000),
			SLATimeout:     10 * time.Minute,
		}
	}
	// Need two ready-to-claim rows in StateSubmitted with no next_poll_after.
	a := mustWalkToSubmitted(t, repo, mk("a"))
	b := mustWalkToSubmitted(t, repo, mk("b"))

	type claimResult struct {
		task *Task
		err  error
	}
	out := make(chan claimResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			tt, err := repo.ClaimNextTask(ctx, provider)
			out <- claimResult{tt, err}
		}()
	}
	gotIDs := map[string]bool{}
	for i := 0; i < 2; i++ {
		r := <-out
		if r.err != nil {
			t.Fatalf("Claim err: %v", r.err)
		}
		if r.task == nil {
			t.Fatalf("Claim returned nil task")
		}
		gotIDs[r.task.ID] = true
	}
	if !gotIDs[a.ID] || !gotIDs[b.ID] {
		t.Errorf("expected both %s and %s claimed exactly once; got %v", a.ID, b.ID, gotIDs)
	}

	// Third claim with no remaining rows → (nil, nil).
	if r, err := repo.ClaimNextTask(ctx, provider); r != nil || err != nil {
		t.Errorf("ClaimNextTask exhausted = %v / %v; want nil/nil", r, err)
	}
}

// TestPostgresDialect_FindStuck_AndFindTimedOut against real Postgres so
// the dialect's FindStuckSQL / FindTimedOutSQL parse and return the
// canonical row shape. Verifies BOTH the populated and empty paths.
func TestPostgresDialect_FindStuck_AndFindTimedOut(t *testing.T) {
	t.Parallel()
	db, repo := newPostgresRepo(t)
	ctx := context.Background()

	// Empty.
	if got, err := repo.FindStuck(ctx, time.Minute, 10); err != nil || len(got) != 0 {
		t.Errorf("FindStuck empty = %v / %d", err, len(got))
	}
	if got, err := repo.FindTimedOut(ctx, 10); err != nil || len(got) != 0 {
		t.Errorf("FindTimedOut empty = %v / %d", err, len(got))
	}

	// Stuck row: a Submitted task with next_poll_after well in the past.
	stuck := mustWalkToSubmitted(t, repo, NewTaskParams{
		AccountID:      "acct-stuck-pg",
		ModelKey:       adapter.ModelKey("m"),
		ProviderKey:    adapter.ProviderKey("p"),
		ParamsJSON:     json.RawMessage(`{}`),
		IdempotencyKey: "stuck-pg-1",
		HeldAmount:     1000,
		SLATimeout:     5 * time.Minute,
	})
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.ExecContext(ctx, `UPDATE task SET next_poll_after = $1 WHERE id = $2`, past, stuck.ID); err != nil {
		t.Fatalf("backdate next_poll_after: %v", err)
	}
	stuckList, err := repo.FindStuck(ctx, 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("FindStuck: %v", err)
	}
	if len(stuckList) != 1 || stuckList[0].ID != stuck.ID {
		t.Errorf("FindStuck got %v; want [%s]", stuckList, stuck.ID)
	}

	// TimedOut row: Held with sla_deadline in the past.
	t2, err := repo.Create(ctx, NewTaskParams{
		AccountID:      "acct-to-pg",
		ModelKey:       adapter.ModelKey("m"),
		ProviderKey:    adapter.ProviderKey("p"),
		ParamsJSON:     json.RawMessage(`{}`),
		IdempotencyKey: "timedout-pg-1",
		HeldAmount:     1000,
		SLATimeout:     5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkHeld(ctx, t2.ID, 1000); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, t2.ID); err != nil {
		t.Fatalf("backdate sla: %v", err)
	}
	to, err := repo.FindTimedOut(ctx, 10)
	if err != nil {
		t.Fatalf("FindTimedOut: %v", err)
	}
	found := false
	for _, x := range to {
		if x.ID == t2.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("timed-out task not surfaced; got %v", to)
	}
}

// TestPostgresDialect_CreateSchemaSQL_AppliesCleanly: the dialect's
// CreateSchemaSQL is exposed for non-goose test setups. Verify the
// statements actually parse and run by applying them against a fresh
// admin database (not the goose-migrated one) — catches typos like
// missing parentheses or unknown column types.
func TestPostgresDialect_CreateSchemaSQL_AppliesCleanly(t *testing.T) {
	t.Parallel()
	db, _ := newPostgresRepo(t)
	ctx := context.Background()

	// Drop the goose-managed tables first so the CreateSchemaSQL
	// statements can land into a clean slate.
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS task_event CASCADE`); err != nil {
		t.Fatalf("drop task_event: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS task CASCADE`); err != nil {
		t.Fatalf("drop task: %v", err)
	}

	for _, stmt := range (PostgresDialect{}).CreateSchemaSQL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("CreateSchemaSQL stmt failed: %v\nSQL:\n%s", err, stmt)
		}
	}

	// Sanity: re-running is idempotent (uses IF NOT EXISTS).
	for _, stmt := range (PostgresDialect{}).CreateSchemaSQL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("re-run not idempotent: %v\nSQL: %s", err, stmt)
		}
	}
}

// TestPostgresDialect_ClaimNextTaskSQL_ContainsForUpdateSkipLocked is a
// lightweight invariant: the production claim must remain SKIP LOCKED.
// Belt-and-suspenders alongside the shape check in coverage_test.go;
// kept here so the Postgres-only file is self-documenting.
func TestPostgresDialect_ClaimNextTaskSQL_ContainsForUpdateSkipLocked(t *testing.T) {
	sql := PostgresDialect{}.ClaimNextTaskSQL()
	for _, want := range []string{"FOR UPDATE", "SKIP LOCKED", "RETURNING"} {
		if !strings.Contains(sql, want) {
			t.Errorf("PostgresDialect.ClaimNextTaskSQL missing %q\n%s", want, sql)
		}
	}
}

// mustWalkToSubmitted creates a task and walks Created → Held → Submitted
// against the supplied repo. Used by Postgres-only tests where the
// SQLite test scaffolding isn't available.
func mustWalkToSubmitted(t *testing.T, repo *Repo, p NewTaskParams) *Task {
	t.Helper()
	ctx := context.Background()
	task, err := repo.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkHeld(ctx, task.ID, p.HeldAmount); err != nil {
		t.Fatalf("mark held: %v", err)
	}
	ref := adapter.UpstreamRef("ref-" + task.ID)
	if err := repo.MarkSubmittedAsync(ctx, task.ID, ref, "", time.Now().UTC()); err != nil {
		t.Fatalf("mark submitted: %v", err)
	}
	out, err := repo.FindByID(ctx, task.ID)
	if err != nil || out == nil {
		t.Fatalf("re-find: %v / %v", err, out)
	}
	return out
}
