// Coverage-targeted tests for branches that the smoke suites
// (worker_test, repo_test, dedup_test, state_test, backoff_test) don't
// naturally exercise. Kept in a separate file so the smoke flow stays
// readable.
//
// Coverage focus per plans/CODE-REVIEW.md F3:
//   - reconciler.go : NewReconciler, Run, SweepOnce, sweepStuck, sweepTimedOut
//                     (all 0% prior).
//   - repo.go       : MarkCancelled, FindStuck, FindTimedOut, Dialect
//                     (all 0% prior).
//   - dialect.go    : PostgresDialect SQL strings (production path) verified
//                     via shape contracts, since the SQLite-backed test
//                     suite cannot reach Postgres SKIP LOCKED / FOR UPDATE.
//
// As with internal/wallet/coverage_test.go, the production Postgres SQL
// surface is locked via shape assertions; a real Postgres testcontainer
// suite is deferred per F3's design intent.

package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/events"
)

// ─────────────────────────────────────────────────────────────────────────
// PostgresDialect SQL string shape contracts
// ─────────────────────────────────────────────────────────────────────────

func TestPostgresDialect_SQLShape(t *testing.T) {
	d := PostgresDialect{}
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"InsertTask", d.InsertTaskSQL(), []string{"INSERT INTO task", "params_json", "$12"}},
		{"InsertTaskEvent", d.InsertTaskEventSQL(), []string{"INSERT INTO task_event", "$6"}},
		{"SelectTaskByID", d.SelectTaskByIDSQL(), []string{"FROM task", "WHERE id = $1"}},
		{"SelectByWebhook", d.SelectTaskByWebhookTokenSQL(), []string{"webhook_token = $1"}},
		{"SelectByIdempotency", d.SelectTaskByIdempotencyKeySQL(), []string{"idempotency_key = $1"}},
		{"LockTaskByID", d.LockTaskByIDSQL(), []string{"FOR UPDATE", "WHERE id = $1"}},
		{"UpdateState", d.UpdateStateSQL(), []string{"UPDATE task", "SET state = $1", "WHERE id = $3"}},
		{"UpdateHeldAmount", d.UpdateHeldAmountSQL(), []string{"held_amount = $1", "$3"}},
		{"UpdateSubmitted", d.UpdateSubmittedSQL(), []string{"upstream_ref = $1", "polling_url = $2", "$5"}},
		{"UpdateSchedule", d.UpdateScheduleSQL(), []string{"next_poll_after = $1", "poll_attempt = $2", "$4"}},
		{"UpdateActualCostAndTerminal", d.UpdateActualCostAndTerminalSQL(), []string{"actual_cost = $1", "terminal_at = $2", "$4"}},
		{"UpdateErrorAndTerminal", d.UpdateErrorAndTerminalSQL(), []string{"last_error_class = $1", "raw_error = $2", "$5"}},
		{"UpdateTerminalAt", d.UpdateTerminalAtSQL(), []string{"terminal_at = $1", "$3"}},
		// ClaimNextTaskSQL is the canonical Postgres queue pattern: must
		// have FOR UPDATE SKIP LOCKED and atomic UPDATE…RETURNING.
		{"ClaimNextTask", d.ClaimNextTaskSQL(), []string{
			"UPDATE task", "FOR UPDATE SKIP LOCKED", "RETURNING",
			"poll_attempt = poll_attempt + 1", "$1", "$2",
		}},
		{"FindStuck", d.FindStuckSQL(), []string{
			"FROM task", "state IN ('submitted', 'running')",
			"next_poll_after < $1", "LIMIT $2",
		}},
		{"FindTimedOut", d.FindTimedOutSQL(), []string{
			"state IN ('held', 'submitted', 'running')",
			"sla_deadline < $1", "LIMIT $2",
		}},
		{"ListEvents", d.ListEventsSQL(), []string{
			"FROM task_event", "task_id = $1", "ORDER BY created_at",
		}},
	}
	for _, tc := range cases {
		for _, want := range tc.want {
			if !strings.Contains(tc.sql, want) {
				t.Errorf("%s missing %q\nfull SQL:\n%s", tc.name, want, tc.sql)
			}
		}
	}

	// CreateSchemaSQL exists for Postgres-in-tests; verify both core tables.
	schema := strings.Join(d.CreateSchemaSQL(), "\n")
	for _, want := range []string{"CREATE TABLE", "task", "task_event", "BIGSERIAL"} {
		if !strings.Contains(schema, want) {
			t.Errorf("PostgresDialect.CreateSchemaSQL missing %q", want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Repo: MarkCancelled, FindStuck, FindTimedOut, Dialect getter
// ─────────────────────────────────────────────────────────────────────────

func TestRepo_DialectGetter(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	if _, ok := repo.Dialect().(SQLiteDialect); !ok {
		t.Errorf("Dialect() returned %T; want SQLiteDialect", repo.Dialect())
	}
}

func TestRepo_MarkCancelled_HappyAndAlreadyTerminal(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	task, err := repo.Create(ctx, newTestTaskParams("cancel-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkCancelled(ctx, task.ID, "user_request"); err != nil {
		t.Fatalf("MarkCancelled: %v", err)
	}
	out, err := repo.FindByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if out.State != StateCancelled {
		t.Errorf("state = %q; want cancelled", out.State)
	}

	// Replay: second MarkCancelled on a terminal task → ErrAlreadyTerminal.
	err = repo.MarkCancelled(ctx, task.ID, "again")
	if !isAlreadyTerminal(err) {
		t.Errorf("replay MarkCancelled = %v; want ErrAlreadyTerminal", err)
	}
}

func TestRepo_FindStuck_EmptyAndPopulated(t *testing.T) {
	db, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Empty table → empty slice, no error.
	got, err := repo.FindStuck(ctx, time.Minute, 10)
	if err != nil {
		t.Fatalf("FindStuck empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty; got %d rows", len(got))
	}

	// Create a task, walk it Created→Held→Submitted (giving it
	// next_poll_after via a manual UPDATE), then move next_poll_after
	// well into the past so FindStuck picks it up.
	task := mustCreateAndSubmit(t, repo, newTestTaskParams("stuck-1"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.Exec(
		`UPDATE task SET next_poll_after = $1 WHERE id = $2`, past, task.ID,
	); err != nil {
		t.Fatalf("set next_poll_after: %v", err)
	}

	stuck, err := repo.FindStuck(ctx, 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("FindStuck: %v", err)
	}
	if len(stuck) != 1 || stuck[0].ID != task.ID {
		t.Errorf("FindStuck = %v; want [%s]", stuck, task.ID)
	}

	// Recent-enough task should not appear.
	task2 := mustCreateAndSubmit(t, repo, newTestTaskParams("stuck-2"))
	soon := time.Now().UTC().Add(-1 * time.Minute) // recent
	if _, err := db.Exec(
		`UPDATE task SET next_poll_after = $1 WHERE id = $2`, soon, task2.ID,
	); err != nil {
		t.Fatalf("set next_poll_after: %v", err)
	}
	stuck2, _ := repo.FindStuck(ctx, 5*time.Minute, 10)
	for _, s := range stuck2 {
		if s.ID == task2.ID {
			t.Errorf("recent task %s incorrectly classified as stuck", task2.ID)
		}
	}
}

func TestRepo_FindTimedOut_EmptyAndPopulated(t *testing.T) {
	db, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	got, err := repo.FindTimedOut(ctx, 10)
	if err != nil {
		t.Fatalf("FindTimedOut empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty; got %d rows", len(got))
	}

	// A task in Held with sla_deadline in the past must surface.
	task, err := repo.Create(ctx, newTestTaskParams("timeout-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkHeld(ctx, task.ID, 60_000); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.Exec(`UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID); err != nil {
		t.Fatalf("set sla_deadline: %v", err)
	}

	timedOut, err := repo.FindTimedOut(ctx, 10)
	if err != nil {
		t.Fatalf("FindTimedOut: %v", err)
	}
	if len(timedOut) != 1 || timedOut[0].ID != task.ID {
		t.Errorf("FindTimedOut = %v; want [%s]", timedOut, task.ID)
	}

	// Terminal task should NOT appear.
	terminal, _ := repo.Create(ctx, newTestTaskParams("timeout-2"))
	_ = repo.MarkHeld(ctx, terminal.ID, 60_000)
	_ = repo.MarkCancelled(ctx, terminal.ID, "user")
	if _, err := db.Exec(`UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, terminal.ID); err != nil {
		t.Fatalf("set sla_deadline on terminal: %v", err)
	}
	again, _ := repo.FindTimedOut(ctx, 10)
	for _, x := range again {
		if x.ID == terminal.ID {
			t.Errorf("terminal task %s should not surface", terminal.ID)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Reconciler
// ─────────────────────────────────────────────────────────────────────────

func TestNewReconciler_Defaults(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	r := NewReconciler(repo, bus)
	if r.StuckThreshold != DefaultStuckThreshold {
		t.Errorf("StuckThreshold = %v; want %v", r.StuckThreshold, DefaultStuckThreshold)
	}
	if r.Interval != DefaultReconcileInterval {
		t.Errorf("Interval = %v; want %v", r.Interval, DefaultReconcileInterval)
	}
	if r.BatchLimit != DefaultBatchLimit {
		t.Errorf("BatchLimit = %d; want %d", r.BatchLimit, DefaultBatchLimit)
	}
	if r.nowFn == nil {
		t.Error("nowFn must default to a non-nil clock")
	}
}

func TestNewReconciler_PanicOnNilRepo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil repo")
		}
	}()
	_ = NewReconciler(nil, events.NewMemoryBus())
}

func TestNewReconciler_PanicOnNilBus(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil bus")
		}
	}()
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	_ = NewReconciler(repo, nil)
}

// SweepOnce on an empty table is a no-op (no panic, no events).
func TestReconciler_SweepOnce_EmptyIsNoop(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	rec := NewReconciler(repo, bus)
	recorded := newRecordedEvents(bus)

	rec.SweepOnce(context.Background())

	if got := recorded.ByKind("task_timed_out"); len(got) != 0 {
		t.Errorf("expected no TaskTimedOut; got %d", len(got))
	}
}

// sweepStuck re-arms next_poll_after on a task whose schedule has expired
// well past the threshold. Worker would re-claim on its next iteration.
func TestReconciler_SweepStuck_ReschedulesExpiredTask(t *testing.T) {
	db, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	rec := NewReconciler(repo, bus)
	rec.StuckThreshold = time.Minute // tighten for the test

	// Inject a fake clock so we can verify SchedulePoll uses nowFn.
	fakeNow := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	rec.nowFn = func() time.Time { return fakeNow }

	task := mustCreateAndSubmit(t, repo, newTestTaskParams("stuck-r"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.Exec(
		`UPDATE task SET next_poll_after = $1 WHERE id = $2`, past, task.ID,
	); err != nil {
		t.Fatalf("set next_poll_after: %v", err)
	}

	rec.SweepOnce(context.Background())

	// next_poll_after should now equal fakeNow (re-armed).
	out, err := repo.FindByID(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("re-find: %v", err)
	}
	if out.NextPollAfter == nil || !out.NextPollAfter.Equal(fakeNow) {
		t.Errorf("next_poll_after = %v; want %v", out.NextPollAfter, fakeNow)
	}
}

// sweepTimedOut transitions a submitted task past its SLA deadline →
// StateTimedOut and emits TaskTimedOut on the bus.
//
// Latent FSM note: FindTimedOutSQL selects rows with state IN
// ('held', 'submitted', 'running'), but the transition table only allows
// Submitted/Running → TimedOut. A task stuck in Held past its SLA gets
// silently skipped on every reconciler tick (the err != nil branch in
// sweepTimedOut just `continue`s). Tracked as a Sprint-2 follow-up
// alongside the F2/F3 list.
func TestReconciler_SweepTimedOut_TransitionsAndEmits(t *testing.T) {
	db, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	rec := NewReconciler(repo, bus)
	recorded := newRecordedEvents(bus)

	task := mustCreateAndSubmit(t, repo, newTestTaskParams("to-1"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.Exec(`UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID); err != nil {
		t.Fatalf("set sla_deadline: %v", err)
	}

	rec.SweepOnce(context.Background())

	// Verify state transition.
	out, err := repo.FindByID(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("re-find: %v", err)
	}
	if out.State != StateTimedOut {
		t.Errorf("state = %q; want timed_out", out.State)
	}
	// Verify bus saw TaskTimedOut for this task.
	emitted := recorded.ByKind("task_timed_out")
	if len(emitted) != 1 {
		t.Fatalf("expected 1 TaskTimedOut; got %d", len(emitted))
	}
	if emitted[0].GetTaskID() != events.TaskID(task.ID) {
		t.Errorf("TaskTimedOut taskID = %q; want %q", emitted[0].GetTaskID(), task.ID)
	}
}

// sweepTimedOut should silently skip tasks that race to terminal between
// the FindTimedOut and MarkTimedOut calls. Simulate by manually marking
// the task cancelled after FindTimedOut sees it but before our manual
// MarkTimedOut. Since we can't intercept the call easily without
// restructuring, the test instead verifies that the next sweep on a
// task already in StateTimedOut is a no-op (FindTimedOut excludes it).
func TestReconciler_SweepTimedOut_SkipsAlreadyTerminal(t *testing.T) {
	db, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	rec := NewReconciler(repo, bus)

	task := mustCreateAndSubmit(t, repo, newTestTaskParams("to-skip"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	_, _ = db.Exec(`UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID)

	rec.SweepOnce(context.Background()) // first sweep transitions it

	// Second sweep: task is already StateTimedOut → FindTimedOut excludes
	// it → no further events emitted.
	recorded := newRecordedEvents(bus)
	rec.SweepOnce(context.Background())
	if got := recorded.ByKind("task_timed_out"); len(got) != 0 {
		t.Errorf("second sweep emitted %d TaskTimedOut; want 0", len(got))
	}
}

// Run honors context cancellation and exits cleanly.
func TestReconciler_Run_StopsOnCtxCancel(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	rec := NewReconciler(repo, bus)
	rec.Interval = 50 * time.Millisecond // fast tick for the test

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Run(ctx)
		close(done)
	}()

	// Let one or two ticks fire, then cancel.
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// happy
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of ctx.Cancel")
	}
}

// Run with a zero/negative Interval falls back to the default.
func TestReconciler_Run_ZeroIntervalFallsBackToDefault(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	rec := NewReconciler(repo, bus)
	rec.Interval = 0 // would otherwise panic in time.NewTicker

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Run(ctx) // must not panic
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit; defaulting fell through")
	}
}

// Sweeps respect a zero/negative BatchLimit by falling back to the default.
func TestReconciler_Sweep_ZeroBatchLimitFallsBack(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	bus := events.NewMemoryBus()
	defer bus.Close()
	rec := NewReconciler(repo, bus)
	rec.BatchLimit = 0

	// Empty table, just ensure no panic.
	rec.SweepOnce(context.Background())
}

// ─────────────────────────────────────────────────────────────────────────
// validateCreateParams — exhaustive table-driven coverage of the
// every-field-required guard. Pure function, cheap to cover fully.
// ─────────────────────────────────────────────────────────────────────────

func TestValidateCreateParams_AllBranches(t *testing.T) {
	good := newTestTaskParams("ok") // already valid

	cases := []struct {
		name    string
		mutate  func(p *NewTaskParams)
		wantErr bool
	}{
		{"valid", func(*NewTaskParams) {}, false},
		{"empty AccountID", func(p *NewTaskParams) { p.AccountID = "" }, true},
		{"empty ModelKey", func(p *NewTaskParams) { p.ModelKey = "" }, true},
		{"empty ProviderKey", func(p *NewTaskParams) { p.ProviderKey = "" }, true},
		{"empty ParamsJSON", func(p *NewTaskParams) { p.ParamsJSON = nil }, true},
		{"zero SLATimeout", func(p *NewTaskParams) { p.SLATimeout = 0 }, true},
		{"negative SLATimeout", func(p *NewTaskParams) { p.SLATimeout = -1 * time.Second }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mutate(&p)
			err := validateCreateParams(p)
			if tc.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
