// Exhaustive tests for the task FSM. Verifies:
//   - every legal transition listed in `transitions` is accepted by
//     CanTransition / AssertTransition
//   - every other (from, to) pair across AllStates() × AllStates() is
//     rejected with ErrIllegalTransition
//   - IsTerminal / MustRefund / MustSettle / MustPartialRefund predicates
//     return the spec-defined value for every state
//   - the C5 recovery edge AssetLost → Succeeded is reachable
//   - terminal states have no outbound transitions
//
// The goal is ≥95% coverage of state.go AND no unexpected reachability:
// any transition not explicitly enumerated below MUST NOT pass.
package task

import (
	"errors"
	"testing"
)

// expectedTransitions is the canonical set of (from → to) edges that MUST
// be legal. This duplicates the transitions table on purpose — the test is
// the spec; if implementation drifts, this fails. Adding/removing a row here
// is a deliberate spec change.
var expectedTransitions = map[TaskState]map[TaskState]bool{
	StateCreated: {
		StateHeld:      true,
		StateFailed:    true,
		StateCancelled: true,
	},
	StateHeld: {
		StateSubmitted: true,
		StateFailed:    true,
		StateCancelled: true, // C2
	},
	StateSubmitted: {
		StateRunning:   true,
		StateSucceeded: true,
		StateFailed:    true,
		StateTimedOut:  true,
		StateCancelled: true,
	},
	StateRunning: {
		StateSucceeded: true,
		StateFailed:    true,
		StateTimedOut:  true,
		StateCancelled: true,
	},
	StateSucceeded: {
		StateSettled:   true,
		StateAssetLost: true,
	},
	StateAssetLost: {
		StateSucceeded: true, // C5 recovery
	},
	// Terminal states — no outbound edges.
	StateSettled:   {},
	StateFailed:    {},
	StateTimedOut:  {},
	StateCancelled: {},
}

