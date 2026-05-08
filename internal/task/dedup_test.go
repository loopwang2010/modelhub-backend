// Idempotency / dedup tests — AP-12 enforcement.

package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// TestCanonicalizeParams_StableOrder asserts that two equivalent param
// objects with different key ordering produce identical bytes.
func TestCanonicalizeParams_StableOrder(t *testing.T) {
	a := adapter.Params{"b": 2, "a": 1}
	b := adapter.Params{"a": 1, "b": 2}
	ab, err := CanonicalizeParams(a)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	bb, err := CanonicalizeParams(b)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if string(ab) != string(bb) {
		t.Errorf("canonical mismatch:\n a=%s\n b=%s", ab, bb)
	}
}

// TestCanonicalizeParams_NestedSorted asserts nested object keys are
// sorted recursively.
func TestCanonicalizeParams_NestedSorted(t *testing.T) {
	p := adapter.Params{
		"z": 1,
		"a": map[string]any{
			"y": 2,
			"x": 3,
		},
		"arr": []any{map[string]any{"b": 1, "a": 2}},
	}
	got, err := CanonicalizeParams(p)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want := `{"a":{"x":3,"y":2},"arr":[{"a":2,"b":1}],"z":1}`
	if string(got) != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

// TestComputeIdempotencyKey_BucketStable asserts the same inputs in the
// same 60s bucket produce the same key, but different buckets diverge.
func TestComputeIdempotencyKey_BucketStable(t *testing.T) {
	canon, _ := CanonicalizeParams(adapter.Params{"prompt": "hello"})
	t0 := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second) // same bucket
	t2 := t0.Add(61 * time.Second) // different bucket
	k0 := ComputeIdempotencyKey("acct", "model", canon, t0)
	k1 := ComputeIdempotencyKey("acct", "model", canon, t1)
	k2 := ComputeIdempotencyKey("acct", "model", canon, t2)
	if k0 != k1 {
		t.Errorf("same bucket should match: %s vs %s", k0, k1)
	}
	if k0 == k2 {
		t.Errorf("different bucket should diverge: %s == %s", k0, k2)
	}
}

// TestSubmitOrDedup_NewTaskCreated asserts a fresh idempotency key
// produces a new task with IsExisting=false.
func TestSubmitOrDedup_NewTaskCreated(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()

	res, err := SubmitOrDedup(context.Background(), repo, SubmitOrDedupParams{
		AccountID:   "acct1",
		ModelKey:    "model-x",
		ProviderKey: "mock-async",
		Params:      adapter.Params{"prompt": "hello"},
		HeldAmount:  60_000,
		SLATimeout:  5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("submit-or-dedup: %v", err)
	}
	if res.IsExisting {
		t.Errorf("expected new task; got IsExisting=true")
	}
	if res.Task.ID == "" {
		t.Errorf("expected non-empty task ID")
	}
}

// TestSubmitOrDedup_ReplayReturnsExisting asserts a second call with
// identical params (same bucket) returns the existing task.
func TestSubmitOrDedup_ReplayReturnsExisting(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()

	now := time.Now().UTC()
	clock := func() time.Time { return now }
	p := SubmitOrDedupParams{
		AccountID:   "acct1",
		ModelKey:    "model-x",
		ProviderKey: "mock-async",
		Params:      adapter.Params{"prompt": "hello"},
		HeldAmount:  60_000,
		SLATimeout:  5 * time.Minute,
		NowFn:       clock,
	}
	first, err := SubmitOrDedup(context.Background(), repo, p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := SubmitOrDedup(context.Background(), repo, p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.IsExisting {
		t.Errorf("expected dedup hit; got IsExisting=false")
	}
	if second.Task.ID != first.Task.ID {
		t.Errorf("expected same task ID; got %s vs %s", first.Task.ID, second.Task.ID)
	}
}

// TestSubmitOrDedup_DifferentBucketCreatesNew asserts that crossing the
// 60s bucket boundary allows a new task.
func TestSubmitOrDedup_DifferentBucketCreatesNew(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()

	now := time.Now().UTC()
	t1 := func() time.Time { return now }
	t2 := func() time.Time { return now.Add(61 * time.Second) }
	makeParams := func(clock func() time.Time) SubmitOrDedupParams {
		return SubmitOrDedupParams{
			AccountID:   "acct1",
			ModelKey:    "model-x",
			ProviderKey: "mock-async",
			Params:      adapter.Params{"prompt": "hello"},
			HeldAmount:  60_000,
			SLATimeout:  5 * time.Minute,
			NowFn:       clock,
		}
	}
	first, err := SubmitOrDedup(context.Background(), repo, makeParams(t1))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := SubmitOrDedup(context.Background(), repo, makeParams(t2))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.IsExisting {
		t.Errorf("expected new task in different bucket")
	}
	if second.Task.ID == first.Task.ID {
		t.Errorf("different buckets should produce different tasks")
	}
}

// TestRepo_Create_IdempotencyConflictReturnsExisting asserts that
// Repo.Create with an existing key returns ErrIdempotentReplay and the
// existing task pointer.
func TestRepo_Create_IdempotencyConflictReturnsExisting(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	p := newTestTaskParams("dup")
	first, err := repo.Create(context.Background(), p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same idempotency key — should return existing.
	second, err := repo.Create(context.Background(), p)
	if !errors.Is(err, ErrIdempotentReplay) {
		t.Fatalf("expected ErrIdempotentReplay, got %v", err)
	}
	if second == nil || second.ID != first.ID {
		t.Errorf("expected existing task ID %s, got %v", first.ID, second)
	}
}
