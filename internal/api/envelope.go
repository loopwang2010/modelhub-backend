// Package api defines the HTTP-facing request and response envelopes
// for modelhub's unified generation surface. Per ADR-009 and ADR-018,
// modelhub MUST NOT expose endpoints that mirror upstream provider shapes;
// every generation goes through ONE polymorphic endpoint:
//
//	POST /v1/generations
//	GET  /v1/generations/{id}
//
// The request body is polymorphic on the `model` field; per-model param
// shape is owned by the manifest's InputSchema and validated at the API
// boundary before Submit.
//
// The response envelope is uniform across all modalities (image, video,
// audio, edit, llm). Frontend renders any modality off the same shape
// without a giant if-tree.
//
// AP guards enforced here:
//   - AP-19: response.output.url is ONLY a CDN URL post-S9.5; never an
//     upstream URL. The EnvelopeFromTask helper rejects URLs that don't
//     look like our CDN scheme to catch accidental leaks.
//   - ADR-018: this file does not name any upstream provider. The Provider
//     field in the manifest is internal-only; PublicJSON strips it and
//     the envelope NEVER surfaces it.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// ─────────────────────────────────────────────────────────────────────────
// Request envelope
// ─────────────────────────────────────────────────────────────────────────

// GenerationRequest is the wire shape for POST /v1/generations.
// Params is a json.RawMessage so the gateway can defer per-model schema
// validation to the manifest's InputSchema without coupling this struct
// to any specific model's parameters.
type GenerationRequest struct {
	// Model is the modelhub-side identifier (e.g. "flux-pro-1.1").
	// MUST exist in the registered ModelManifest set; the validate-model
	// middleware (RequireModelExists) rejects unknown models with 400.
	Model adapter.ModelKey `json:"model"`

	// Params is the polymorphic per-model parameter object. Validated
	// against ModelManifest.InputSchema before Submit is called.
	Params json.RawMessage `json:"params"`

	// IdempotencyKey, when set on the request, is forwarded as a hint
	// to the adapter (see adapter.IdempotencyKey doc for semantics).
	// When unset, the controller mints a unique per-generation upstream
	// hint and does not deduplicate otherwise identical requests.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Webhook (optional) is a URL we POST to when an async task reaches a
	// terminal state. Omitted = client polls GET /v1/generations/{id}.
	// Must be HTTPS in production; the gateway rejects non-https in prod.
	Webhook string `json:"webhook,omitempty"`
}

