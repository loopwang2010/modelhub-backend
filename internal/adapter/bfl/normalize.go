// Result-shape mapping isolated for golden-file unit testing (H3 fix).
// The adapter and its tests both call NormalizeResult; a malformed
// upstream-shape change is caught by the golden-file test, not in production.
//
// AP-1 guard: nothing in this file may be exported as anything other than
// NormalizeResult itself; all BFL-shape struct definitions stay package-private.

package bfl

import (
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// pollResponse is the shape returned by GET <polling_url>. Only the fields
// we actually read are listed — extra fields are ignored by the JSON decoder.
type pollResponse struct {
	Status string         `json:"status"`
	Result *pollResultBag `json:"result,omitempty"`
	// Progress is best-effort; BFL does not document this shape uniformly.
	Progress *float32 `json:"progress,omitempty"`
}

// pollResultBag is the nested `result` object on a Ready response.
//
// "sample" is the public delivery URL; "details" is the human message on
// status=Error. We accept both (mutually exclusive in practice, but a
// permissive struct keeps the decoder happy).
type pollResultBag struct {
	Sample  string         `json:"sample,omitempty"`
	Details string         `json:"details,omitempty"`
	Seed    *int64         `json:"seed,omitempty"`
	Extras  map[string]any `json:"-"`
}

// submitResponse is the shape returned by POST /v1/{model}.
type submitResponse struct {
	ID         string `json:"id"`
	PollingURL string `json:"polling_url"`
}

// upstream "status" string values per S7-S8-API-RESEARCH.md §1.
const (
	statusPending          = "Pending"
	statusReady            = "Ready"
	statusError            = "Error"
	statusContentModerated = "Content Moderated"
	statusTaskNotFound     = "Task not found"
)

// normalizeReadyResult turns a Ready poll response body into our canonical
// shape. Exported as the adapter's NormalizeResult method but kept here for
// direct unit testing.
//
// Returns ErrInvalidParams when the body is malformed or sample URL is empty
// — these signal an upstream contract change, not a user error, and the
// caller should alert ops, not retry.
func normalizeReadyResult(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("bfl: empty response body")
	}
	var pr pollResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, fmt.Errorf("bfl: decode poll body: %w", err)
	}
	if pr.Status != statusReady {
		return nil, fmt.Errorf("bfl: NormalizeResult requires status=%q, got %q", statusReady, pr.Status)
	}
	if pr.Result == nil || pr.Result.Sample == "" {
		return nil, fmt.Errorf("bfl: ready response missing result.sample")
	}
	out := &adapter.NormalizedResult{
		Modality: adapter.ModalityImage,
		Outputs: []adapter.Output{
			{
				Kind: adapter.OutputKindImageURL,
				// AP-19: this URL is upstream-shaped (delivery-eu1.bfl.ai/...).
				// S9.5's asset worker rewrites it to our CDN URL BEFORE any
				// user-facing surface (envelope.go enforces the prefix).
				URL:      pr.Result.Sample,
				MimeType: "image/jpeg",
			},
		},
	}
	meta := map[string]any{
		"upstream_model": string(model),
	}
	if pr.Result.Seed != nil {
		meta["seed"] = *pr.Result.Seed
	}
	out.Metadata = meta
	return out, nil
}

// classifyPollStatus maps a BFL status string + body to our PollResult.
// Splitting this from normalizeReadyResult keeps the success path testable
// in isolation against minimal fixtures.
func classifyPollStatus(model adapter.ModelKey, raw []byte) (adapter.PollResult, error) {
	if len(raw) == 0 {
		return adapter.PollResult{}, fmt.Errorf("bfl: empty poll response body")
	}
	var pr pollResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return adapter.PollResult{}, fmt.Errorf("bfl: decode poll body: %w", err)
	}
	switch pr.Status {
	case statusPending:
		return adapter.PollResult{Status: adapter.PollPending, Progress: pr.Progress}, nil
	// BFL also reports running-states like "Request Moderated" + "Task in progress" in some
	// undocumented surfaces; surface anything that's neither terminal-good nor terminal-bad
	// as Running so the worker keeps polling.
	case "":
		return adapter.PollResult{Status: adapter.PollRunning, Progress: pr.Progress}, nil
	case statusReady:
		nr, err := normalizeReadyResult(model, raw)
		if err != nil {
			return adapter.PollResult{}, err
		}
		return adapter.PollResult{Status: adapter.PollSucceeded, Progress: pr.Progress, Result: nr}, nil
	case statusContentModerated:
		return adapter.PollResult{
			Status: adapter.PollFailed,
			Error: &adapter.PollError{
				Class:   adapter.ErrClassContentPolicy,
				Message: "content moderated by upstream policy",
				Raw:     capRaw(raw),
			},
		}, nil
	case statusTaskNotFound:
		return adapter.PollResult{
			Status: adapter.PollFailed,
			Error: &adapter.PollError{
				Class:   adapter.ErrClassNotFound,
				Message: "upstream task expired (>~10min retention)",
				Raw:     capRaw(raw),
			},
		}, nil
	case statusError:
		msg := "upstream task failed"
		if pr.Result != nil && pr.Result.Details != "" {
			msg = pr.Result.Details
		}
		return adapter.PollResult{
			Status: adapter.PollFailed,
			Error: &adapter.PollError{
				Class:   adapter.ErrClassUpstream,
				Message: msg,
				Raw:     capRaw(raw),
			},
		}, nil
	default:
		// Treat unknown statuses as Running rather than Failed: a future BFL
		// status code shouldn't take a user's funds for an in-flight task.
		return adapter.PollResult{Status: adapter.PollRunning, Progress: pr.Progress}, nil
	}
}

// capRaw caps the body at adapter.MaxRawErrorBytes (8 KiB) per L4 fix.
func capRaw(raw []byte) []byte {
	if len(raw) <= adapter.MaxRawErrorBytes {
		out := make([]byte, len(raw))
		copy(out, raw)
		return out
	}
	out := make([]byte, adapter.MaxRawErrorBytes)
	copy(out, raw[:adapter.MaxRawErrorBytes])
	return out
}
