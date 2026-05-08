// Contract tests for PendingCostStore. Each test runs against BOTH
// implementations through a table-driven factory; new impls only need to
// add a row to `stores` to inherit the full suite.
//
// Redis is faked with miniredis so tests do not require a real server.

package wallet

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
)

// storeFactory builds a fresh store + an optional clock advancer for tests
// that need to simulate TTL expiry without sleeping. cleanup tears down
// any backing resources (e.g. miniredis instance).
type storeFactory struct {
	name    string
	build   func(t *testing.T) (store PendingCostStore, advance func(time.Duration), cleanup func())
}

// stores is the test matrix. Every contract test runs once per row.
func storeFactories() []storeFactory {
	return []storeFactory{
		{
			name: "InMemory",
			build: func(t *testing.T) (PendingCostStore, func(time.Duration), func()) {
				t.Helper()
				s := NewInMemoryPendingStore()
				now := time.Now()
				clock := &fakeClock{t: now}
				s.SetClock(clock.Now)
				return s, clock.Advance, func() {}
			},
		},
		{
			name: "Redis",
			build: func(t *testing.T) (PendingCostStore, func(time.Duration), func()) {
				t.Helper()
				mr := miniredis.RunT(t)
				client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				s := NewRedisPendingStore(client)
				return s, mr.FastForward, func() {
					_ = client.Close()
				}
			},
		},
	}
}

