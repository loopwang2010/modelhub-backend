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
	"time"

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
// State: a PendingCostStore stages (taskID → ActualCost) populated by
// TaskSucceeded events. AssetHosted later reads this to call Settle with
// the right amount. The store implementation is injected — InMemory for
// single-instance dev, Redis for multi-replica production. See
// pendingstore.go for the contract.
type Subscriber struct {
	w                 Wallet
	bus               events.EventBus
	assetCostFraction AssetCostFractionFunc
	unsubscribe       events.Unsubscribe

	// pendingCost stages cost between TaskSucceeded and AssetHosted.
	// Always non-nil; defaults to InMemoryPendingStore via NewSubscriber.
	pendingCost PendingCostStore

	// pendingTTL controls how long staged costs survive before being
	// auto-evicted. Defaults to DefaultPendingTTL.
	pendingTTL time.Duration

	// errLogger receives non-fatal subscriber errors. Default: stdlib log.
	errLogger func(format string, args ...any)
}

// NewSubscriber constructs a Subscriber backed by an in-memory pending-cost
// store (preserves the original single-instance semantics). For multi-
// instance deployments use NewSubscriberWithStore with a Redis-backed
// PendingCostStore.
//
// fractionFn is invoked on AssetLost to decide the compute/asset split.
// If nil, defaults to 0.5.
func NewSubscriber(w Wallet, bus events.EventBus, fractionFn AssetCostFractionFunc) *Subscriber {
	return NewSubscriberWithStore(w, bus, fractionFn, NewInMemoryPendingStore())
}

// NewSubscriberWithStore is the injection-friendly constructor used in
// production wiring (where the pending-cost store may be Redis-backed)
// and in tests (where mocks may be substituted). store MUST be non-nil.
func NewSubscriberWithStore(w Wallet, bus events.EventBus, fractionFn AssetCostFractionFunc, store PendingCostStore) *Subscriber {
	if w == nil {
		panic("wallet: NewSubscriber requires non-nil wallet")
	}
	if bus == nil {
		panic("wallet: NewSubscriber requires non-nil bus")
	}
	if store == nil {
		panic("wallet: NewSubscriberWithStore requires non-nil store")
	}
	if fractionFn == nil {
		fractionFn = func(_ context.Context, _ string) (float64, error) { return 0.5, nil }
	}
	return &Subscriber{
		w:                 w,
		bus:               bus,
		assetCostFraction: fractionFn,
		pendingCost:       store,
		pendingTTL:        DefaultPendingTTL,
		errLogger:         func(format string, args ...any) { log.Printf("wallet: "+format, args...) },
	}
}

// SetPendingTTL overrides the TTL used when staging costs from
// TaskSucceeded. Safe to call before Start; not safe to call concurrently
// with running event delivery.
func (s *Subscriber) SetPendingTTL(ttl time.Duration) {
	if ttl > 0 {
		s.pendingTTL = ttl
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
		taskID := string(e.GetTaskID())
		if err := s.pendingCost.Set(ctx, taskID, adapter.CostUSD(e.ActualCost), s.pendingTTL); err != nil {
			// Set failure means AssetHosted later will fall through to the
			// "without prior TaskSucceeded" refund path. Log so ops can
			// see Redis hiccups without dropping the event.
			s.errLogger("TaskSucceeded pendingCost.Set failed task=%s: %v", taskID, err)
		}
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
	actual, ok, err := s.pendingCost.Get(ctx, taskID)
	if err != nil {
		// Treat transport errors as a miss and refund — same fail-safe
		// posture as the F9 fix below: better to refund than to guess.
		s.errLogger("AssetHosted pendingCost.Get failed task=%s: %v", taskID, err)
		ok = false
	}
	if ok {
		// Best-effort cleanup; failure here just means the entry survives
		// until TTL. Not worth blocking the Settle on it.
		if err := s.pendingCost.Delete(ctx, taskID); err != nil {
			s.errLogger("AssetHosted pendingCost.Delete failed task=%s: %v", taskID, err)
		}
	}

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
