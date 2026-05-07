// Worker — pulls tasks from the queue, calls adapter.Poll, advances FSM.
//
// Architecture: one Worker instance per (provider, goroutine). Multiple
// goroutines may share a Worker safely. Each iteration:
//
//  1. Wait for the per-provider rate limit budget (golang.org/x/time/rate)
//  2. ClaimNextTask via SKIP LOCKED — bumps poll_attempt, clears next_poll_after
//  3. If a task was claimed, advance(): adapter.Poll → state transition
//  4. If no task, sleep with light jitter (NOT a fixed sleep)
//
// AP-3 guard: every Poll → still-pending result reschedules via
// NextPollAfter(attempt). NO time.Sleep on the result-pending path —
// the sleep is only the "no work to do" idle ping.
//
// FSM advancement maps adapter.PollStatus to internal/task.TaskState:
//
//	PollPending   → stay Submitted/Running (whichever matches), reschedule
//	PollRunning   → ensure Running, reschedule
//	PollSucceeded → Succeeded, emit TaskSucceeded
//	PollFailed    → Failed (or TimedOut for ErrClassTimeout), emit corresponding event
//
// Event emission is decoupled via the EventBus parameter. Wallet (S6)
// subscribes from outside this package — S5 never imports S6.

package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"golang.org/x/time/rate"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
)

// Worker drives one provider's queue forward. Safe for use by multiple
// goroutines concurrently — each iteration claims its own task via
// SKIP LOCKED.
type Worker struct {
	ProviderKey adapter.ProviderKey
	Adapter     adapter.ProviderAdapter
	Repo        *Repo
	Bus         events.EventBus
	RateLimiter *rate.Limiter

	// IdleSleepMax is the upper bound on the "no work to do" idle delay.
	// Default 2 s. A small randomized component is added on top.
	IdleSleepMax time.Duration

	// pollExecutor is an injection point for tests — it lets a test
	// pre-empt adapter.Poll without re-implementing the worker loop.
	// nil = use real adapter.Poll.
	pollExecutor func(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) (adapter.PollResult, error)
}

// NewWorker constructs a Worker. RateLimiter may be nil (no rate limit).
func NewWorker(providerKey adapter.ProviderKey, adp adapter.ProviderAdapter, repo *Repo, bus events.EventBus, limiter *rate.Limiter) *Worker {
	if adp == nil {
		panic("task: NewWorker requires non-nil adapter")
	}
	if repo == nil {
		panic("task: NewWorker requires non-nil repo")
	}
	if bus == nil {
		panic("task: NewWorker requires non-nil bus")
	}
	return &Worker{
		ProviderKey:  providerKey,
		Adapter:      adp,
		Repo:         repo,
		Bus:          bus,
		RateLimiter:  limiter,
		IdleSleepMax: 2 * time.Second,
	}
}

// Run loops until ctx is cancelled. Each iteration waits for the rate
// limiter, claims one task, and advances it. Returns when ctx is done.
func (w *Worker) Run(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if w.RateLimiter != nil {
			if err := w.RateLimiter.Wait(ctx); err != nil {
				return
			}
		}
		t, err := w.Repo.ClaimNextTask(ctx, w.ProviderKey)
		if err != nil {
			// DB error — back off briefly to avoid hammering.
			if !w.idleSleep(ctx) {
				return
			}
			continue
		}
		if t == nil {
			if !w.idleSleep(ctx) {
				return
			}
			continue
		}
		// advance is best-effort — log+continue on error rather than
		// killing the worker.
		_ = w.advance(ctx, t)
	}
}

// AdvanceOne is the non-loop test entry point: claim one task and
// advance it (if any). Returns whether a task was processed.
func (w *Worker) AdvanceOne(ctx context.Context) (bool, error) {
	t, err := w.Repo.ClaimNextTask(ctx, w.ProviderKey)
	if err != nil {
		return false, err
	}
	if t == nil {
		return false, nil
	}
	if err := w.advance(ctx, t); err != nil {
		return true, err
	}
	return true, nil
}

// advance polls the upstream provider for t and applies the result to
// the FSM.
func (w *Worker) advance(ctx context.Context, t *Task) error {
	if t.UpstreamRef == "" {
		// Submit was never called for this task (programmer error).
		// Skip; reconciler will catch it via SLA timeout.
		return fmt.Errorf("worker: task %s has empty upstream_ref", t.ID)
	}
	pr, err := w.poll(ctx, t.ModelKey, adapter.UpstreamRef(t.UpstreamRef))
	if err != nil {
		// Treat poll-time errors as upstream errors. The next iteration
		// will retry naturally; meanwhile reschedule.
		_ = w.reschedule(ctx, t)
		return err
	}
	switch pr.Status {
	case adapter.PollPending:
		// Stay in current state; just push next_poll_after out.
		return w.reschedule(ctx, t)
	case adapter.PollRunning:
		if t.State == StateSubmitted {
			err := w.Repo.MarkRunning(ctx, t.ID)
			if err == nil {
				_ = w.Bus.Publish(events.MakeTaskRunning(events.NewBaseEvent(events.TaskID(t.ID)), pr.Progress))
			} else if !isAlreadyTerminal(err) {
				return err
			}
		}
		return w.reschedule(ctx, t)
	case adapter.PollSucceeded:
		return w.handleSuccess(ctx, t, pr)
	case adapter.PollFailed:
		return w.handleFailure(ctx, t, pr)
	default:
		return fmt.Errorf("worker: unknown poll status %q for task %s", pr.Status, t.ID)
	}
}

