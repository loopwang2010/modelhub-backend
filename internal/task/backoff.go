// Backoff — exponential backoff with jitter for poll scheduling.
//
// Per AP-3, the worker MUST never use constant-interval polling. Every
// "still pending" Poll result schedules the next attempt via
// NextPollAfter, which produces 5s → 10s → 20s → 40s → 60s (capped)
// with ±25% jitter.
//
// Why no time.Sleep in production code:
//   - The worker advances FSM state via DB writes; sleep durations are
//     persisted as `next_poll_after`, not held in goroutine memory.
//   - This makes the worker safely restartable: if a goroutine crashes
//     mid-Poll, the next iteration after restart picks up at the correct
//     scheduled time (or sooner if the reconciler clears the field).
//
// Webhook-capable providers still poll, but at the slow end of the
// curve (60s minimum) — webhooks deliver fast updates, polling is the
// safety net.

package task

import (
	"math/rand"
	"time"
)

// MinPollInterval is the floor for any scheduled poll. New tasks first
// poll after this duration.
const MinPollInterval = 5 * time.Second

// MaxPollInterval is the ceiling for any scheduled poll.
const MaxPollInterval = 60 * time.Second

// JitterFraction is the fraction of base added/subtracted as jitter.
// 0.25 produces ±25% of base.
const JitterFraction = 0.25

// rngFunc is the random source used by NextPollAfter. Tests inject a
// deterministic seed via SetRNG; production uses rand.Float64.
var rngFunc = rand.Float64

// SetRNG installs a custom random source (for tests). Returns a closure
// that restores the previous source.
func SetRNG(fn func() float64) func() {
	prev := rngFunc
	rngFunc = fn
	return func() { rngFunc = prev }
}

// NextPollAfter returns the absolute time the next poll should fire,
// given the current poll attempt count (0-indexed).
//
// Formula:
//
//	base = min(MinPollInterval << attempt, MaxPollInterval)
//	jitter = (rand_0_1 - 0.5) * 2 * JitterFraction * base   // ±25%
//	return now + base + jitter
//
// Effective curve:
//
//	attempt 0 → 5s   ± 25%   = 3.75s..6.25s
//	attempt 1 → 10s  ± 25%   = 7.5s..12.5s
//	attempt 2 → 20s  ± 25%   = 15s..25s
//	attempt 3 → 40s  ± 25%   = 30s..50s
//	attempt 4 → 60s  ± 25%   = 45s..75s (capped at 60s before jitter)
//	attempt N→ 60s  ± 25%   = 45s..75s
func NextPollAfter(attempt int) time.Time {
	return time.Now().UTC().Add(BackoffDuration(attempt))
}

// BackoffDuration computes ONLY the duration component of NextPollAfter.
// Useful for tests that want to assert curve shape without time.Now()
// noise.
func BackoffDuration(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// Compute base = MinPollInterval * 2^attempt with capping.
	shift := attempt
	if shift > 10 {
		shift = 10 // prevent overflow on absurd inputs
	}
	base := MinPollInterval << shift
	if base > MaxPollInterval {
		base = MaxPollInterval
	}
	// Jitter: (rng - 0.5) * 2 * 0.25 * base = (-0.25..+0.25) * base
	jitter := time.Duration((rngFunc() - 0.5) * 2 * JitterFraction * float64(base))
	out := base + jitter
	if out < 0 {
		// Defensive: should never happen with the formula above, but
		// makes the function total.
		out = 0
	}
	return out
}

// SlowPollNext is used for webhook-capable providers — clamps to the
// slow end of the curve regardless of attempt count.
func SlowPollNext() time.Time {
	return time.Now().UTC().Add(slowPollDuration())
}

func slowPollDuration() time.Duration {
	base := MaxPollInterval
	jitter := time.Duration((rngFunc() - 0.5) * 2 * JitterFraction * float64(base))
	out := base + jitter
	if out < 0 {
		out = 0
	}
	return out
}
