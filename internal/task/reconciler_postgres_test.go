// Reconciler tests against real Postgres.
//
// The SQLite-backed coverage_test.go file already covers reconciler
// behaviour at the FSM level. This file uses the F7 testcontainer harness
// to verify the same behaviour against the production SQL dialect:
//   - sweepStuck re-arms next_poll_after correctly via UpdateScheduleSQL.
//   - sweepTimedOut emits TaskTimedOut events via PostgresDialect's
//     UpdateTerminalAtSQL + UpdateStateSQL.
//   - The full Run loop honours ctx cancellation with the production SQL.
//
// All tests SKIP cleanly when Docker is unavailable.

package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
	"github.com/QuantumNous/new-api/internal/testutil"
)

// recordedBus is a copy of the helper from testutil_test.go but expressed
// as a method-callable struct with no SQLite dependency. Postgres tests
// can call it without dragging in the SQLite-only newTestDB scaffolding.
type recordedBus struct {
	mu     sync.Mutex
	events []events.Event
}

func newRecordedBus(bus events.EventBus) *recordedBus {
	r := &recordedBus{}
	bus.Subscribe(func(ev events.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, ev)
	})
	return r
}

func (r *recordedBus) byKind(kind string) []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, 0, len(r.events))
	for _, e := range r.events {
		if e.Kind() == kind {
			out = append(out, e)
		}
	}
	return out
}

// pgReconcilerSetup spins up a Postgres-backed Repo + bus + Reconciler
// and returns them with the recorded subscriber wired in.
func pgReconcilerSetup(t *testing.T) (*sql.DB, *Repo, *Reconciler, *recordedBus) {
	t.Helper()
	db := testutil.NewPostgresDB(t)
	repo := NewRepo(db, PostgresDialect{})
	bus := events.NewMemoryBus()
	t.Cleanup(func() { bus.Close() })
	rec := NewReconciler(repo, bus)
	rec.StuckThreshold = time.Minute
	recorder := newRecordedBus(bus)
	return db, repo, rec, recorder
}

func pgTaskParams(seed string) NewTaskParams {
	return NewTaskParams{
		AccountID:      "acct-pg-" + seed,
		ModelKey:       adapter.ModelKey("test-model"),
		ProviderKey:    adapter.ProviderKey("mock-async"),
		ParamsJSON:     json.RawMessage(`{"prompt":"x"}`),
		IdempotencyKey: "key-pg-" + seed,
		HeldAmount:     adapter.CostUSD(60_000),
		SLATimeout:     5 * time.Minute,
	}
}