// reschedule sets next_poll_after using exponential backoff with jitter.
// Webhook-capable adapters poll at the slow end of the curve.
func (w *Worker) reschedule(ctx context.Context, t *Task) error {
	caps := w.Adapter.Capabilities(t.ModelKey)
	var next time.Time
	if caps.SupportsWebhook {
		next = SlowPollNext()
	} else {
		next = NextPollAfter(t.PollAttempt)
	}
	return w.Repo.SchedulePoll(ctx, t.ID, next, t.PollAttempt)
}

// handleSuccess advances the FSM to Succeeded and emits the
// TaskSucceeded event so the wallet (S6) and asset worker (S9.5) can
// react.
func (w *Worker) handleSuccess(ctx context.Context, t *Task, pr adapter.PollResult) error {
	// HeldAmount is what we held; ActualCost is what we'll bill.
	// EstimateCost is the floor here for now — adapters that report a
	// real cost in metadata can override later (S9.5/wallet refinement).
	actual := t.HeldAmount
	if c, err := w.Adapter.EstimateCost(t.ModelKey, paramsFromJSON(t.ParamsJSON)); err == nil && c > 0 {
		actual = c
	}
	if err := w.Repo.MarkSucceeded(ctx, t.ID, actual); err != nil {
		if isAlreadyTerminal(err) {
			return nil
		}
		return err
	}
	_ = w.Bus.Publish(events.MakeTaskSucceeded(events.NewBaseEvent(events.TaskID(t.ID)), events.CostUSD(actual)))
	if pr.Result != nil && len(pr.Result.Outputs) > 0 {
		first := pr.Result.Outputs[0]
		_ = w.Bus.Publish(events.MakeOutputAvailable(events.NewBaseEvent(events.TaskID(t.ID)),
			first.URL, first.MimeType, first.SizeBytes))
	}
	return nil
}

// handleFailure routes timeout vs other failures to the correct FSM
// transition + event variant.
func (w *Worker) handleFailure(ctx context.Context, t *Task, pr adapter.PollResult) error {
	class := adapter.ErrClassUnknown
	msg := "upstream reported failure"
	var raw []byte
	if pr.Error != nil {
		if pr.Error.Class != "" {
			class = pr.Error.Class
		}
		if pr.Error.Message != "" {
			msg = pr.Error.Message
		}
		raw = pr.Error.Raw
	}
	if class == adapter.ErrClassTimeout {
		if err := w.Repo.MarkTimedOut(ctx, t.ID, msg); err != nil {
			if isAlreadyTerminal(err) {
				return nil
			}
			return err
		}
		_ = w.Bus.Publish(events.MakeTaskTimedOut(events.NewBaseEvent(events.TaskID(t.ID)),
			events.CostUSD(t.HeldAmount)))
		return nil
	}
	if err := w.Repo.MarkFailed(ctx, t.ID, class, msg, raw); err != nil {
		if isAlreadyTerminal(err) {
			return nil
		}
		return err
	}
	_ = w.Bus.Publish(events.MakeTaskFailed(events.NewBaseEvent(events.TaskID(t.ID)),
		events.ErrorClass(class), msg))
	return nil
}

// idleSleep waits a short, jittered duration before the next iteration.
// Returns false when ctx was cancelled during the sleep.
//
// This is the ONLY sleep on the worker hot path — and it is bounded
// short (under 2.5s typical). It's used purely to avoid spinning when
// the queue is empty; it does NOT affect the AP-3 backoff curve, which
// is computed via DB persistence in NextPollAfter.
func (w *Worker) idleSleep(ctx context.Context) bool {
	maxD := w.IdleSleepMax
	if maxD <= 0 {
		maxD = 2 * time.Second
	}
	// Floor 250ms; ceiling maxD.
	d := time.Duration(rand.Float64()*float64(maxD-250*time.Millisecond)) + 250*time.Millisecond
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// poll dispatches to the configured pollExecutor or falls back to
// adapter.Poll.
func (w *Worker) poll(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) (adapter.PollResult, error) {
	if w.pollExecutor != nil {
		return w.pollExecutor(ctx, model, ref)
	}
	return w.Adapter.Poll(ctx, model, ref)
}

// SetPollExecutor is a test-only injection point.
func (w *Worker) SetPollExecutor(fn func(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) (adapter.PollResult, error)) {
	w.pollExecutor = fn
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// isAlreadyTerminal returns true when err is one of the "task already
// advanced past us" categories — used by the worker to make every
// state-mutation idempotent.
func isAlreadyTerminal(err error) bool {
	if errors.Is(err, ErrTerminalState) {
		return true
	}
	var illegal *ErrIllegalTransition
	return errors.As(err, &illegal)
}

// paramsFromJSON best-effort parses adapter.Params from a JSON blob.
// Returns nil on parse failure — callers fall back to held amount.
func paramsFromJSON(raw []byte) adapter.Params {
	if len(raw) == 0 {
		return nil
	}
	var out adapter.Params
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
