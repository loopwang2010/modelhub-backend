// End-to-end Subscriber tests against a real Postgres testcontainer +
// the F5 PendingCostStore (InMemoryPendingStore). These complement the
// SQLite subscriber tests in coverage_test.go by proving the full
// EventBus → Wallet path also works against the production SQL.
//
// Per F2 in plans/CODE-REVIEW.md the subscriber's coverage is "limited"
// for the post-F5 path on real Postgres. This file:
//
//   - Wires NewSubscriberWithStore(NewInMemoryPendingStore()) explicitly.
//   - Exercises the full TaskSucceeded → AssetHosted → Settle flow on
//     real Postgres (subscriber.go onAssetHosted hot path).
//   - Exercises the AssetLost branch with a real fractionFn against PG.
//   - Exercises the terminal events (TaskFailed/TimedOut/Cancelled) that
//     drive Refund through onTaskTerminal.
//   - Asserts InvariantSum == 0 after each delivery (sum-zero across
//     real BIGINT/CHECK-constraint Postgres rows).
//
// All tests skip cleanly when Docker is unavailable.

package wallet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
)

// pgSubscriberHarness wires real Postgres + real bus + Subscriber.
type pgSubscriberHarness struct {
	w     *DBWallet
	bus   *events.MemoryBus
	sub   *Subscriber
	store *InMemoryPendingStore
	logs  *recordingLogger
}

// newPGSubscriberHarness skips on no-Docker (via newPostgresWallet) and
// returns a fully wired harness, registering invariant + cleanup hooks
// on t.Cleanup.
func newPGSubscriberHarness(t *testing.T, fractionFn AssetCostFractionFunc) *pgSubscriberHarness {
	t.Helper()
	w, _ := newPostgresWallet(t)
	if w == nil {
		return nil
	}
	bus := events.NewMemoryBus()
	store := NewInMemoryPendingStore()
	logs := &recordingLogger{}
	sub := NewSubscriberWithStore(w, bus, fractionFn, store)
	sub.SetErrLogger(logs.capture())
	unsub := sub.Start(context.Background())
	t.Cleanup(func() {
		unsub()
		_ = bus.Close()
	})
	return &pgSubscriberHarness{w: w, bus: bus, sub: sub, store: store, logs: logs}
}

// TestPGSubscriber_HappyPath_TaskSucceededThenAssetHosted is the canonical
// post-F5 happy path: TaskSucceeded → store stages cost → AssetHosted →
// Settle on real Postgres. Verifies the InMemoryPendingStore bridge
// works end-to-end against the production SQL.
func TestPGSubscriber_HappyPath_TaskSucceededThenAssetHosted(t *testing.T) {
	h := newPGSubscriberHarness(t, nil)
	if h == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-sub-happy"
	if err := h.w.Topup(ctx, acct, 2_000_000, "seed", "admin", "idem-pg-sub-happy-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	if _, err := h.w.Hold(ctx, acct, "t-pg-happy", 500_000, "idem-pg-sub-happy-hold"); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	publishPG(t, h.bus, events.MakeTaskSucceeded(events.NewBaseEvent("t-pg-happy"), 400_000))

	// The store should now contain the staged cost.
	cost, ok, _ := h.store.Get(ctx, "t-pg-happy")
	if !ok || cost != 400_000 {
		t.Errorf("pending store after TaskSucceeded: cost=%d ok=%v; want 400_000 true", cost, ok)
	}

	publishPG(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-pg-happy"),
		"https://cdn.example.com/x.png", 1024))

	// AssetHosted should drain the escrow + delete the staged cost.
	if _, ok, _ := h.store.Get(ctx, "t-pg-happy"); ok {
		t.Errorf("pending store still has entry after AssetHosted; should be deleted")
	}
	bal, _ := h.w.Balance(ctx, acct)
	if bal != 1_600_000 {
		t.Errorf("user balance = %d; want 1_600_000", bal)
	}
	rev, _ := h.w.Balance(ctx, SystemAccountRevenue)
	if rev != 400_000 {
		t.Errorf("revenue = %d; want 400_000", rev)
	}
	if sum, _ := h.w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum = %d; want 0", sum)
	}
}

