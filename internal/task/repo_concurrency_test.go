// Concurrency-focused Postgres-backed tests.
//
// These tests exercise paths that can't be reached on SQLite because the
// pure-Go modernc.org/sqlite driver serialises writers — there is no real
// row-level locking, no SKIP LOCKED, and no SERIALIZATION FAILURE.
//
// Coverage targets:
//   - ClaimNextTask under concurrent load (SKIP LOCKED).
//   - Repo.transition retry path on real Postgres lock contention (the
//     transactional select-for-update + update sequence).
//
// All tests SKIP cleanly when Docker is unavailable.

package task

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// TestRepo_Postgres_ClaimNextTask_NoDoubleClaim seeds N tasks and runs
// 2 * N concurrent claim goroutines. The 2N races to claim N rows must
// never produce a duplicate claim — every claimed task ID must be
// returned exactly once, and the surplus goroutines must each get
// (nil, nil).
func TestRepo_Postgres_ClaimNextTask_NoDoubleClaim(t *testing.T) {
	t.Parallel()
	_, repo := newPostgresRepo(t)
	ctx := context.Background()

	const provider = adapter.ProviderKey("mock-async")
	const numTasks = 8
	const numWorkers = 16

	wantIDs := make(map[string]bool, numTasks)
	for i := 0; i < numTasks; i++ {
		seed := "concur-" + string(rune('a'+i))
		p := pgTaskParams(seed)
		p.ProviderKey = provider
		task := mustWalkToSubmitted(t, repo, p)
		wantIDs[task.ID] = true
	}

	type claimRes struct {
		task *Task
		err  error
	}
	results := make(chan claimRes, numWorkers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tt, err := repo.ClaimNextTask(ctx, provider)
			results <- claimRes{tt, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	gotIDs := map[string]int{}
	nilCount := 0
	for r := range results {
		if r.err != nil {
			t.Errorf("ClaimNextTask err: %v", r.err)
			continue
		}
		if r.task == nil {
			nilCount++
			continue
		}
		gotIDs[r.task.ID]++
	}

	// Every seeded task must have been claimed exactly once.
	for id := range wantIDs {
		if gotIDs[id] != 1 {
			t.Errorf("task %s claimed %d times; want 1", id, gotIDs[id])
		}
	}
	// No surprise IDs.
	for id := range gotIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected claimed ID %s", id)
		}
	}
	// Surplus workers got (nil, nil).
	if want := numWorkers - numTasks; nilCount != want {
		t.Errorf("nil claims = %d; want %d (surplus workers)", nilCount, want)
	}
}

