// Wallet ←→ EventBus subscription per ADR-011.
//
// Wallet does NOT import internal/task. The coupling lives entirely in
// internal/events: the worker (S5) publishes; the wallet subscribes here.
//
// Event mapping:
//
//   AssetHosted    → Settle(escrow, actualCost)            // happy path
//   AssetLost      → PartialRefund(escrow, fraction)       // partial fail
//   TaskFailed     → Refund(escrow)                        // full fail
//   TaskTimedOut   → Refund(escrow) | partial if prepaid   // SLA breach
//   TaskCancelled  → Refund(escrow)                        // user/admin cancel
//
// TaskHeld / TaskSubmitted / TaskRunning / TaskSucceeded / OutputAvailable
// are NOT consumed by the wallet — Hold has already fired before TaskHeld;
// Settle waits for AssetHosted (per ADR-010, never directly on TaskSucceeded
// because the asset isn't on our CDN yet).

package wallet

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
)

// AssetCostFractionFunc resolves the asset-vs-compute split for a task.
// Returns a value in [0,1]; 0 means all compute (full revenue, no refund),
// 1 means all asset (full refund, no revenue).
//
// The catalog package owns the manifest; the wallet doesn't import it. The
// caller wires this in at Subscribe time.
type AssetCostFractionFunc func(ctx context.Context, taskID string) (float64, error)

// Subscriber wires a Wallet to an EventBus.
//
// State: holds a small in-memory map of (taskID → ActualCost) populated by
// TaskSucceeded events. AssetHosted later reads this to call Settle with
// the right amount. The map is bounded by how many tasks are between
// TaskSucceeded and AssetHosted at any instant — typically seconds, so
// the map stays small. For multi-instance deployments this needs to be
// promoted to a shared store (Redis / DB).
type Subscriber struct {
	w                 Wallet
	bus               events.EventBus
	assetCostFraction AssetCostFractionFunc
	unsubscribe       events.Unsubscribe

	costMu      sync.Mutex
	pendingCost map[string]adapter.CostUSD

	// errLogger receives non-fatal subscriber errors. Default: stdlib log.
	errLogger func(format string, args ...any)
}

// NewSubscriber constructs a Subscriber. fractionFn is invoked on AssetLost
// to decide the compute/asset split. If nil, defaults to 0.5.
func NewSubscriber(w Wallet, bus events.EventBus, fractionFn AssetCostFractionFunc) *Subscriber {
	if w == nil {
		panic("wallet: NewSubscriber requires non-nil wallet")
	}
	if bus == nil {
		panic("wallet: NewSubscriber requires non-nil bus")
	}
	if fractionFn == nil {
		fractionFn = func(_ context.Context, _ string) (float64, error) { return 0.5, nil }
	}
	return &Subscriber{
		w:                 w,
		bus:               bus,
		assetCostFraction: fractionFn,
		pendingCost:       make(map[string]adapter.CostUSD),
		errLogger:         func(format string, args ...any) { log.Printf("wallet: "+format, args...) },
	}
}

// SetErrLogger overrides the default stdlib log.Printf.
func (s *Subscriber) SetErrLogger(fn func(format string, args ...any)) {
	if fn != nil {
		s.errLogger = fn
	}
}

// Start subscribes to the bus. Returns an Unsubscribe func that the caller
// MUST invoke on shutdown (typically via defer in main()).
func (s *Subscriber) Start(ctx context.Context) events.Unsubscribe {
	s.unsubscribe = s.bus.Subscribe(func(ev events.Event) {
		s.handle(ctx, ev)
	})
	return s.unsubscribe
}

