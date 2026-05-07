// Reconciler — the safety net.
//
// The Worker is the happy path: claim → poll → advance. The reconciler
// catches everything that falls through:
//
//  1. sweepStuck: tasks whose next_poll_after expired more than N
//     minutes ago without being claimed (worker crash, infinite-backoff
//     bug, DB issue). Reset next_poll_after to NULL so the worker
//     re-claims on its next iteration.
//
//  2. sweepTimedOut: tasks past sla_deadline still in non-terminal
//     state. Transition to StateTimedOut and emit TaskTimedOut so the
//     wallet refunds.
//
// Per the notebooklm_reconciler_dual_sweep memory entry referenced in
// S5-WORKER-DESIGN.md §6: BOTH sweeps run on every tick. Skipping one
// leaves a class of zombies orphaned forever (worker waiting for poll
// schedule that already triggered, while task waits for SLA escalation
// that never fires).
//
// Cadence: 1-minute timer. Each tick sweeps both sets sequentially.
// Returns when ctx is cancelled.

package task

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/internal/events"
)

// Default reconciler tuning.
const (
	// DefaultStuckThreshold is how far past next_poll_after we wait
	// before considering a task "stuck". Per S5 §6.
	DefaultStuckThreshold = 5 * time.Minute

	// DefaultReconcileInterval is the timer period.
	DefaultReconcileInterval = 1 * time.Minute

	// DefaultBatchLimit caps how many rows each sweep handles per tick
	// — keeps any single sweep cheap.
	DefaultBatchLimit = 100
)

// Reconciler runs periodic safety-net sweeps over the task table.
type Reconciler struct {
	Repo *Repo
	Bus  events.EventBus

	// StuckThreshold is the grace period past next_poll_after.
	StuckThreshold time.Duration

	// Interval is the tick period.
	Interval time.Duration

	// BatchLimit caps rows per sweep.
	BatchLimit int

	// nowFn is overridable for tests; defaults to time.Now().UTC().
	nowFn func() time.Time
}

// NewReconciler constructs a Reconciler with default tuning.
func NewReconciler(repo *Repo, bus events.EventBus) *Reconciler {
	if repo == nil {
		panic("task: NewReconciler requires non-nil repo")
	}
	if bus == nil {
		panic("task: NewReconciler requires non-nil bus")
	}
	return &Reconciler{
		Repo:           repo,
		Bus:            bus,
		StuckThreshold: DefaultStuckThreshold,
		Interval:       DefaultReconcileInterval,
		BatchLimit:     DefaultBatchLimit,
		nowFn:          func() time.Time { return time.Now().UTC() },
	}
}

// Run loops until ctx is cancelled. Each tick sweeps both sets.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	// Run an immediate sweep on entry so a freshly-restarted reconciler
	// catches accumulated debt without waiting a full interval.
	r.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.sweepOnce(ctx)
		}
	}
}

// sweepOnce performs one stuck + one timed-out sweep. Public so tests
// can drive a single iteration without spinning up a ticker.
func (r *Reconciler) SweepOnce(ctx context.Context) {
	r.sweepOnce(ctx)
}

func (r *Reconciler) sweepOnce(ctx context.Context) {
	r.sweepStuck(ctx)
	r.sweepTimedOut(ctx)
}

// sweepStuck finds tasks whose next_poll_after expired more than
// StuckThreshold ago and clears the field so the worker can re-claim.
//
// We do NOT change state here — we just nudge the schedule. The actual
// FSM advance happens via the worker on the next iteration.
func (r *Reconciler) sweepStuck(ctx context.Context) {
	limit := r.BatchLimit
	if limit <= 0 {
		limit = DefaultBatchLimit
	}
	tasks, err := r.Repo.FindStuck(ctx, r.StuckThreshold, limit)
	if err != nil {
		return
	}
	for _, t := range tasks {
		// Re-arm by setting next_poll_after to "now" with attempt
		// preserved. Worker's claim query treats next_poll_after <= now
		// as ready.
		_ = r.Repo.SchedulePoll(ctx, t.ID, r.nowFn(), t.PollAttempt)
	}
}

// sweepTimedOut finds tasks past sla_deadline still in non-terminal
// state and transitions them to StateTimedOut. Emits TaskTimedOut so
// the wallet refunds.
func (r *Reconciler) sweepTimedOut(ctx context.Context) {
	limit := r.BatchLimit
	if limit <= 0 {
		limit = DefaultBatchLimit
	}
	tasks, err := r.Repo.FindTimedOut(ctx, limit)
	if err != nil {
		return
	}
	for _, t := range tasks {
		err := r.Repo.MarkTimedOut(ctx, t.ID, "sla_deadline_exceeded")
		if err != nil {
			if isAlreadyTerminal(err) {
				continue
			}
			// Other error — skip this row, try again next tick.
			continue
		}
		_ = r.Bus.Publish(events.MakeTaskTimedOut(
			events.NewBaseEvent(events.TaskID(t.ID)),
			events.CostUSD(t.HeldAmount),
		))
	}
}