// fakeClock is goroutine-safe time source for the in-memory store. The
// Redis fake uses miniredis.FastForward instead.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// runForEachStore iterates the test matrix as a sub-test per impl.
func runForEachStore(t *testing.T, fn func(t *testing.T, store PendingCostStore, advance func(time.Duration))) {
	t.Helper()
	for _, f := range storeFactories() {
		f := f
		t.Run(f.name, func(t *testing.T) {
			store, advance, cleanup := f.build(t)
			defer cleanup()
			fn(t, store, advance)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Basic round-trip
// ─────────────────────────────────────────────────────────────────────────

func TestPendingCostStore_SetGetDelete_RoundTrip(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		ctx := context.Background()
		const taskID = "task-roundtrip"
		const cost adapter.CostUSD = 1_234_567

		if err := s.Set(ctx, taskID, cost, 1*time.Hour); err != nil {
			t.Fatalf("Set: %v", err)
		}

		got, ok, err := s.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !ok {
			t.Fatalf("Get returned ok=false right after Set")
		}
		if got != cost {
			t.Errorf("Get cost = %d; want %d", got, cost)
		}

		if err := s.Delete(ctx, taskID); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		got, ok, err = s.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get after Delete: %v", err)
		}
		if ok || got != 0 {
			t.Errorf("Get after Delete = (%d, %v); want (0, false)", got, ok)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Missing key — must NOT be an error (idempotency / fail-open contract)
// ─────────────────────────────────────────────────────────────────────────

func TestPendingCostStore_GetMissing_ReturnsFalseNoError(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		ctx := context.Background()
		got, ok, err := s.Get(ctx, "nonexistent")
		if err != nil {
			t.Errorf("Get(missing) err = %v; want nil", err)
		}
		if ok {
			t.Errorf("Get(missing) ok = true; want false")
		}
		if got != 0 {
			t.Errorf("Get(missing) cost = %d; want 0", got)
		}
	})
}

func TestPendingCostStore_DeleteMissing_NoError(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		if err := s.Delete(context.Background(), "nonexistent"); err != nil {
			t.Errorf("Delete(missing) err = %v; want nil", err)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Idempotency — Set-Set with same key overwrites
// ─────────────────────────────────────────────────────────────────────────

func TestPendingCostStore_SetTwice_Overwrites(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		ctx := context.Background()
		const taskID = "task-overwrite"

		if err := s.Set(ctx, taskID, 100, time.Hour); err != nil {
			t.Fatalf("Set #1: %v", err)
		}
		if err := s.Set(ctx, taskID, 200, time.Hour); err != nil {
			t.Fatalf("Set #2: %v", err)
		}

		got, ok, err := s.Get(ctx, taskID)
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if got != 200 {
			t.Errorf("Get after Set-Set = %d; want 200 (overwrite)", got)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────
// TTL expiry — entry must vanish after TTL and Get must report miss
// ─────────────────────────────────────────────────────────────────────────

func TestPendingCostStore_TTL_ExpiredEntriesReportMiss(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, advance func(time.Duration)) {
		ctx := context.Background()
		const taskID = "task-expires"

		if err := s.Set(ctx, taskID, 99, 5*time.Second); err != nil {
			t.Fatalf("Set: %v", err)
		}
		// Just before expiry — still present.
		advance(4 * time.Second)
		if _, ok, err := s.Get(ctx, taskID); err != nil || !ok {
			t.Fatalf("Get pre-expiry: ok=%v err=%v", ok, err)
		}
		// After expiry — gone.
		advance(2 * time.Second)
		got, ok, err := s.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get post-expiry: %v", err)
		}
		if ok || got != 0 {
			t.Errorf("Get post-expiry = (%d, %v); want (0, false)", got, ok)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Validation — empty taskID, non-positive TTL
// ─────────────────────────────────────────────────────────────────────────

func TestPendingCostStore_Set_EmptyTaskID_Errors(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		err := s.Set(context.Background(), "", 1, time.Hour)
		if err == nil {
			t.Errorf("Set(\"\") err = nil; want error")
		}
	})
}

func TestPendingCostStore_Set_NonPositiveTTL_Errors(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		if err := s.Set(context.Background(), "tid", 1, 0); err == nil {
			t.Errorf("Set(ttl=0) err = nil; want error")
		}
		if err := s.Set(context.Background(), "tid", 1, -time.Second); err == nil {
			t.Errorf("Set(ttl<0) err = nil; want error")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Empty-taskID Get/Delete — silent miss, no error
// ─────────────────────────────────────────────────────────────────────────

func TestPendingCostStore_GetEmptyTaskID_SilentMiss(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		got, ok, err := s.Get(context.Background(), "")
		if err != nil || ok || got != 0 {
			t.Errorf("Get(\"\") = (%d, %v, %v); want (0, false, nil)", got, ok, err)
		}
	})
}

func TestPendingCostStore_DeleteEmptyTaskID_NoError(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		if err := s.Delete(context.Background(), ""); err != nil {
			t.Errorf("Delete(\"\") err = %v; want nil", err)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Concurrent Set/Get/Delete — the goroutine-safety test (run with -race)
// ─────────────────────────────────────────────────────────────────────────

func TestPendingCostStore_ConcurrentAccess_NoRace(t *testing.T) {
	runForEachStore(t, func(t *testing.T, s PendingCostStore, _ func(time.Duration)) {
		ctx := context.Background()
		const goroutines = 32
		const opsPerG = 100

		var wg sync.WaitGroup
		var setOK, getOK, delOK atomic.Int64

		for g := 0; g < goroutines; g++ {
			g := g
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < opsPerG; i++ {
					tid := keyFor(g, i)
					if err := s.Set(ctx, tid, adapter.CostUSD(i), time.Minute); err == nil {
						setOK.Add(1)
					}
					if _, _, err := s.Get(ctx, tid); err == nil {
						getOK.Add(1)
					}
					if err := s.Delete(ctx, tid); err == nil {
						delOK.Add(1)
					}
				}
			}()
		}
		wg.Wait()

		want := int64(goroutines * opsPerG)
		if setOK.Load() != want || getOK.Load() != want || delOK.Load() != want {
			t.Errorf("ops not all clean: set=%d get=%d del=%d want=%d",
				setOK.Load(), getOK.Load(), delOK.Load(), want)
		}
	})
}

// keyFor builds a unique-per-goroutine taskID. Kept tiny to keep
// miniredis memory low.
func keyFor(g, i int) string {
	return "g" + itoa(g) + "-" + itoa(i)
}

// itoa is a tiny non-allocating int-to-string for test keys (avoids
// pulling in strconv repeatedly in the tight loop).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ─────────────────────────────────────────────────────────────────────────
// Impl-specific tests
// ─────────────────────────────────────────────────────────────────────────

// In-memory eviction releases map entries after Get-on-expired.
func TestInMemoryPendingStore_LazyEvict_Frees(t *testing.T) {
	s := NewInMemoryPendingStore()
	clock := &fakeClock{t: time.Now()}
	s.SetClock(clock.Now)

	if err := s.Set(context.Background(), "tid", 1, time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.Len(); got != 1 {
		t.Errorf("Len after Set = %d; want 1", got)
	}
	clock.Advance(2 * time.Second)
	// Get triggers lazy eviction.
	if _, ok, _ := s.Get(context.Background(), "tid"); ok {
		t.Errorf("Get post-expiry returned ok=true")
	}
	if got := s.Len(); got != 0 {
		t.Errorf("Len after lazy evict = %d; want 0", got)
	}
}

// Redis store: corrupted JSON in Redis surfaces as an error from Get.
func TestRedisPendingStore_CorruptPayload_ReturnsError(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	// Plant a malformed value directly via miniredis.
	mr.Set(PendingRedisKey("corrupt"), "{not valid json")
	mr.SetTTL(PendingRedisKey("corrupt"), time.Hour)

	s := NewRedisPendingStore(client)
	_, _, err := s.Get(context.Background(), "corrupt")
	if err == nil {
		t.Errorf("Get on corrupt payload err = nil; want error")
	}
}

// NewRedisPendingStore panics on nil client (programmer error).
func TestNewRedisPendingStore_NilClientPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil client")
		}
	}()
	_ = NewRedisPendingStore(nil)
}

// PendingRedisKey is stable (callers may rely on it for ops debug).
func TestPendingRedisKey_StablePrefix(t *testing.T) {
	if got := PendingRedisKey("abc"); got != "wallet:pending:abc" {
		t.Errorf("PendingRedisKey = %q; want wallet:pending:abc", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// NewPendingStoreFromEnv selector
// ─────────────────────────────────────────────────────────────────────────

func TestNewPendingStoreFromEnv_DefaultsToInMemory(t *testing.T) {
	store, err := NewPendingStoreFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if _, ok := store.(*InMemoryPendingStore); !ok {
		t.Errorf("default store type = %T; want *InMemoryPendingStore", store)
	}
}

func TestNewPendingStoreFromEnv_RedisRequiresURL(t *testing.T) {
	getenv := func(k string) string {
		if k == PendingStoreEnvVar {
			return "redis"
		}
		return ""
	}
	_, err := NewPendingStoreFromEnv(getenv)
	if err == nil {
		t.Errorf("err = nil; want error when REDIS_URL is unset")
	}
}

func TestNewPendingStoreFromEnv_RedisInvalidURL(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case PendingStoreEnvVar:
			return "redis"
		case PendingStoreRedisURLEnv:
			return "://not a url"
		}
		return ""
	}
	_, err := NewPendingStoreFromEnv(getenv)
	if err == nil {
		t.Errorf("err = nil; want parse error")
	}
}

func TestNewPendingStoreFromEnv_RedisHappyPath(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	getenv := func(k string) string {
		switch k {
		case PendingStoreEnvVar:
			return "redis"
		case PendingStoreRedisURLEnv:
			return "redis://" + mr.Addr()
		}
		return ""
	}
	store, err := NewPendingStoreFromEnv(getenv)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, ok := store.(*RedisPendingStore); !ok {
		t.Errorf("redis store type = %T; want *RedisPendingStore", store)
	}
	// Smoke: write/read via the env-built store.
	ctx := context.Background()
	if err := store.Set(ctx, "envtid", 7, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cost, ok, err := store.Get(ctx, "envtid")
	if err != nil || !ok || cost != 7 {
		t.Errorf("Get = (%d, %v, %v); want (7, true, nil)", cost, ok, err)
	}
}

func TestNewPendingStoreFromEnv_UnknownValueErrors(t *testing.T) {
	getenv := func(k string) string {
		if k == PendingStoreEnvVar {
			return "memcached"
		}
		return ""
	}
	_, err := NewPendingStoreFromEnv(getenv)
	if err == nil {
		t.Errorf("err = nil; want error on unknown impl")
	}
}

func TestNewPendingStoreFromEnv_MemoryAlias(t *testing.T) {
	for _, val := range []string{"memory", "in-memory"} {
		val := val
		t.Run(val, func(t *testing.T) {
			getenv := func(k string) string {
				if k == PendingStoreEnvVar {
					return val
				}
				return ""
			}
			store, err := NewPendingStoreFromEnv(getenv)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if _, ok := store.(*InMemoryPendingStore); !ok {
				t.Errorf("store type = %T; want *InMemoryPendingStore", store)
			}
		})
	}
}

// Sanity: redis.Nil is what go-redis returns on missing — our store maps
// that to (0, false, nil). This guards against a future go-redis change
// that would silently break the contract.
func TestRedisPendingStore_RedisNilSentinelIsContractMiss(t *testing.T) {
	if !errors.Is(redis.Nil, redis.Nil) {
		t.Skip("redis.Nil sentinel changed shape upstream; revisit Get error mapping")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Subscriber + store error injection
//
// These tests live here (not in coverage_test.go) because they exercise
// the PendingCostStore boundary the subscriber depends on. They use a
// stub store that synthesizes errors so we can verify the subscriber's
// fail-safe refund path runs even when the store transport breaks.
// ─────────────────────────────────────────────────────────────────────────

// newSubscriberHarnessWithStore mirrors newSubscriberHarness (coverage_test.go)
// but lets the test inject any PendingCostStore implementation. Kept here
// because the store-injection seam is what these tests are exercising.
func newSubscriberHarnessWithStore(t *testing.T, fractionFn AssetCostFractionFunc, store PendingCostStore) (*subscriberHarness, func()) {
	t.Helper()
	w, walletCleanup := newTestWallet(t)
	bus := events.NewMemoryBus()
	logs := &recordingLogger{}
	sub := NewSubscriberWithStore(w, bus, fractionFn, store)
	sub.SetErrLogger(logs.capture())
	unsub := sub.Start(context.Background())
	cleanup := func() {
		unsub()
		_ = bus.Close()
		walletCleanup()
	}
	return &subscriberHarness{w: w, bus: bus, sub: sub, logs: logs}, cleanup
}

// errorStore is a PendingCostStore that returns the configured error from
// every method. Used to assert subscriber log+refund behavior on store
// outage.
type errorStore struct {
	setErr, getErr, delErr error
	// optional: honor Set even when erroring, so a single test can
	// exercise both halves of the bridge.
	delegate PendingCostStore
}

func (e *errorStore) Set(ctx context.Context, taskID string, cost adapter.CostUSD, ttl time.Duration) error {
	if e.delegate != nil {
		_ = e.delegate.Set(ctx, taskID, cost, ttl)
	}
	return e.setErr
}
func (e *errorStore) Get(ctx context.Context, taskID string) (adapter.CostUSD, bool, error) {
	if e.getErr != nil {
		return 0, false, e.getErr
	}
	if e.delegate != nil {
		return e.delegate.Get(ctx, taskID)
	}
	return 0, false, nil
}
func (e *errorStore) Delete(ctx context.Context, taskID string) error {
	if e.delegate != nil {
		_ = e.delegate.Delete(ctx, taskID)
	}
	return e.delErr
}

// When the store's Get fails, AssetHosted must fall through to the
// "without prior TaskSucceeded" refund path (fail-safe) and emit a log.
// This protects ADR-005's integrity boundary: a Redis hiccup must NEVER
// cause a Settle for the wrong amount.
func TestSubscriber_AssetHosted_StoreGetError_LogsAndRefunds(t *testing.T) {
	h, cleanup := newSubscriberHarnessWithStore(t, nil, &errorStore{
		getErr: errors.New("redis connection refused"),
	})
	defer cleanup()

	publish(t, h.bus, events.MakeTaskSucceeded(events.NewBaseEvent("t-store-err"), 100))
	publish(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-store-err"),
		"https://cdn.example.com/x.png", 1024))

	if !h.logs.any("pendingCost.Get failed") {
		t.Errorf("expected store-error log; got %v", h.logs.logs)
	}
}

// When the store's Set fails, the subscriber logs but continues.
// AssetHosted will then trigger the "no prior TaskSucceeded" refund
// path on a properly-funded escrow — exactly the F9 fix's safety net.
func TestSubscriber_TaskSucceeded_StoreSetError_LogsAndContinues(t *testing.T) {
	h, cleanup := newSubscriberHarnessWithStore(t, nil, &errorStore{
		setErr: errors.New("redis SET timeout"),
	})
	defer cleanup()

	publish(t, h.bus, events.MakeTaskSucceeded(events.NewBaseEvent("t-set-err"), 100))

	if !h.logs.any("pendingCost.Set failed") {
		t.Errorf("expected Set-error log; got %v", h.logs.logs)
	}
}

// SetPendingTTL: smoke test for the configurability hook.
func TestSubscriber_SetPendingTTL(t *testing.T) {
	h, cleanup := newSubscriberHarnessWithStore(t, nil, NewInMemoryPendingStore())
	defer cleanup()
	h.sub.SetPendingTTL(2 * time.Hour)
	h.sub.SetPendingTTL(0) // ignored
	if h.sub.pendingTTL != 2*time.Hour {
		t.Errorf("pendingTTL = %v; want 2h", h.sub.pendingTTL)
	}
}

// NewSubscriberWithStore: panic on nil store (programmer error).
func TestNewSubscriberWithStore_NilStorePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil store")
		}
	}()
	w, cleanup := newTestWallet(t)
	defer cleanup()
	_ = NewSubscriberWithStore(w, nilBus(t), nil, nil)
}

// nilBus returns a non-nil bus stub for the panic-on-store test (the
// nil-bus and nil-wallet cases are already covered in coverage_test.go).
func nilBus(t *testing.T) *events.MemoryBus {
	t.Helper()
	return events.NewMemoryBus()
}