// handle dispatches one event. Errors are logged but not propagated — a
// subscriber failure must NOT block the bus or the publisher.
func (s *Subscriber) handle(ctx context.Context, ev events.Event) {
	switch e := ev.(type) {
	case events.TaskSucceeded:
		// Cache ActualCost so AssetHosted (which doesn't carry cost) can use it.
		s.costMu.Lock()
		s.pendingCost[string(e.GetTaskID())] = adapter.CostUSD(e.ActualCost)
		s.costMu.Unlock()
	case events.AssetHosted:
		s.onAssetHosted(ctx, e)
	case events.AssetLost:
		s.onAssetLost(ctx, e)
	case events.TaskFailed:
		s.onTaskTerminal(ctx, string(e.GetTaskID()), "TaskFailed")
	case events.TaskTimedOut:
		s.onTaskTerminal(ctx, string(e.GetTaskID()), "TaskTimedOut")
	case events.TaskCancelled:
		s.onTaskTerminal(ctx, string(e.GetTaskID()), "TaskCancelled")
	default:
		// Other events (TaskHeld/Submitted/Running/OutputAvailable) are not
		// the wallet's concern. Silent ignore.
	}
}

func (s *Subscriber) onAssetHosted(ctx context.Context, e events.AssetHosted) {
	taskID := string(e.GetTaskID())
	escrow := EscrowID(taskID)

	// Look up cached ActualCost from the prior TaskSucceeded.
	s.costMu.Lock()
	actual, ok := s.pendingCost[taskID]
	if ok {
		delete(s.pendingCost, taskID)
	}
	s.costMu.Unlock()

	if !ok {
		// AssetHosted without prior TaskSucceeded — only happens if events
		// were dropped or re-delivered out of order. We don't know the
		// actual cost, so refund the user instead of guessing zero.
		//
		// F9 fix: previously called Settle(escrow, 0) here. That violated
		// the wallet_ledger CHECK(amount<>0) constraint at the drain row
		// for any *funded* escrow — the only reason it didn't surface in
		// production was that the escrow was almost always already drained
		// (replay) so Settle returned ErrEscrowAlreadySettled first.
		s.errLogger("AssetHosted without prior TaskSucceeded for task=%s; refunding", taskID)
		if err := s.w.Refund(ctx, escrow); err != nil {
			if errors.Is(err, ErrEscrowAlreadySettled) {
				return // benign: escrow was already drained by a prior Settle/Refund
			}
			s.errLogger("AssetHosted fallback-Refund failed task=%s: %v", taskID, err)
		}
		return
	}

	if err := s.w.Settle(ctx, escrow, actual); err != nil {
		if errors.Is(err, ErrEscrowAlreadySettled) {
			return // idempotent replay; benign
		}
		s.errLogger("AssetHosted Settle failed task=%s: %v", taskID, err)
	}
}

func (s *Subscriber) onAssetLost(ctx context.Context, e events.AssetLost) {
	taskID := string(e.GetTaskID())
	escrow := EscrowID(taskID)
	fraction, err := s.assetCostFraction(ctx, taskID)
	if err != nil {
		s.errLogger("AssetLost fractionFn failed task=%s: %v (defaulting to 0.5)", taskID, err)
		fraction = 0.5
	}
	if err := s.w.PartialRefund(ctx, escrow, fraction); err != nil {
		if errors.Is(err, ErrEscrowAlreadySettled) {
			return
		}
		s.errLogger("AssetLost PartialRefund failed task=%s: %v", taskID, err)
	}
}

// onTaskTerminal handles Failed/TimedOut/Cancelled — all do a full refund.
// Caller passes the kind string for log clarity.
func (s *Subscriber) onTaskTerminal(ctx context.Context, taskID, kind string) {
	if err := s.w.Refund(ctx, EscrowID(taskID)); err != nil {
		if errors.Is(err, ErrEscrowAlreadySettled) {
			return
		}
		s.errLogger("%s Refund failed task=%s: %v", kind, taskID, err)
	}
}

// Stop unsubscribes from the bus. Safe to call multiple times.
func (s *Subscriber) Stop() {
	if s.unsubscribe == nil {
		return
	}
	s.unsubscribe()
	s.unsubscribe = nil
}