// Validate checks the structural invariants of a GenerationRequest.
// Per-model schema validation is the next pipeline step (see manifest).
func (r *GenerationRequest) Validate() error {
	if r.Model == "" {
		return errors.New("envelope: model is required")
	}
	if len(r.Params) == 0 {
		return errors.New("envelope: params is required")
	}
	// Confirm Params decodes as a JSON object (not an array, scalar, or null).
	// json.Unmarshal of "null" into map[string]any does NOT error — it leaves
	// the map as nil. Disallow that explicitly.
	trimmed := strings.TrimSpace(string(r.Params))
	if trimmed == "" || trimmed == "null" {
		return errors.New("envelope: params must be a JSON object")
	}
	if !strings.HasPrefix(trimmed, "{") {
		return errors.New("envelope: params must be a JSON object")
	}
	var probe map[string]any
	if err := json.Unmarshal(r.Params, &probe); err != nil {
		return fmt.Errorf("envelope: params must be a JSON object: %w", err)
	}
	if r.Webhook != "" {
		if !strings.HasPrefix(r.Webhook, "https://") && !strings.HasPrefix(r.Webhook, "http://") {
			return errors.New("envelope: webhook must be an absolute URL")
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Response envelope
// ─────────────────────────────────────────────────────────────────────────

// Status is the public-facing task lifecycle as seen by API clients.
// This is a SUBSET of internal/task.TaskState — clients never see e.g.
// "asset_lost" (collapsed to "failed" on the wire).
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// GenerationResponse is the uniform response shape for both
// POST /v1/generations and GET /v1/generations/{id}.
type GenerationResponse struct {
	// ID uniquely identifies this generation; format "gen_<base64ish>".
	ID string `json:"id"`

	// Model echoes the request's model field.
	Model adapter.ModelKey `json:"model"`

	// Status — see Status doc.
	Status Status `json:"status"`

	// Modality / TaskKind echo the manifest. Lets clients render results
	// without a separate /v1/models lookup.
	Modality adapter.Modality `json:"modality"`
	TaskKind adapter.TaskKind `json:"task_kind"`

	// CreatedAt is when the task was accepted; in UTC.
	CreatedAt time.Time `json:"created_at"`

	// CompletedAt is non-nil only when Status is terminal.
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// Output is non-nil only when Status == "succeeded". Polymorphic on
	// Type — see Output doc.
	Output *Output `json:"output,omitempty"`

	// Error is non-nil only when Status is "failed" or "cancelled".
	Error *Error `json:"error,omitempty"`

	// Credits surfaces wallet bookkeeping. Always present so clients can
	// compute remaining balance without a separate endpoint.
	Credits Credits `json:"credits"`
}

// Output is the polymorphic generated artifact. Type is the discriminator;
// URL is always our CDN URL post-S9.5 (NEVER an upstream URL — AP-19).
//
// Metadata holds optional model-specific extras (seed used, actual model
// version, upstream timing). Free-form by design; clients that don't need
// it ignore the field.
type Output struct {
	// Type is one of: "image_url", "video_url", "audio_url", "text",
	// "base64". Matches adapter.OutputKind verbatim.
	Type adapter.OutputKind `json:"type"`

	// URL is our CDN URL. Required when Type ends in "_url"; ignored for
	// "text" and "base64".
	URL string `json:"url,omitempty"`

	// Text is set when Type == "text" (LLM modality). Absent otherwise.
	Text string `json:"text,omitempty"`

	// Base64 is set when Type == "base64". Encoded payload.
	Base64 string `json:"base64,omitempty"`

	// MimeType for binary outputs.
	MimeType string `json:"mime_type,omitempty"`

	// SizeBytes for binary outputs (informational).
	SizeBytes int64 `json:"size_bytes,omitempty"`

	// Metadata is free-form per-model extras.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Error is the canonical failure payload.
type Error struct {
	// Code is a stable identifier suitable for client switch statements.
	// Maps from adapter.ErrorClass (e.g. "rate_limit", "content_policy").
	Code string `json:"code"`
	// Message is a sanitized human-readable string. Safe to surface.
	Message string `json:"message"`
}

// Credits surfaces the wallet bookkeeping per ADR-009/ADR-010. Held =
// reserved at task start; Settled = moved to revenue on Succeeded; Refunded
// = released back to user on Failed/Cancelled/AssetLost. Units are
// micro-USD (matching adapter.CostUSD).
type Credits struct {
	Held     adapter.CostUSD `json:"held"`
	Settled  adapter.CostUSD `json:"settled"`
	Refunded adapter.CostUSD `json:"refunded"`
}

// ─────────────────────────────────────────────────────────────────────────
// Builders
// ─────────────────────────────────────────────────────────────────────────

// TaskSnapshot is the minimal view of an internal task that the envelope
// builder needs. We model it as a struct (not a *task.Task pointer) so
// internal/api stays decoupled from internal/task — the worker constructs
// this snapshot when serving GET /v1/generations/{id}.
//
// AP-1 guard: this struct is provider-AGNOSTIC by design. The builder
// MUST NOT consume any provider-shaped data.
type TaskSnapshot struct {
	ID          string
	Model       adapter.ModelKey
	Status      Status
	CreatedAt   time.Time
	CompletedAt *time.Time
	Result      *adapter.NormalizedResult
	ErrorClass  adapter.ErrorClass
	ErrorMsg    string
	Credits     Credits
}

// EnvelopeFromTask converts a TaskSnapshot + manifest into the wire envelope.
// Returns an error when the snapshot violates ADR-018/AP-19 (e.g. the
// result URL is not CDN-shaped). Caller is responsible for fixing the data
// or returning an internal-error response.
func EnvelopeFromTask(ts *TaskSnapshot, m *catalog.ModelManifest) (*GenerationResponse, error) {
	if ts == nil {
		return nil, errors.New("envelope: nil TaskSnapshot")
	}
	if m == nil {
		return nil, errors.New("envelope: nil ModelManifest")
	}
	if ts.Model != m.Key {
		return nil, fmt.Errorf("envelope: snapshot model %q != manifest model %q", ts.Model, m.Key)
	}
	resp := &GenerationResponse{
		ID:          ts.ID,
		Model:       ts.Model,
		Status:      ts.Status,
		Modality:    m.Modality,
		TaskKind:    m.TaskKind,
		CreatedAt:   ts.CreatedAt,
		CompletedAt: ts.CompletedAt,
		Credits:     ts.Credits,
	}
	switch ts.Status {
	case StatusSucceeded:
		out, err := outputFromResult(ts.Result)
		if err != nil {
			return nil, err
		}
		resp.Output = out
	case StatusFailed, StatusCancelled:
		resp.Error = &Error{
			Code:    string(ts.ErrorClass),
			Message: ts.ErrorMsg,
		}
		if resp.Error.Code == "" {
			resp.Error.Code = "unknown"
		}
	}
	return resp, nil
}

// outputFromResult builds the wire Output from a NormalizedResult. Multi-
// output results (e.g. flux num_images=4) are flattened to the first
// element for v0; future revs may add an Outputs []Output field.
//
// AP-19 guard: rejects non-CDN URLs for any *_url type. CDN URLs MUST start
// with one of the prefixes in cdnURLPrefixes; this exists so a code-path
// that accidentally pipes an upstream URL straight to the envelope fails
// the test suite instead of leaking to users.
func outputFromResult(r *adapter.NormalizedResult) (*Output, error) {
	if r == nil {
		return nil, errors.New("envelope: succeeded task missing NormalizedResult")
	}
	if len(r.Outputs) == 0 {
		return nil, errors.New("envelope: NormalizedResult has zero outputs")
	}
	first := r.Outputs[0]
	out := &Output{
		Type:      first.Kind,
		MimeType:  first.MimeType,
		SizeBytes: first.SizeBytes,
		Metadata:  r.Metadata,
	}
	switch first.Kind {
	case adapter.OutputKindImageURL, adapter.OutputKindVideoURL, adapter.OutputKindAudioURL:
		if first.URL == "" {
			return nil, fmt.Errorf("envelope: %s output missing URL", first.Kind)
		}
		if !isCDNURL(first.URL) {
			return nil, fmt.Errorf("envelope: AP-19 violation: output URL %q is not a recognized CDN prefix", first.URL)
		}
		out.URL = first.URL
	case adapter.OutputKindText:
		// URL field unused; text content lives in metadata or a separate
		// field that adapters that emit text fill via NormalizeResult.
		// For now, surface the URL field as the text body when adapters
		// pre-flatten there, otherwise fall back to MetadataString helper.
		// Keeping this conservative until S15 (text-LLM) lands.
		if v, ok := r.Metadata["text"].(string); ok {
			out.Text = v
		}
	case adapter.OutputKindBase64:
		// Same conservative pattern as text — adapters pre-flatten via
		// metadata until base64 outputs are exercised end-to-end.
		if v, ok := r.Metadata["base64"].(string); ok {
			out.Base64 = v
		}
	default:
		return nil, fmt.Errorf("envelope: unsupported OutputKind %q", first.Kind)
	}
	return out, nil
}

// cdnURLPrefixes is the allow-list of URL schemes/hostnames considered
// "our CDN". Update this when we onboard new CDN domains.
//
// The "https://cdn.modelhub.local/" prefix is the dev/test CDN baked into
// the mock adapters; production prefixes get added once the real CDN is
// provisioned in S9.5.
var cdnURLPrefixes = []string{
	"https://cdn.modelhub.local/",
	"https://cdn.modelhub.com/",
	"https://assets.modelhub.com/",
}

// isCDNURL returns true when u starts with a known CDN prefix.
func isCDNURL(u string) bool {
	for _, p := range cdnURLPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// MapTaskStateToStatus translates internal task.TaskState string values to
// the wire-visible Status. Lives here (not in internal/task) so internal/api
// can stay the single owner of public vocabulary.
//
// Mapping (per ADR-009 wire contract):
//
//	created, held, submitted    → queued
//	running                     → running
//	succeeded, settled          → succeeded
//	asset_lost                  → failed   (clients see only the symptom)
//	failed                      → failed
//	timed_out                   → failed
//	cancelled                   → cancelled
func MapTaskStateToStatus(internal string) Status {
	switch internal {
	case "created", "held", "submitted":
		return StatusQueued
	case "running":
		return StatusRunning
	case "succeeded", "settled":
		return StatusSucceeded
	case "asset_lost", "failed", "timed_out":
		return StatusFailed
	case "cancelled":
		return StatusCancelled
	default:
		// Unknown state — fail closed so we never surface an undocumented
		// state to clients.
		return StatusFailed
	}
}