// TestRepo_Postgres_ConcurrentTransition_OnlyOneWins drives two
// goroutines racing to call MarkSucceeded on the same task. Exactly one
// must succeed; the other must observe the row in a terminal state and
// return ErrTerminalState (or an illegal-transition error). This
// exercises the LockTaskByIDSQL FOR UPDATE pathway under real
// contention — paths the SQLite scaffolding can't trigger.
func TestRepo_Postgres_ConcurrentTransition_OnlyOneWins(t *testing.T) {
	t.Parallel()
	_, repo := newPostgresRepo(t)
	ctx := context.Background()

	task := mustWalkToSubmitted(t, repo, pgTaskParams("race-1"))

	type result struct {
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := repo.MarkSucceeded(ctx, task.ID, adapter.CostUSD(50_000))
			results <- result{err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var nilErrs, terminalErrs, otherErrs int
	for r := range results {
		switch {
		case r.err == nil:
			nilErrs++
		case isAlreadyTerminal(r.err):
			terminalErrs++
		default:
			otherErrs++
			t.Logf("unexpected error: %v", r.err)
		}
	}
	if nilErrs != 1 {
		t.Errorf("expected exactly 1 winning transition; got %d", nilErrs)
	}
	if terminalErrs != 1 {
		t.Errorf("expected exactly 1 terminal-state observation; got %d", terminalErrs)
	}
	if otherErrs > 0 {
		t.Errorf("got %d other errors", otherErrs)
	}

	// Final state must be Succeeded.
	out, _ := repo.FindByID(ctx, task.ID)
	if out.State != StateSucceeded {
		t.Errorf("final state = %q; want succeeded", out.State)
	}
}

// TestRepo_Postgres_TransitionAfterDelete_ReturnsNotFound: if the row is
// gone before LockTaskByIDSQL runs, transition must return
// ErrTaskNotFound — exercises the sql.ErrNoRows branch in transitionOnce.
func TestRepo_Postgres_TransitionAfterDelete_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	db, repo := newPostgresRepo(t)
	ctx := context.Background()

	task, err := repo.Create(ctx, pgTaskParams("del-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Delete events first to satisfy FK, then the row.
	if _, err := db.ExecContext(ctx, `DELETE FROM task_event WHERE task_id = $1`, task.ID); err != nil {
		t.Fatalf("delete events: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM task WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := repo.MarkHeld(ctx, task.ID, 1000); err != ErrTaskNotFound {
		t.Errorf("MarkHeld on deleted row = %v; want ErrTaskNotFound", err)
	}
}

// TestRepo_Postgres_ClaimNextTask_RespectsProviderKey: a task seeded for
// provider "p1" must NOT be claimable by a worker for provider "p2".
func TestRepo_Postgres_ClaimNextTask_RespectsProviderKey(t *testing.T) {
	t.Parallel()
	_, repo := newPostgresRepo(t)
	ctx := context.Background()

	p1 := pgTaskParams("provider-isolation")
	p1.ProviderKey = adapter.ProviderKey("provider-1")
	mustWalkToSubmitted(t, repo, p1)

	// Wrong provider sees nothing.
	got, err := repo.ClaimNextTask(ctx, adapter.ProviderKey("provider-2"))
	if err != nil {
		t.Fatalf("ClaimNextTask wrong provider: %v", err)
	}
	if got != nil {
		t.Errorf("provider-2 claimed task %s; want nil", got.ID)
	}
	// Right provider claims it.
	got, err = repo.ClaimNextTask(ctx, adapter.ProviderKey("provider-1"))
	if err != nil || got == nil {
		t.Fatalf("provider-1 claim = %v / %v; want a task", got, err)
	}
}

// TestRepo_Postgres_ClaimNextTask_RespectsNextPollAfter: a task with
// next_poll_after in the future must NOT be claimable until that time
// has passed.
func TestRepo_Postgres_ClaimNextTask_RespectsNextPollAfter(t *testing.T) {
	t.Parallel()
	db, repo := newPostgresRepo(t)
	ctx := context.Background()

	const provider = adapter.ProviderKey("mock-async-future")
	p := pgTaskParams("future-claim")
	p.ProviderKey = provider
	task := mustWalkToSubmitted(t, repo, p)

	future := time.Now().UTC().Add(1 * time.Hour)
	if _, err := db.ExecContext(ctx,
		`UPDATE task SET next_poll_after = $1 WHERE id = $2`, future, task.ID); err != nil {
		t.Fatalf("set next_poll_after: %v", err)
	}

	got, err := repo.ClaimNextTask(ctx, provider)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if got != nil {
		t.Errorf("future-scheduled task incorrectly claimed: %v", got.ID)
	}
}

// TestRepo_Postgres_Idempotency_Replay: Create with the same
// idempotency_key twice returns ErrIdempotentReplay on the second call,
// with the existing task. Exercises the conflict-detect-by-re-query
// path in Repo.Create against real Postgres.
func TestRepo_Postgres_Idempotency_Replay(t *testing.T) {
	t.Parallel()
	_, repo := newPostgresRepo(t)
	ctx := context.Background()

	p := pgTaskParams("idem-replay")
	first, err := repo.Create(ctx, p)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := repo.Create(ctx, p)
	if err != ErrIdempotentReplay {
		t.Errorf("second Create err = %v; want ErrIdempotentReplay", err)
	}
	if second == nil || second.ID != first.ID {
		t.Errorf("idempotent replay returned %v; want existing %s", second, first.ID)
	}
}
