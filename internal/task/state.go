// Package task defines the modelhub task lifecycle finite state machine.
//
// Per ADR-004, every generation request is modeled as a Task that walks
// this FSM. Wallet operations and asset hosting are coupled to specific
// transitions (see ADR-010, ADR-011). The actual worker, persistence,
// and event-bus integration ship in S5; this file only owns the state
// vocabulary and transition table.
package task

import (
	"errors"
	"fmt"
)

// TaskState is the canonical state of a generation task throughout its lifecycle.
//
// Lifecycle overview:
//
//	Created → Held → Submitted → Running → Succeeded → AssetLost? → Settled
//	                                  ↘ Failed / TimedOut / Cancelled → Refunded
//
// Held vs Submitted: Held = wallet escrow created locally; Submitted = upstream
// provider returned 200/202. We MUST NOT advance Created → Submitted in one step
// (per AP-2 anti-pattern in the blueprint).
type TaskState string

const (
	// StateCreated is the initial state. Task row exists; nothing else has happened.
	StateCreated TaskState = "created"

	// StateHeld means the wallet has reserved (held) credit in escrow.
	// EstimateCost has run; the user has enough balance.
	// Next: StateSubmitted (success) or StateFailed (Submit returned an error).
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
	// permanently after retries. User gets a partial refund (compute cost retained,
	// asset cost refunded).
	StateAssetLost TaskState = "asset_lost"

	// StateSettled is the terminal happy-path state. Wallet has moved escrow
	// → revenue. Difference between held and actual is refunded (already accounted).
	StateSettled TaskState = "settled"

	// StateFailed is a terminal error state. Wallet has refunded the escrow.
	StateFailed TaskState = "failed"

	// StateTimedOut means the task exceeded our SLA without producing a result.
	// Wallet refunds (full or partial depending on upstream cost incurred).
	StateTimedOut TaskState = "timed_out"

	// StateCancelled means the user (or admin) cancelled the task before it finished.
	// Wallet refunds. Adapter.Cancel() may have been called.
	StateCancelled TaskState = "cancelled"
)

// IsTerminal reports whether s is a final state — no further transitions allowed.
func (s TaskState) IsTerminal() bool {
	switch s {
	case StateSettled, StateFailed, StateTimedOut, StateCancelled, StateAssetLost:
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
// Any transition NOT listed here is invalid and panics if attempted.
// This table is exhaustively tested in state_test.go.
var transitions = map[TaskState][]TaskState{
	StateCreated: {
		StateHeld,     // hold succeeded
		StateFailed,   // hold failed (insufficient balance, etc.)
	},
	StateHeld: {
		StateSubmitted,
		StateFailed, // Submit returned non-2xx, or sync inline-failure
	},
	StateSubmitted: {
		StateRunning,
		StateSucceeded, // sync result returned inline
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
		StateSettled,    // AssetHosted received
		StateAssetLost,  // asset worker exhausted retries
	},
	// Terminal states have no outbound transitions.
	StateSettled:    nil,
	StateFailed:     nil,
	StateTimedOut:   nil,
	StateCancelled:  nil,
	StateAssetLost:  nil,
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