// TestPGSubscriber_AssetLost_DrivesPartialRefund exercises onAssetLost
// against real Postgres including the fractionFn callback.
func TestPGSubscriber_AssetLost_DrivesPartialRefund(t *testing.T) {
	called := atomic.Bool{}
	fn := func(_ context.Context, taskID string) (float64, error) {
		called.Store(true)
		if taskID != "t-pg-lost" {
			t.Errorf("fractionFn taskID = %q", taskID)
		}
		return 0.4, nil
	}
	h := newPGSubscriberHarness(t, fn)
	if h == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-sub-lost"
	if err := h.w.Topup(ctx, acct, 1_000_000, "seed", "admin", "idem-pg-sub-lost-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	if _, err := h.w.Hold(ctx, acct, "t-pg-lost", 500_000, "idem-pg-sub-lost-hold"); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	publishPG(t, h.bus, events.MakeAssetLost(events.NewBaseEvent("t-pg-lost"),
		"https://upstream/x.png", "404 expired"))

	if !called.Load() {
		t.Error("fractionFn not invoked")
	}
	// 40% of 500_000 = 200_000 refunded → user has 500_000 + 200_000 = 700_000.
	bal, _ := h.w.Balance(ctx, acct)
	if bal != 700_000 {
		t.Errorf("balance = %d; want 700_000 (60pct revenue / 40pct refund)", bal)
	}
	rev, _ := h.w.Balance(ctx, SystemAccountRevenue)
	if rev != 300_000 {
		t.Errorf("revenue = %d; want 300_000 (60 percent of 500_000)", rev)
	}
	if sum, _ := h.w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum = %d; want 0", sum)
	}
}

// TestPGSubscriber_TaskTerminalEvents_AllRefund exercises the three
// terminal-event handlers on real Postgres.
func TestPGSubscriber_TaskTerminalEvents_AllRefund(t *testing.T) {
	cases := []struct {
		name    string
		taskID  string
		makeEv  func(events.BaseEvent) events.Event
	}{
		{"failed", "t-pg-term-failed", func(b events.BaseEvent) events.Event {
			return events.MakeTaskFailed(b, "upstream", "boom")
		}},
		{"timed_out", "t-pg-term-timeout", func(b events.BaseEvent) events.Event {
			return events.MakeTaskTimedOut(b, 0)
		}},
		{"cancelled", "t-pg-term-cancel", func(b events.BaseEvent) events.Event {
			return events.MakeTaskCancelled(b, 0, "user")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGSubscriberHarness(t, nil)
			if h == nil {
				return
			}
			ctx := context.Background()
			acct := AccountID("user:pg-term-" + tc.name)
			if err := h.w.Topup(ctx, acct, 1_000_000, "seed", "admin",
				adapter.IdempotencyKey("idem-pg-term-seed-"+tc.name)); err != nil {
				t.Fatalf("Topup: %v", err)
			}
			if _, err := h.w.Hold(ctx, acct, tc.taskID, 500_000,
				adapter.IdempotencyKey("idem-pg-term-hold-"+tc.name)); err != nil {
				t.Fatalf("Hold: %v", err)
			}
			publishPG(t, h.bus, tc.makeEv(events.NewBaseEvent(events.TaskID(tc.taskID))))

			bal, _ := h.w.Balance(ctx, acct)
			if bal != 1_000_000 {
				t.Errorf("balance = %d; want 1_000_000 (full refund)", bal)
			}
			if sum, _ := h.w.InvariantSum(ctx); sum != 0 {
				t.Errorf("InvariantSum = %d; want 0", sum)
			}
		})
	}
}

// TestPGSubscriber_AssetHostedWithoutPriorSucceeded_RefundsFundedEscrow
// exercises the F9 fix branch on real Postgres: a funded escrow that
// receives AssetHosted without prior TaskSucceeded must Refund instead
// of crashing on CHECK(amount<>0).
func TestPGSubscriber_AssetHostedWithoutPriorSucceeded_RefundsFundedEscrow(t *testing.T) {
	h := newPGSubscriberHarness(t, nil)
	if h == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-sub-noprior"
	if err := h.w.Topup(ctx, acct, 1_000_000, "seed", "admin", "idem-pg-sub-noprior-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	if _, err := h.w.Hold(ctx, acct, "t-pg-noprior", 500_000, "idem-pg-sub-noprior-hold"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	publishPG(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-pg-noprior"),
		"https://cdn.example.com/x.png", 1024))

	if !h.logs.any("without prior TaskSucceeded") {
		t.Errorf("expected warning log; got %v", h.logs.logs)
	}
	bal, _ := h.w.Balance(ctx, acct)
	if bal != 1_000_000 {
		t.Errorf("balance = %d; want 1_000_000 (full refund via F9 fix)", bal)
	}
	if sum, _ := h.w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum = %d; want 0", sum)
	}
}

