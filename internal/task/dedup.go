// Dedup — idempotency wrapper layer (per ADR-006 / S5-WORKER-DESIGN.md §8).
//
// The wrapper layer is the AUTHORITATIVE dedup mechanism. Adapters
// forward the IdempotencyKey as a hint to upstream (defense in depth),
// but the (idempotency_key → task) mapping in our DB is what actually
// guarantees one-task-per-key semantics.
//
// Idempotency key formula (locked per BLUEPRINT.md S5 Tasks #8):
//
//	sha256( account_id ":" model_key ":" canonical_params_json ":" floor(now/60s) )
//
// 60s bucket: same user retrying same params within 60s collapses to
// one task. After 60s, a new task is allowed (rare legitimate re-run).
//
// canonical_params_json: deterministic JSON encoding (sorted keys, no
// extraneous whitespace) so {"a":1,"b":2} and {"b":2, "a":1} hash
// identically. CanonicalizeParams handles this.
//
// SubmitOrDedup is the entry point for /v1/generations:
//   1. Compute idempotency key (caller-provided or auto-derived)
//   2. Try Repo.Create with the key
//   3. If ErrIdempotentReplay, return the existing task
//   4. Otherwise return the new task
//
// The "atomic INSERT-OR-RETURN existing" sketch in S5-WORKER-DESIGN.md §8
// is implemented here as Create+catch — equivalent on a UNIQUE-indexed
// column and portable across Postgres / SQLite without ON CONFLICT
// dialect divergence.

package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// IdempotencyBucketSeconds is the bucket size in seconds for the
// idempotency key formula. 60 per blueprint S5 #8.
const IdempotencyBucketSeconds int64 = 60

// SubmitOrDedupParams is the input to SubmitOrDedup.
type SubmitOrDedupParams struct {
	AccountID         string
	ModelKey          adapter.ModelKey
	ProviderKey       adapter.ProviderKey
	Params            adapter.Params
	HeldAmount        adapter.CostUSD
	SLATimeout        time.Duration
	OverrideKey       string // optional caller-supplied idempotency key
	NowFn             func() time.Time
}

// SubmitOrDedupResult bundles the task with a "was this a dedup hit" flag.
type SubmitOrDedupResult struct {
	Task       *Task
	IsExisting bool // true if an existing task was returned (dedup hit)
}

// SubmitOrDedup is the authoritative idempotency entry point.
//
// Returns the existing task with IsExisting=true on a dedup hit. Returns
// a freshly-created task with IsExisting=false on a new task. Any other
// error is a fault.
func SubmitOrDedup(ctx context.Context, repo *Repo, p SubmitOrDedupParams) (*SubmitOrDedupResult, error) {
	if repo == nil {
		return nil, errors.New("dedup: nil repo")
	}
	if p.NowFn == nil {
		p.NowFn = func() time.Time { return time.Now().UTC() }
	}
	canonical, err := CanonicalizeParams(p.Params)
	if err != nil {
		return nil, fmt.Errorf("dedup: canonicalize params: %w", err)
	}
	key := p.OverrideKey
	if key == "" {
		key = ComputeIdempotencyKey(p.AccountID, p.ModelKey, canonical, p.NowFn())
	}
	t, err := repo.Create(ctx, NewTaskParams{
		AccountID:      p.AccountID,
		ModelKey:       p.ModelKey,
		ProviderKey:    p.ProviderKey,
		ParamsJSON:     canonical,
		IdempotencyKey: key,
		HeldAmount:     p.HeldAmount,
		SLATimeout:     p.SLATimeout,
	})
	if err == nil {
		return &SubmitOrDedupResult{Task: t, IsExisting: false}, nil
	}
	if errors.Is(err, ErrIdempotentReplay) && t != nil {
		return &SubmitOrDedupResult{Task: t, IsExisting: true}, nil
	}
	return nil, err
}

// ComputeIdempotencyKey returns the SHA-256 hex of the canonical input.
//
// Inputs:
//   - accountID: e.g. user_42 (or org_X for multi-tenant per ADR-013)
//   - modelKey:  e.g. "flux-pro-1.1"
//   - canonicalParams: bytes of CanonicalizeParams(params)
//   - now: the time used for bucketing
func ComputeIdempotencyKey(accountID string, modelKey adapter.ModelKey, canonicalParams []byte, now time.Time) string {
	bucket := now.UTC().Unix() / IdempotencyBucketSeconds
	h := sha256.New()
	h.Write([]byte(accountID))
	h.Write([]byte{':'})
	h.Write([]byte(string(modelKey)))
	h.Write([]byte{':'})
	h.Write(canonicalParams)
	h.Write([]byte{':'})
	h.Write([]byte(fmt.Sprintf("%d", bucket)))
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalizeParams returns a deterministic JSON encoding of params:
//   - object keys sorted alphabetically
//   - no extraneous whitespace
//   - nested objects/arrays recursed into
//
// This guarantees that two equivalent param objects (different key
// order, different float formatting) produce the same bytes — which is
// what the idempotency hash depends on.
func CanonicalizeParams(p adapter.Params) ([]byte, error) {
	if p == nil {
		return []byte("{}"), nil
	}
	return marshalCanonical(map[string]any(p))
}

// marshalCanonical is the recursive canonical encoder.
//
// We DON'T use encoding/json's default — Go's json.Marshal sorts map
// keys but does not control float / string nuances we care about. The
// implementation here is intentionally narrow: maps → sorted keys,
// arrays → preserved order, leaves → encoding/json.Marshal.
func marshalCanonical(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			sb.Write(kb)
			sb.WriteByte(':')
			vb, err := marshalCanonical(t[k])
			if err != nil {
				return nil, err
			}
			sb.Write(vb)
		}
		sb.WriteByte('}')
		return []byte(sb.String()), nil
	case []any:
		var sb strings.Builder
		sb.WriteByte('[')
		for i, el := range t {
			if i > 0 {
				sb.WriteByte(',')
			}
			eb, err := marshalCanonical(el)
			if err != nil {
				return nil, err
			}
			sb.Write(eb)
		}
		sb.WriteByte(']')
		return []byte(sb.String()), nil
	default:
		return json.Marshal(t)
	}
}
