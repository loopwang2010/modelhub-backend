// Package events defines the FSM event contract emitted by the task worker
// (S5) and consumed by the wallet (S6) and asset hosting (S9.5) layers.
//
// Per ADR-011 and the S2.5 blueprint section, this package is owned by S2.5
// to break the would-be S5 ↔ S6 circular dependency. Both producer and
// consumer import only `internal/events` — never each other.
//
// Event taxonomy (one variant per FSM transition that has a side-effect):
//
//	TaskHeld          — wallet must reserve credit
//	TaskSubmitted     — upstream acknowledged the request
//	TaskRunning       — upstream reports work in progress
//	TaskSucceeded     — upstream produced output (carries actual_cost)
//	TaskFailed        — terminal failure (carries error_class)
//	TaskTimedOut      — exceeded SLA (carries prepaid_amount)
//	TaskCancelled     — user/admin cancelled (carries refund_amount)
//	OutputAvailable   — asset URL ready (S9.5 emits to advance Settle)
//	AssetHosted       — CDN copy confirmed (wallet listens to Settle)
//	AssetLost         — asset worker exhausted retries (partial refund)
//
// Sealed-interface pattern: Event is a sealed sum type. Concrete variants
// implement an unexported method `eventKind()` so that no other package can
// declare a new variant.
//
// EventBus is intentionally minimal — Publish + Subscribe + Unsubscribe.
// In-memory implementation (NewMemoryBus) ships with this package for unit
// tests and dev-mode boot. Production durable implementation (e.g. Redis
// streams or NATS) lives in S5.

package events

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// Identity
// ─────────────────────────────────────────────────────────────────────────

// TaskID is an opaque task identifier. Producers and subscribers treat it
// as an opaque string; format ("gen_xxx" per ADR-009) is owned by S5.
type TaskID string

// CostUSD mirrors adapter.CostUSD (micro-USD). Re-declared locally to keep
// internal/events free of an internal/adapter import cycle.
type CostUSD int64

// ErrorClass mirrors adapter.ErrorClass; same rationale.
type ErrorClass string

// ─────────────────────────────────────────────────────────────────────────
// Sealed Event sum type
// ─────────────────────────────────────────────────────────────────────────

// Event is the sealed interface every variant implements.
// The unexported eventKind() method prevents new variants outside this package.
type Event interface {
	// TaskID returns the task this event pertains to. Every variant carries one.
	GetTaskID() TaskID

	// At returns when the event occurred (UTC). Producers are responsible
	// for filling this; subscribers may use it for ordering or audit.
	At() time.Time

	// Kind returns a stable string discriminator (e.g. "task_held"). Useful for
	// metrics labels, logs, and admin tooling. Sealed-method variant marker
	// is `eventKind()` (unexported).
	Kind() string

	eventKind() // unexported — sealed
}

// baseEvent embeds common fields. Variants embed it to inherit GetTaskID/At.
type baseEvent struct {
	TaskID TaskID    `json:"task_id"`
	When   time.Time `json:"at"`
}

func (b baseEvent) GetTaskID() TaskID { return b.TaskID }
func (b baseEvent) At() time.Time     { return b.When }

// TaskHeld — wallet must reserve credit equal to HeldAmount.
// Emitted when the FSM transitions Created → Held.
type TaskHeld struct {
	baseEvent
	HeldAmount    CostUSD `json:"held_amount"`
	Model         string  `json:"model"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (TaskHeld) eventKind()    {}
func (TaskHeld) Kind() string  { return "task_held" }

// TaskSubmitted — upstream acknowledged. Wallet/UI may surface this state.
type TaskSubmitted struct {
	baseEvent
	Provider    string `json:"provider"`
	UpstreamRef string `json:"upstream_ref,omitempty"` // empty for sync inline
}

func (TaskSubmitted) eventKind()   {}
func (TaskSubmitted) Kind() string { return "task_submitted" }

// TaskRunning — upstream reports active work (async only).
// Progress is 0..1 when reported by upstream; nil otherwise.
type TaskRunning struct {
	baseEvent
	Progress *float32 `json:"progress,omitempty"`
}

func (TaskRunning) eventKind()   {}
func (TaskRunning) Kind() string { return "task_running" }

// TaskSucceeded — upstream produced output. Wallet must Settle:
// move (held - actual_cost) back to user, ActualCost to revenue.
type TaskSucceeded struct {
	baseEvent
	ActualCost CostUSD `json:"actual_cost"`
}

func (TaskSucceeded) eventKind()   {}
func (TaskSucceeded) Kind() string { return "task_succeeded" }

// TaskFailed — terminal failure. Wallet refunds full HeldAmount.
type TaskFailed struct {
	baseEvent
	ErrorClass ErrorClass `json:"error_class"`
	Message    string     `json:"message"` // sanitized; safe for user surfacing
}

func (TaskFailed) eventKind()   {}
func (TaskFailed) Kind() string { return "task_failed" }

// TaskTimedOut — exceeded SLA. PrepaidAmount lets wallet decide partial vs
// full refund (e.g. if upstream cost was already incurred).
type TaskTimedOut struct {
	baseEvent
	PrepaidAmount CostUSD `json:"prepaid_amount"`
}

func (TaskTimedOut) eventKind()   {}
func (TaskTimedOut) Kind() string { return "task_timed_out" }

// TaskCancelled — user or admin cancelled. RefundAmount is what wallet
// should release (typically full HeldAmount; less if upstream charged us).
type TaskCancelled struct {
	baseEvent
	RefundAmount CostUSD `json:"refund_amount"`
	By           string  `json:"by"` // "user" | "admin"
}

func (TaskCancelled) eventKind()   {}
func (TaskCancelled) Kind() string { return "task_cancelled" }

// OutputAvailable — output URL is the upstream's CDN URL; asset worker
// uses it to download and re-host. NEVER surfaced to API clients.
//
// Per ADR-018 / AP-19, the URL field returned to API clients is always the
// post-S9.5 CDN URL. OutputAvailable carries the *upstream* URL only as
// the asset worker's input.
type OutputAvailable struct {
	baseEvent
	UpstreamURL string `json:"upstream_url"`
	MimeType    string `json:"mime_type,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
}