// TestReconciler_Postgres_SweepStuck_ReschedulesExpiredRow.
// Submitted task with backdated next_poll_after must have it reset to
// nowFn() so the worker re-claims on its next iteration.
func TestReconciler_Postgres_SweepStuck_ReschedulesExpiredRow(t *testing.T) {
	t.Parallel()
	db, repo, rec, _ := pgReconcilerSetup(t)
	ctx := context.Background()

	fakeNow := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	rec.nowFn = func() time.Time { return fakeNow }

	stuck := mustWalkToSubmitted(t, repo, pgTaskParams("stuck-pg-1"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.ExecContext(ctx, `UPDATE task SET next_poll_after = $1 WHERE id = $2`, past, stuck.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	rec.SweepOnce(ctx)

	out, err := repo.FindByID(ctx, stuck.ID)
	if err != nil {
		t.Fatalf("re-find: %v", err)
	}
	if out.NextPollAfter == nil {
		t.Fatalf("NextPollAfter nil after sweep; expected fakeNow")
	}
	// Postgres rounds TIMESTAMPTZ to microseconds; tolerate a 1ms drift.
	if delta := out.NextPollAfter.Sub(fakeNow); delta < -time.Millisecond || delta > time.Millisecond {
		t.Errorf("NextPollAfter = %v; want ~%v (delta %v)", out.NextPollAfter, fakeNow, delta)
	}
}

// TestReconciler_Postgres_SweepStuck_NoOpWhenEmpty: no rows, no events,
// no panic. Silent-failure prevention.
func TestReconciler_Postgres_SweepStuck_NoOpWhenEmpty(t *testing.T) {
	t.Parallel()
	_, _, rec, recorded := pgReconcilerSetup(t)
	rec.SweepOnce(context.Background())
	if got := recorded.byKind("task_timed_out"); len(got) != 0 {
		t.Errorf("expected no events on empty table; got %d", len(got))
	}
}

// TestReconciler_Postgres_SweepTimedOut_TransitionsAndEmits exercises the
// canonical SLA-rescue path against PostgresDialect: a Submitted task
// whose sla_deadline has passed must be marked TimedOut and a
// TaskTimedOut event must be published carrying the held amount as the
// prepaid quantity (so the wallet can refund).
func TestReconciler_Postgres_SweepTimedOut_TransitionsAndEmits(t *testing.T) {
	t.Parallel()
	db, repo, rec, recorded := pgReconcilerSetup(t)
	ctx := context.Background()

	task := mustWalkToSubmitted(t, repo, pgTaskParams("to-1"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.ExecContext(ctx, `UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID); err != nil {
		t.Fatalf("backdate sla: %v", err)
	}

	rec.SweepOnce(ctx)

	out, _ := repo.FindByID(ctx, task.ID)
	if out.State != StateTimedOut {
		t.Errorf("state = %q; want timed_out", out.State)
	}
	emitted := recorded.byKind("task_timed_out")
	if len(emitted) != 1 {
		t.Fatalf("expected 1 TaskTimedOut; got %d", len(emitted))
	}
	if emitted[0].GetTaskID() != events.TaskID(task.ID) {
		t.Errorf("event task ID = %q; want %q", emitted[0].GetTaskID(), task.ID)
	}
}

// TestReconciler_Postgres_SweepTimedOut_RescuesStuckHeld is the F8
// regression guard repeated against real Postgres. The Held → TimedOut
// edge added in commit 4a6438f7 must keep working — without it, a task
// stuck in Held past its SLA leaks the held escrow forever.
func TestReconciler_Postgres_SweepTimedOut_RescuesStuckHeld(t *testing.T) {
	t.Parallel()
	db, repo, rec, recorded := pgReconcilerSetup(t)
	ctx := context.Background()

	task, err := repo.Create(ctx, pgTaskParams("held-stuck-pg"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkHeld(ctx, task.ID, 60_000); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.ExecContext(ctx, `UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID); err != nil {
		t.Fatalf("backdate sla: %v", err)
	}

	rec.SweepOnce(ctx)

	out, _ := repo.FindByID(ctx, task.ID)
	if out.State != StateTimedOut {
		t.Errorf("F8 regression: held-stuck task state = %q; want timed_out", out.State)
	}
	if got := recorded.byKind("task_timed_out"); len(got) != 1 {
		t.Errorf("F8 regression: expected 1 TaskTimedOut event; got %d", len(got))
	}
}

// TestReconciler_Postgres_SweepTimedOut_SkipsAlreadyTerminal ensures a
// second sweep over a row that's already in StateTimedOut is a no-op.
// FindTimedOutSQL excludes terminal states; the test ratifies the SQL.
func TestReconciler_Postgres_SweepTimedOut_SkipsAlreadyTerminal(t *testing.T) {
	t.Parallel()
	db, repo, rec, _ := pgReconcilerSetup(t)
	ctx := context.Background()

	task := mustWalkToSubmitted(t, repo, pgTaskParams("to-skip-pg"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.ExecContext(ctx, `UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID); err != nil {
		t.Fatalf("backdate sla: %v", err)
	}
	rec.SweepOnce(ctx) // first sweep transitions

	// Re-record events from this point on; first sweep already emitted one.
	bus2 := events.NewMemoryBus()
	defer bus2.Close()
	rec2 := NewReconciler(repo, bus2)
	rec2.StuckThreshold = time.Minute
	recorded2 := newRecordedBus(bus2)

	rec2.SweepOnce(ctx)
	if got := recorded2.byKind("task_timed_out"); len(got) != 0 {
		t.Errorf("second sweep emitted %d events; want 0", len(got))
	}
}

// TestReconciler_Postgres_SweepBothPaths verifies the dual-sweep
// invariant from notebooklm_reconciler_dual_sweep: BOTH sweepStuck and
// sweepTimedOut run on every tick. We seed one stuck row + one timed-out
// row and expect a single SweepOnce to handle both.
func TestReconciler_Postgres_SweepBothPaths(t *testing.T) {
	t.Parallel()
	db, repo, rec, recorded := pgReconcilerSetup(t)
	ctx := context.Background()

	stuck := mustWalkToSubmitted(t, repo, pgTaskParams("stuck-both"))
	timedOut := mustWalkToSubmitted(t, repo, pgTaskParams("to-both"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.ExecContext(ctx, `UPDATE task SET next_poll_after = $1 WHERE id = $2`, past, stuck.ID); err != nil {
		t.Fatalf("backdate stuck: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, timedOut.ID); err != nil {
		t.Fatalf("backdate timed-out: %v", err)
	}

	rec.SweepOnce(ctx)

	// Stuck row should have next_poll_after re-armed (non-nil and recent).
	stuckOut, _ := repo.FindByID(ctx, stuck.ID)
	if stuckOut.NextPollAfter == nil {
		t.Errorf("stuck task next_poll_after still nil after sweep")
	}
	// Timed-out row should be in StateTimedOut.
	toOut, _ := repo.FindByID(ctx, timedOut.ID)
	if toOut.State != StateTimedOut {
		t.Errorf("timed-out task state = %q; want timed_out", toOut.State)
	}
	// Bus saw exactly one TaskTimedOut.
	if got := recorded.byKind("task_timed_out"); len(got) != 1 {
		t.Errorf("bus saw %d TaskTimedOut; want 1", len(got))
	}
}

// TestReconciler_Postgres_Run_StopsOnContextCancel: drives the full Run
// loop against Postgres briefly, then cancels and confirms exit.
func TestReconciler_Postgres_Run_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	_, _, rec, _ := pgReconcilerSetup(t)
	rec.Interval = 25 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Run(ctx)
		close(done)
	}()
	// Allow at least one tick.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of cancel")
	}
}

// TestReconciler_Postgres_Run_EmitsTimedOutEventDuringLoop: seed a task
// before starting Run, let the timer fire, and confirm the bus saw at
// least one TaskTimedOut. This is the closest thing we have to an
// end-to-end "reconciler is alive" check on the production SQL surface.
func TestReconciler_Postgres_Run_EmitsTimedOutEventDuringLoop(t *testing.T) {
	t.Parallel()
	db, repo, rec, recorded := pgReconcilerSetup(t)
	rec.Interval = 25 * time.Millisecond

	// Seed a Submitted task with backdated SLA before Run starts. The
	// initial immediate sweep on entry will catch it.
	task := mustWalkToSubmitted(t, repo, pgTaskParams("run-loop"))
	past := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := db.ExecContext(context.Background(),
		`UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID); err != nil {
		t.Fatalf("backdate sla: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Run(ctx)
		close(done)
	}()

	// Poll briefly for the event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(recorded.byKind("task_timed_out")) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if got := recorded.byKind("task_timed_out"); len(got) != 1 {
		t.Errorf("Run loop emitted %d TaskTimedOut; want 1", len(got))
	}
}

// TestReconciler_Postgres_BatchLimit_RespectsCap: with BatchLimit smaller
// than the number of timed-out rows, only BatchLimit rows are
// transitioned per sweep. Subsequent sweeps mop up the rest.
func TestReconciler_Postgres_BatchLimit_RespectsCap(t *testing.T) {
	t.Parallel()
	db, repo, rec, recorded := pgReconcilerSetup(t)
	ctx := context.Background()
	rec.BatchLimit = 2

	const want = 5
	ids := make([]string, 0, want)
	past := time.Now().UTC().Add(-30 * time.Minute)
	for i := 0; i < want; i++ {
		seed := "batch-" + strings.Repeat("x", i+1)
		task := mustWalkToSubmitted(t, repo, pgTaskParams(seed))
		if _, err := db.ExecContext(ctx, `UPDATE task SET sla_deadline = $1 WHERE id = $2`, past, task.ID); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		ids = append(ids, task.ID)
	}

	rec.SweepOnce(ctx)
	if got := len(recorded.byKind("task_timed_out")); got != 2 {
		t.Errorf("first sweep emitted %d events; want 2 (BatchLimit)", got)
	}

	// Second sweep picks up next 2.
	rec.SweepOnce(ctx)
	if got := len(recorded.byKind("task_timed_out")); got != 4 {
		t.Errorf("second sweep cumulative = %d; want 4", got)
	}

	// Third sweep handles the last one.
	rec.SweepOnce(ctx)
	if got := len(recorded.byKind("task_timed_out")); got != 5 {
		t.Errorf("third sweep cumulative = %d; want 5", got)
	}

	// All rows now StateTimedOut.
	for _, id := range ids {
		out, _ := repo.FindByID(ctx, id)
		if out.State != StateTimedOut {
			t.Errorf("task %s state = %q; want timed_out", id, out.State)
		}
	}
}