// TestPGSubscriber_PendingTTLOverride confirms SetPendingTTL is plumbed.
// Sets a tiny TTL, advances the InMemoryStore clock past expiry, and
// verifies AssetHosted falls through the F9 fallback-Refund branch
// because pendingCost.Get returns false.
func TestPGSubscriber_PendingTTLOverride_ExpiryTriggersFallback(t *testing.T) {
	h := newPGSubscriberHarness(t, nil)
	if h == nil {
		return
	}
	// Fake clock controlled here.
	now := time.Now()
	h.store.SetClock(func() time.Time { return now })
	h.sub.SetPendingTTL(50 * time.Millisecond)

	ctx := context.Background()
	const acct AccountID = "user:pg-sub-ttl"
	if err := h.w.Topup(ctx, acct, 1_000_000, "seed", "admin", "idem-pg-sub-ttl-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	if _, err := h.w.Hold(ctx, acct, "t-pg-ttl", 400_000, "idem-pg-sub-ttl-hold"); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	publishPG(t, h.bus, events.MakeTaskSucceeded(events.NewBaseEvent("t-pg-ttl"), 300_000))
	// Advance store clock past TTL.
	now = now.Add(time.Second)

	publishPG(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-pg-ttl"),
		"https://cdn.example.com/x.png", 1024))

	if !h.logs.any("without prior TaskSucceeded") {
		t.Errorf("TTL expiry should drive fallback; got logs=%v", h.logs.logs)
	}
	bal, _ := h.w.Balance(ctx, acct)
	if bal != 1_000_000 {
		t.Errorf("balance = %d; want 1_000_000 (refunded)", bal)
	}
	if sum, _ := h.w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum = %d; want 0", sum)
	}
}

// TestPGSubscriber_NewSubscriberWithStore_PanicsOnNilStore covers the
// final uncovered branch in NewSubscriberWithStore (90.9% baseline).
func TestPGSubscriber_NewSubscriberWithStore_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil store")
		}
	}()
	bus := events.NewMemoryBus()
	defer bus.Close()
	// We don't need Postgres for this — skip not applicable.
	_ = NewSubscriberWithStore(struct{ Wallet }{}, bus, nil, nil)
}

// TestPGSubscriber_ConcurrentAssetHostedDeliveries exercises Settle under
// concurrent event delivery on real Postgres — pure smoke of the
// SERIALIZABLE retry path through the subscriber wiring (rather than
// the wallet API directly as in tx_serializable_test.go).
func TestPGSubscriber_ConcurrentAssetHostedDeliveries(t *testing.T) {
	h := newPGSubscriberHarness(t, nil)
	if h == nil {
		return
	}
	ctx := context.Background()

	const N = 4
	const acct AccountID = "user:pg-sub-conc"
	if err := h.w.Topup(ctx, acct, 10_000_000, "seed", "admin", "idem-pg-sub-conc-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	for i := 0; i < N; i++ {
		taskID := fmt.Sprintf("t-pg-sub-conc-%d", i)
		if _, err := h.w.Hold(ctx, acct, taskID, 500_000,
			adapter.IdempotencyKey(fmt.Sprintf("idem-pg-sub-conc-hold-%d", i))); err != nil {
			t.Fatalf("Hold %d: %v", i, err)
		}
	}

	// Fire all the TaskSucceeded events first (they're sync-published).
	for i := 0; i < N; i++ {
		taskID := fmt.Sprintf("t-pg-sub-conc-%d", i)
		publishPG(t, h.bus, events.MakeTaskSucceeded(events.NewBaseEvent(events.TaskID(taskID)), 400_000))
	}

	// MemoryBus.Publish is synchronous, so to actually exercise concurrent
	// delivery into the subscriber we publish AssetHosted from goroutines.
	// The handler internally calls Settle, which goes through runTx.
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("t-pg-sub-conc-%d", idx)
			ev := events.MakeAssetHosted(events.NewBaseEvent(events.TaskID(taskID)),
				"https://cdn.example.com/x.png", 1024)
			errs[idx] = h.bus.Publish(ev)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("publish %d: %v", i, e)
		}
	}

	// Each Settle drained 400_000; user got 100_000 over-refund each ×4.
	bal, _ := h.w.Balance(ctx, acct)
	expected := adapter.CostUSD(10_000_000 - N*500_000 + N*100_000) // start - holds + over-refund
	if bal != expected {
		t.Errorf("balance = %d; want %d", bal, expected)
	}
	if sum, _ := h.w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after concurrent deliveries = %d; want 0", sum)
	}
	// No raw serialization failures should have leaked into the error log.
	if h.logs.any("could not serialize") || h.logs.any("40001") {
		t.Errorf("raw PG error in logs: %v", h.logs.logs)
	}
}

// publishPG is the Postgres-test variant of publish() in coverage_test.go;
// kept with a unique name so both files can coexist without symbol clash.
func publishPG(t *testing.T, bus *events.MemoryBus, ev events.Event) {
	t.Helper()
	if err := bus.Publish(ev); err != nil {
		t.Fatalf("publish %s: %v", ev.Kind(), err)
	}
}

// Ensure errors import is not flagged unused — used in the subscriber
// tests to check ErrEscrowAlreadySettled / etc. in future expansions.
var _ = errors.Is