func (OutputAvailable) eventKind()   {}
func (OutputAvailable) Kind() string { return "output_available" }

// AssetHosted — asset worker confirmed CDN copy. Wallet may now Settle.
type AssetHosted struct {
	baseEvent
	CDNURL    string `json:"cdn_url"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

func (AssetHosted) eventKind()   {}
func (AssetHosted) Kind() string { return "asset_hosted" }

// AssetLost — asset worker exhausted retries. Wallet partial-refunds.
type AssetLost struct {
	baseEvent
	UpstreamURL string `json:"upstream_url"`
	Reason      string `json:"reason"`
}

func (AssetLost) eventKind()   {}
func (AssetLost) Kind() string { return "asset_lost" }

// ─────────────────────────────────────────────────────────────────────────
// EventBus interface
// ─────────────────────────────────────────────────────────────────────────

// EventBus is the publish/subscribe contract. Production impl in S5.
//
// Semantics:
//   - Publish blocks until all CURRENT subscribers have been delivered the
//     event. Subscribers added after Publish returns will not see this event.
//     This guarantees ordering for tests and is acceptable for in-memory.
//   - Each subscriber receives every event exactly once.
//   - Subscribers' handlers MUST NOT block indefinitely; the bus does not
//     enforce a timeout. (Production impl in S5 will.)
//   - Unsubscribe is idempotent.
type EventBus interface {
	Publish(event Event) error
	Subscribe(handler Handler) Unsubscribe
}

// Handler receives one event at a time. Implementations must be
// concurrency-safe — the in-memory bus delivers events sequentially per
// Publish call but parallel Publishes can produce parallel deliveries.
type Handler func(Event)

// Unsubscribe is returned by Subscribe; calling it removes the handler.
// Idempotent — calling more than once is a no-op.
type Unsubscribe func()

// ─────────────────────────────────────────────────────────────────────────
// In-memory implementation
// ─────────────────────────────────────────────────────────────────────────

// MemoryBus is an in-process EventBus. Suitable for unit tests, dev mode,
// and single-process deployments. NOT suitable for multi-replica production.
type MemoryBus struct {
	mu          sync.RWMutex
	subscribers map[uint64]Handler
	nextID      atomic.Uint64
	closed      atomic.Bool
}

// NewMemoryBus returns a ready-to-use in-memory bus.
func NewMemoryBus() *MemoryBus {
	return &MemoryBus{
		subscribers: make(map[uint64]Handler),
	}
}

// Publish delivers event to every subscriber present at call time.
// Returns ErrBusClosed if Close was called.
func (b *MemoryBus) Publish(event Event) error {
	if b.closed.Load() {
		return ErrBusClosed
	}
	if event == nil {
		return fmt.Errorf("events: cannot publish nil event")
	}
	b.mu.RLock()
	// Snapshot — handlers may call Subscribe/Unsubscribe within a Publish
	// (re-entrancy); the snapshot avoids deadlock and stale-iteration panics.
	handlers := make([]Handler, 0, len(b.subscribers))
	for _, h := range b.subscribers {
		handlers = append(handlers, h)
	}
	b.mu.RUnlock()
	for _, h := range handlers {
		h(event)
	}
	return nil
}

// Subscribe registers handler and returns an Unsubscribe closure.
func (b *MemoryBus) Subscribe(handler Handler) Unsubscribe {
	if handler == nil {
		// Returning a no-op Unsubscribe avoids a separate error return for
		// such an obvious programmer error; tests catch it via a panic.
		panic("events: cannot subscribe nil handler")
	}
	id := b.nextID.Add(1)
	b.mu.Lock()
	b.subscribers[id] = handler
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, id)
			b.mu.Unlock()
		})
	}
}

// Len returns the current subscriber count. Test-only helper.
func (b *MemoryBus) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Close prevents further Publish and Subscribe. Safe to call once. Existing
// subscribers are kept for inspection.
func (b *MemoryBus) Close() error {
	b.closed.Store(true)
	return nil
}

// ErrBusClosed is returned by Publish on a closed bus.
var ErrBusClosed = fmt.Errorf("events: bus is closed")

// ─────────────────────────────────────────────────────────────────────────
// Convenience constructors
// ─────────────────────────────────────────────────────────────────────────

// NowUTC returns the current UTC time, useful for producers building events.
func NowUTC() time.Time { return time.Now().UTC() }

// NewBase builds a baseEvent with the current UTC time. Embedded in variants.
func NewBase(taskID TaskID) baseEvent {
	return baseEvent{TaskID: taskID, When: NowUTC()}
}
