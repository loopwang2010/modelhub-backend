// Package task defines the modelhub task lifecycle finite state machine.
//
// Per ADR-004, every generation request is modeled as a Task that walks
// this FSM. Wallet operations and asset hosting are coupled to specific
// transitions (see ADR-010, ADR-011). The actual worker, persistence,
// and event-bus integration ship in S5; this file only owns the state
// vocabulary and transition table.
//
// v2 (2026-05-07): post-adversarial-review revisions:
//   - C1: documented Submitted as logical "upstream-acknowledged" marker even
//     for sync inline results; no separate StateInline state needed
//   - C2: added StateHeld → StateCancelled edge for cancel-during-Submit race
//   - C5: AssetLost is no longer terminal — added recovery edge to Succeeded
//     so the asset worker can re-host hours later if upstream URL recovers
package task

import (
	"errors"
	"fmt"
)

// TaskState is the canonical state of a generation task throughout its lifecycle.
//
// Lifecycle overview:
//
//	Created → Held → Submitted → Running? → Succeeded → Settled
//	            ↓        ↓          ↓          ↓
//	            ↓        ↓          ↓          → AssetLost ⇄ Succeeded (recovery)
//	            ↓        ↓          ↓          → Failed/TimedOut/Cancelled (refund)
//	            ↓        ↓          → Failed/TimedOut/Cancelled (refund)
//	            ↓        → Failed (refund) / Cancelled (refund)
//	            → Failed (refund) / Cancelled (refund)
//
// Sync model convention (C1): when an upstream returns a result inline (no
// async ref), the worker still walks Created → Held → Submitted → Succeeded.
// `StateSubmitted` here means "upstream has acknowledged the request" — for
// sync, the Submit call's 200-with-result IS the acknowledgement. We do NOT
// add a separate StateInline because the bookkeeping (timestamps, audit log)
// is identical to async; the only difference is whether Poll is called next.
type TaskState string

const (
	// StateCreated is the initial state. Task row exists; nothing else has happened.
	StateCreated TaskState = "created"

	// StateHeld means the wallet has reserved (held) credit in escrow.
	// EstimateCost has run; the user has enough balance.
	// Next: StateSubmitted (Submit succeeded), StateFailed (Submit error),
	// or StateCancelled (user cancelled before Submit completed — C2 race fix).
	StateHeld TaskState = "held"

	// StateSubmitted means the upstream provider has acknowledged the request.
	// For sync models, this state is transient — we go straight to StateSucceeded.
	// For async, we sit here until poll/webhook reports progress.
	StateSubmitted TaskState = "submitted"

	// StateRunning means upstream is actively producing output (async only).
	// Some upstreams skip directly to Succeeded without a Running phase; that's fine.
	StateRunning TaskState = "running"

	// StateSucceeded means upstream produced an output, but we haven't yet
	// hosted the asset on our CDN. Settle waits for AssetHosted (per ADR-010).
	StateSucceeded TaskState = "succeeded"

	// StateAssetLost means upstream succeeded but our asset download failed
	// after the worker's retry budget. User has received a partial refund.
	// NOT terminal (C5 fix): the asset worker can re-attempt hours later if
	// the upstream URL becomes accessible again, and on success transitions
	// back to StateSucceeded for re-Settle.
	StateAssetLost TaskState = "asset_lost"

	// StateSettled is the terminal happy-path state. Wallet has moved escrow
	// → revenue. Difference between held and actual is refunded (already accounted).
	StateSettled TaskState = "settled"

	// StateFailed is a terminal error state. Wallet has refunded the escrow.
	StateFailed TaskState = "failed"

	// StateTimedOut means the task exceeded our SLA without producing a result.
	// Wallet refunds (full or partial depending on upstream cost incurred).
	StateTimedOut TaskState = "timed_out"

	// StateCancelled means the user (or admin) cancelled the task before terminal.
	// Wallet refunds. Adapter.Cancel() may have been called.
	StateCancelled TaskState = "cancelled"
)