// TestCanTransition_ExhaustiveCrossProduct verifies CanTransition's answer
// against expectedTransitions for every (from, to) pair across AllStates().
// This is the cross-product the parent agent specified.
func TestCanTransition_ExhaustiveCrossProduct(t *testing.T) {
	all := AllStates()
	for _, from := range all {
		for _, to := range all {
			want := expectedTransitions[from][to]
			got := CanTransition(from, to)
			if got != want {
				t.Errorf("CanTransition(%q → %q) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestAssertTransition_ExhaustiveCrossProduct: AssertTransition agrees with
// CanTransition. Allowed → nil. Disallowed → *ErrIllegalTransition with
// matching from/to.
func TestAssertTransition_ExhaustiveCrossProduct(t *testing.T) {
	all := AllStates()
	for _, from := range all {
		for _, to := range all {
			err := AssertTransition(from, to)
			want := expectedTransitions[from][to]
			if want {
				if err != nil {
					t.Errorf("AssertTransition(%q → %q) returned %v; expected nil", from, to, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("AssertTransition(%q → %q) returned nil; expected ErrIllegalTransition", from, to)
				continue
			}
			var illegal *ErrIllegalTransition
			if !errors.As(err, &illegal) {
				t.Errorf("AssertTransition(%q → %q) returned %T %v; expected *ErrIllegalTransition", from, to, err, err)
				continue
			}
			if illegal.From != from || illegal.To != to {
				t.Errorf("ErrIllegalTransition has From=%q To=%q; want From=%q To=%q", illegal.From, illegal.To, from, to)
			}
			// Error() must include both states for diagnosis.
			msg := illegal.Error()
			if !contains(msg, string(from)) || !contains(msg, string(to)) {
				t.Errorf("ErrIllegalTransition.Error() = %q; expected to include %q and %q", msg, from, to)
			}
		}
	}
}

// TestUnknownFromState_RejectsAllTargets: a state not in transitions table
// (synthetic "unknown") MUST be rejected by CanTransition / AssertTransition
// for every conceivable target.
func TestUnknownFromState_RejectsAllTargets(t *testing.T) {
	const unknown TaskState = "unknown_state_xyz"
	for _, to := range AllStates() {
		if CanTransition(unknown, to) {
			t.Errorf("CanTransition(%q → %q) returned true; unknown source should be rejected", unknown, to)
		}
		if err := AssertTransition(unknown, to); err == nil {
			t.Errorf("AssertTransition(%q → %q) returned nil; unknown source should be rejected", unknown, to)
		}
	}
}

// TestIsTerminal_AllStates: enumerated.
func TestIsTerminal_AllStates(t *testing.T) {
	cases := map[TaskState]bool{
		StateCreated:   false,
		StateHeld:      false,
		StateSubmitted: false,
		StateRunning:   false,
		StateSucceeded: false,
		StateAssetLost: false, // C5: NOT terminal
		StateSettled:   true,
		StateFailed:    true,
		StateTimedOut:  true,
		StateCancelled: true,
	}
	for _, s := range AllStates() {
		want, ok := cases[s]
		if !ok {
			t.Fatalf("test case missing for state %q — update cases map", s)
		}
		if got := s.IsTerminal(); got != want {
			t.Errorf("(%q).IsTerminal() = %v, want %v", s, got, want)
		}
	}
}

func TestMustRefund_AllStates(t *testing.T) {
	cases := map[TaskState]bool{
		StateCreated:   false,
		StateHeld:      false,
		StateSubmitted: false,
		StateRunning:   false,
		StateSucceeded: false,
		StateAssetLost: false, // partial refund, not full
		StateSettled:   false,
		StateFailed:    true,
		StateTimedOut:  true,
		StateCancelled: true,
	}
	for _, s := range AllStates() {
		want, ok := cases[s]
		if !ok {
			t.Fatalf("test case missing for state %q", s)
		}
		if got := s.MustRefund(); got != want {
			t.Errorf("(%q).MustRefund() = %v, want %v", s, got, want)
		}
	}
}

func TestMustSettle_AllStates(t *testing.T) {
	for _, s := range AllStates() {
		want := s == StateSettled
		if got := s.MustSettle(); got != want {
			t.Errorf("(%q).MustSettle() = %v, want %v", s, got, want)
		}
	}
}

func TestMustPartialRefund_AllStates(t *testing.T) {
	for _, s := range AllStates() {
		want := s == StateAssetLost
		if got := s.MustPartialRefund(); got != want {
			t.Errorf("(%q).MustPartialRefund() = %v, want %v", s, got, want)
		}
	}
}

// TestRecoveryEdge_AssetLost_To_Succeeded: explicitly tests the C5 fix.
func TestRecoveryEdge_AssetLost_To_Succeeded(t *testing.T) {
	if !CanTransition(StateAssetLost, StateSucceeded) {
		t.Fatal("AssetLost → Succeeded recovery edge missing (C5 fix regression)")
	}
	if err := AssertTransition(StateAssetLost, StateSucceeded); err != nil {
		t.Fatalf("AssertTransition AssetLost → Succeeded returned %v; want nil", err)
	}
	// Round-trip: from Succeeded we may settle.
	if !CanTransition(StateSucceeded, StateSettled) {
		t.Fatal("Succeeded → Settled missing")
	}
}

// TestTerminalStates_HaveNoOutbound: every terminal state rejects every
// possible outbound move.
func TestTerminalStates_HaveNoOutbound(t *testing.T) {
	terminals := []TaskState{StateSettled, StateFailed, StateTimedOut, StateCancelled}
	for _, from := range terminals {
		for _, to := range AllStates() {
			if CanTransition(from, to) {
				t.Errorf("terminal %q has outbound transition to %q", from, to)
			}
		}
	}
}

// TestAllStates_StableOrder: the function returns exactly the 10 known states
// in a stable order; admin tooling depends on that order.
func TestAllStates_StableOrder(t *testing.T) {
	got := AllStates()
	want := []TaskState{
		StateCreated, StateHeld, StateSubmitted, StateRunning,
		StateSucceeded, StateAssetLost,
		StateSettled, StateFailed, StateTimedOut, StateCancelled,
	}
	if len(got) != len(want) {
		t.Fatalf("AllStates len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllStates[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestErrTerminalState_Sentinel: existence sanity check; callers compare via
// errors.Is.
func TestErrTerminalState_Sentinel(t *testing.T) {
	if ErrTerminalState == nil {
		t.Fatal("ErrTerminalState sentinel must be non-nil")
	}
	if ErrTerminalState.Error() == "" {
		t.Fatal("ErrTerminalState.Error() empty")
	}
	if !errors.Is(ErrTerminalState, ErrTerminalState) {
		t.Fatal("ErrTerminalState fails errors.Is identity check")
	}
}

// TestErrIllegalTransition_AsEnvelope: callers may use errors.As to extract
// from/to fields.
func TestErrIllegalTransition_AsEnvelope(t *testing.T) {
	err := AssertTransition(StateSettled, StateHeld)
	if err == nil {
		t.Fatal("expected error")
	}
	var ill *ErrIllegalTransition
	if !errors.As(err, &ill) {
		t.Fatalf("errors.As failed for %T", err)
	}
	if ill.From != StateSettled || ill.To != StateHeld {
		t.Errorf("From/To mismatch: %v", ill)
	}
}

// contains is a tiny helper to avoid importing strings.Contains for one call.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
