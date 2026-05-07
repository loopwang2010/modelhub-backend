// Backoff curve tests — AP-3 enforcement.
//
// What we assert:
//   1. The base intervals at attempts 0..4 are 5s, 10s, 20s, 40s, 60s.
//      (60s is the cap; attempts ≥4 stay there.)
//   2. Jitter is bounded ±25% of base (never wider).
//   3. 100 simulated polls all-pending produces durations following the
//      curve, never a constant interval — covers the AP-3 anti-pattern
//      "polling without exponential backoff".
//   4. SlowPollNext stays in the [45s, 75s] band.

package task

import (
	"math"
	"testing"
	"time"
)

// TestBackoffDuration_BaseProgression covers the canonical 5/10/20/40/60
// curve with the random source pinned to 0.5 (= zero jitter).
func TestBackoffDuration_BaseProgression(t *testing.T) {
	restore := SetRNG(func() float64 { return 0.5 })
	defer restore()

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 60 * time.Second},
		{5, 60 * time.Second},
		{10, 60 * time.Second},
	}
	for _, tc := range cases {
		got := BackoffDuration(tc.attempt)
		if got != tc.want {
			t.Errorf("attempt=%d: got %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// TestBackoffDuration_JitterBounded asserts that the jittered output
// is always within ±25% of the base for every attempt.
func TestBackoffDuration_JitterBounded(t *testing.T) {
	for attempt := 0; attempt <= 6; attempt++ {
		base := time.Duration(MinPollInterval) << attempt
		if base > MaxPollInterval {
			base = MaxPollInterval
		}
		minAllowed := time.Duration(float64(base) * (1 - JitterFraction))
		maxAllowed := time.Duration(float64(base) * (1 + JitterFraction))
		// Sample with deterministic RNG values pegged at extremes.
		for _, r := range []float64{0.0, 0.25, 0.5, 0.75, 1.0} {
			restore := SetRNG(func() float64 { return r })
			d := BackoffDuration(attempt)
			restore()
			if d < minAllowed-1*time.Millisecond || d > maxAllowed+1*time.Millisecond {
				t.Errorf("attempt=%d r=%v: %v outside [%v, %v]", attempt, r, d, minAllowed, maxAllowed)
			}
		}
	}
}

// TestBackoffDuration_NotConstant simulates 100 poll attempts and
// asserts the durations follow the curve — not a constant interval.
// This is the AP-3 guard: a regression that hard-codes 30s polling
// would fail this test.
func TestBackoffDuration_NotConstant(t *testing.T) {
	// Deterministic RNG returning a sweeping sequence so jitter varies.
	idx := 0
	values := []float64{0.1, 0.3, 0.5, 0.7, 0.9}
	restore := SetRNG(func() float64 {
		v := values[idx%len(values)]
		idx++
		return v
	})
	defer restore()

	durations := make([]time.Duration, 100)
	for i := range durations {
		// attempt = 0 first; subsequent attempts grow.
		durations[i] = BackoffDuration(i)
	}

	// Distinct durations in the first 5 attempts.
	seen := map[time.Duration]bool{}
	for i := 0; i < 5; i++ {
		seen[durations[i]] = true
	}
	if len(seen) < 3 {
		t.Fatalf("expected at least 3 distinct durations in first 5 attempts; got %v", durations[:5])
	}

	// Every duration falls within [3.75s, 75s] (min jitter floor for
	// attempt 0, max jitter ceiling for capped 60s).
	for i, d := range durations {
		if d < 3*time.Second || d > 80*time.Second {
			t.Errorf("attempt %d: %v outside expected envelope", i, d)
		}
	}

	// Attempts 4..N must all sit inside the capped 60s ±25% band.
	for i := 4; i < len(durations); i++ {
		d := durations[i]
		if d < 45*time.Second-1*time.Millisecond || d > 75*time.Second+1*time.Millisecond {
			t.Errorf("attempt %d: %v not in [45s, 75s] (cap with ±25%% jitter)", i, d)
		}
	}
}

// TestSlowPollNext_StaysInSlowBand asserts SlowPollNext returns
// timestamps roughly 45..75 seconds in the future.
func TestSlowPollNext_StaysInSlowBand(t *testing.T) {
	restore := SetRNG(func() float64 { return 0.5 })
	defer restore()
	now := time.Now().UTC()
	at := SlowPollNext()
	delta := at.Sub(now)
	// Allow 1s slack for time elapsed during the call.
	if delta < 59*time.Second || delta > 61*time.Second {
		t.Errorf("zero-jitter SlowPollNext: %v not ~60s", delta)
	}
}

// TestNextPollAfter_AbsoluteTime asserts NextPollAfter returns a future
// time (not a duration).
func TestNextPollAfter_AbsoluteTime(t *testing.T) {
	restore := SetRNG(func() float64 { return 0.5 })
	defer restore()
	at := NextPollAfter(0)
	if at.Before(time.Now().UTC()) {
		t.Errorf("NextPollAfter returned past time: %v", at)
	}
	delta := time.Until(at)
	if math.Abs(float64(delta-5*time.Second)) > float64(time.Second) {
		t.Errorf("NextPollAfter(0) = +%v, want ~+5s", delta)
	}
}