// IsTerminal reports whether s is a final state — no further transitions allowed.
// AssetLost is NOT terminal (C5 fix) because the asset worker may recover later.
func (s TaskState) IsTerminal() bool {
	switch s {
	case StateSettled, StateFailed, StateTimedOut, StateCancelled:
		return true
	default:
		return false
	}
}

// MustRefund reports whether transitioning into s requires the wallet to refund the held escrow.
// Note StateAssetLost is a partial refund (handled separately, not a full refund).
func (s TaskState) MustRefund() bool {
	switch s {
	case StateFailed, StateTimedOut, StateCancelled:
		return true
	default:
		return false
	}
}

// MustSettle reports whether transitioning into s requires the wallet to settle the held escrow.
// Re-entering StateSucceeded from StateAssetLost (recovery path) also triggers Settle.
func (s TaskState) MustSettle() bool {
	return s == StateSettled
}

// MustPartialRefund reports whether transitioning into s requires partial refund (per ADR-010).
func (s TaskState) MustPartialRefund() bool {
	return s == StateAssetLost
}

// ─────────────────────────────────────────────────────────────────────────
// Transition table — the source of truth for legal state moves.
// ─────────────────────────────────────────────────────────────────────────

// transitions defines which (from → to) edges are valid.
// Any transition NOT listed here is invalid. AssertTransition returns
// ErrIllegalTransition for unlisted moves; callers MUST treat that as a bug.
//
// This table is exhaustively tested in state_test.go.
var transitions = map[TaskState][]TaskState{
	StateCreated: {
		StateHeld,      // hold succeeded
		StateFailed,    // hold failed (insufficient balance, manifest disabled, etc.)
		StateCancelled, // user cancelled before any work began
	},
	StateHeld: {
		StateSubmitted,
		StateFailed,    // Submit returned non-2xx, or sync inline-failure
		StateCancelled, // C2 fix: user cancelled mid-Submit (race)
	},
	StateSubmitted: {
		StateRunning,
		StateSucceeded, // sync result returned inline OR async completed faster than Running detection
		StateFailed,
		StateTimedOut,
		StateCancelled,
	},
	StateRunning: {
		StateSucceeded,
		StateFailed,
		StateTimedOut,
		StateCancelled,
	},
	StateSucceeded: {
		StateSettled,   // AssetHosted received (happy path)
		StateAssetLost, // asset worker exhausted retries
	},
	StateAssetLost: {
		StateSucceeded, // C5 fix: asset worker recovers later (re-host succeeded)
		// NOTE: cannot directly settle from AssetLost — must transition through
		// Succeeded so the worker sees a consistent "have asset" precondition
	},
	// Terminal states have no outbound transitions.
	StateSettled:   nil,
	StateFailed:    nil,
	StateTimedOut:  nil,
	StateCancelled: nil,
}

// CanTransition reports whether (from → to) is a legal move.
func CanTransition(from, to TaskState) bool {
	allowed, ok := transitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// ErrIllegalTransition is returned by enforcers when a caller attempts an
// invalid state move. Worker code MUST treat this as a programmer error
// (i.e., a bug), not a runtime condition to recover from gracefully.
type ErrIllegalTransition struct {
	From, To TaskState
}

func (e *ErrIllegalTransition) Error() string {
	return fmt.Sprintf("task: illegal state transition %q → %q", e.From, e.To)
}

// AssertTransition returns nil if the move is legal, ErrIllegalTransition otherwise.
// Higher layers should use this in any code that mutates a task's state.
func AssertTransition(from, to TaskState) error {
	if CanTransition(from, to) {
		return nil
	}
	return &ErrIllegalTransition{From: from, To: to}
}

// ErrTerminalState is returned when a caller attempts to advance a task that
// is already in a terminal state.
var ErrTerminalState = errors.New("task: already in terminal state")

// AllStates returns every defined TaskState in a fixed order.
// Used by exhaustive tests and admin tooling.
func AllStates() []TaskState {
	return []TaskState{
		StateCreated, StateHeld, StateSubmitted, StateRunning,
		StateSucceeded, StateAssetLost,
		StateSettled, StateFailed, StateTimedOut, StateCancelled,
	}
}
