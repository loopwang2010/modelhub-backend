// Package adapter defines the central ProviderAdapter interface that every
// upstream provider integration must implement.
//
// This file is the load-bearing contract for modelhub. Per ADR-003, the
// interface MUST stay provider-agnostic — no fal-specific fields, no
// OpenAI-style tool-calling parameters, no Replicate cog references.
// If a code reviewer can identify the upstream provider from anything
// in this file, the abstraction has leaked.
//
// v2 (2026-05-07): post-adversarial-review revisions per S2 review:
//   - Poll/Cancel now take ModelKey (C3): Google Vertex AI op paths embed model
//   - SubmitResult is now a sealed union (H1): no more nil-xor-set polymorphism
//   - Capabilities is per-model (H2): BFL has webhook for flux-1.1 but not flux-pro
//   - NormalizeResult is exposed (H3): testable in isolation against golden files
//   - MaxCostUSD ceiling (H4): wallet refuses estimates beyond this
//   - OutputType renamed OutputKind (H5): disambiguates from Modality
//   - Idempotency contract clarified (C4): adapter forwards as a hint;
//     true dedup happens in the wrapper layer in S2.5
//   - PollResult.Progress (M1): 0–1 fraction for streaming-aware UIs
//   - VerifyWebhook on interface (M2): kills HMAC reinvention per adapter
package adapter

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// Identity types
// ─────────────────────────────────────────────────────────────────────────

// ProviderKey identifies an upstream provider in the registry.
// Examples: "bfl", "google-ai", "openai".
type ProviderKey string

// ModelKey identifies a specific model variant exposed to users via /v1/models.
// Examples: "flux-pro-1.1", "veo-3.0-pro", "gpt-image-1-edit".
// The format is opaque to the registry — manifests own the model identity.
type ModelKey string

// IdempotencyKey is a sha256-hex hash of (account_id, model, canonical_params, time_bucket).
// Bucket size is 60s per S5 spec — see internal/task for canonicalization.
//
// Adapter contract (C4 fix): Submit() forwards this to upstream when the
// upstream supports a known idempotency header. This is a HINT, not the
// source of truth for dedup. The wrapper layer in S2.5 owns the
// (idempotency_key → task_id) mapping in our DB and is the authoritative
// dedup mechanism. Adapters that forget to forward the key still produce
// correct (de-duplicated) behavior at the wrapper layer.
type IdempotencyKey string

// UpstreamRef is an opaque reference to a task on the upstream provider's side.
// The adapter is responsible for understanding its format. We persist it on the
// task row to support polling, cancellation, and webhook correlation.
type UpstreamRef string

// CostUSD is denominated in MICRO-DOLLARS (USD * 1_000_000) to avoid float pitfalls
// in wallet arithmetic. $0.04 = 40_000.
type CostUSD int64

// MaxCostUSD is the largest cost a single Submit may incur (H4 fix).
// Wallet rejects any EstimateCost result exceeding this with a clear error.
// Set generously to $1_000 — far above any reasonable single-call cost,
// but tight enough to catch accidental zero-or-1e9 bugs.
const MaxCostUSD CostUSD = 1_000_000_000 // $1,000 in micro-USD

// ─────────────────────────────────────────────────────────────────────────
// Domain enums (kept here to avoid import cycles with task/catalog packages)
// ─────────────────────────────────────────────────────────────────────────

// Modality describes the OUTPUT type a model produces.
type Modality string

const (
	ModalityImage Modality = "image"
	ModalityVideo Modality = "video"
	ModalityAudio Modality = "audio"
	ModalityEdit  Modality = "edit" // image-in → image-out
	ModalityLLM   Modality = "llm"
)

// TaskKind describes the dispatch pattern for invoking a model.
type TaskKind string

const (
	TaskKindSync      TaskKind = "sync"
	TaskKindAsync     TaskKind = "async"
	TaskKindStreaming TaskKind = "streaming"
)

// ─────────────────────────────────────────────────────────────────────────
// Params
// ─────────────────────────────────────────────────────────────────────────

// Params is the validated, normalized input for a model invocation.
// Shape is per-model and validated against ModelManifest.InputSchema BEFORE
// Submit is called. Adapters MUST NOT do schema validation themselves —
// trust the caller. Adapters MAY translate from this canonical form into
// provider-specific request bodies inside Submit.
//
// Provider-specific structural constraints that JSON-Schema can't express
// (e.g., "width must be divisible by 64") may optionally be enforced via
// the ParamsValidator interface (M4) — adapters that implement it get a
// pre-flight pass before Submit.
type Params map[string]any

// ParamsValidator is an OPTIONAL interface adapters may implement to
// express provider-specific param constraints in a testable way (M4 fix).
// The wrapper layer in S2.5 invokes Validate before calling Submit.
type ParamsValidator interface {
	Validate(model ModelKey, params Params) error
}

// ─────────────────────────────────────────────────────────────────────────
// Submit result — sealed union (H1 fix)
// ─────────────────────────────────────────────────────────────────────────

// SubmitResult is a sealed sum type returned by ProviderAdapter.Submit.
// Concrete implementations: AsyncSubmit, SyncSubmit. The interface method
// (resultKind) is unexported, so only this package can declare new variants.
// Callers use a type switch.
type SubmitResult interface {
	AcceptedAt() time.Time
	resultKind() // unexported sealing method
}

// AsyncSubmit is the SubmitResult variant for async tasks.
// The worker should poll using UpstreamRef.
type AsyncSubmit struct {
	UpstreamRef UpstreamRef
	At          time.Time
}

func (a AsyncSubmit) AcceptedAt() time.Time { return a.At }
func (AsyncSubmit) resultKind()             {}

// SyncSubmit is the SubmitResult variant for sync tasks where the upstream
// returned a result inline. Worker proceeds directly to Settle (after S9.5
// asset hosting) — no Poll required.
type SyncSubmit struct {
	Result *NormalizedResult
	At     time.Time
}

func (s SyncSubmit) AcceptedAt() time.Time { return s.At }
func (SyncSubmit) resultKind()             {}

// ─────────────────────────────────────────────────────────────────────────
// Poll result
// ─────────────────────────────────────────────────────────────────────────

// PollResult is what an adapter returns from Poll().
type PollResult struct {
	Status PollStatus

	// Progress is an optional 0..1 completion fraction (M1).
	// Set by upstreams that report progress (BFL flux-1.1, future streaming-image).
	// nil = no progress info available.
	Progress *float32

	// Result is set when Status == PollSucceeded. Contains upstream URLs which
	// S9.5 asset worker rewrites to our CDN URL before any user exposure.
	Result *NormalizedResult

	// Error is set when Status == PollFailed.
	Error *PollError
}

// PollStatus is the adapter-level view of upstream task state.
// Distinct from internal/task.TaskState (which has more states for our internal
// FSM like Held, Settled, AssetLost). Mapping happens in the worker layer.
type PollStatus string

const (
	PollPending   PollStatus = "pending"
	PollRunning   PollStatus = "running"
	PollSucceeded PollStatus = "succeeded"
	PollFailed    PollStatus = "failed"
)

// PollError categorizes upstream failures so the caller can decide refund policy.
type PollError struct {
	Class   ErrorClass
	Message string // Human-readable; safe to surface to user after sanitization.

	// Raw is the original upstream error body. Capped at 8KiB on receipt (L4 fix)
	// to prevent unbounded memory growth from upstreams that stream stack traces.
	// For logs only, NEVER surfaced to user.
	Raw []byte
}

// MaxRawErrorBytes is the cap on PollError.Raw size.
const MaxRawErrorBytes = 8 * 1024

// ErrorClass is the canonical taxonomy for upstream failures.
type ErrorClass string

const (
	ErrClassAuth          ErrorClass = "auth"           // 401 — admin issue, user gets full refund
	ErrClassPayment       ErrorClass = "payment"        // 402 — modelhub out of upstream credit, full refund
	ErrClassRateLimit     ErrorClass = "rate_limit"     // 429 — back off, retry
	ErrClassContentPolicy ErrorClass = "content_policy" // upstream NSFW/policy reject — partial refund (compute portion)
	ErrClassUpstream      ErrorClass = "upstream"       // 5xx — full refund
	ErrClassNotFound      ErrorClass = "not_found"      // upstream ref expired/cleaned (e.g., BFL ~10min retention).
	//                                                     Worker maps to StateTimedOut for FSM; surfaces as
	//                                                     "task expired" to user. See M5 review note.
	ErrClassTimeout ErrorClass = "timeout" // exceeded our SLA — full refund
	ErrClassUnknown ErrorClass = "unknown" // Catch-all; alerts ops
)

// ─────────────────────────────────────────────────────────────────────────
// Normalized result
// ─────────────────────────────────────────────────────────────────────────

// NormalizedResult is the unified output shape across all providers/modalities.
// Adapters MUST map upstream-specific response shapes into this struct via
// NormalizeResult() (now exposed on the interface, H3 fix).
type NormalizedResult struct {
	Modality Modality
	Outputs  []Output       // Most models return 1; some return N (e.g., Flux num_images=4)
	Metadata map[string]any // Optional: seed used, model version actually run, upstream timing
}

// Output is a single result element. After S9.5 hosts it, URL is our CDN URL.
type Output struct {
	Kind      OutputKind
	URL       string // CDN URL post-S9.5; never an upstream URL exposed to user
	MimeType  string
	SizeBytes int64 // Renamed from Bytes (L3) to disambiguate from "actual byte content"
}

// OutputKind is the discriminator for the polymorphic NormalizedResult.Outputs.
// Renamed from OutputType (H5) to avoid confusion with Modality.
type OutputKind string

const (
	OutputKindImageURL OutputKind = "image_url"
	OutputKindVideoURL OutputKind = "video_url"
	OutputKindAudioURL OutputKind = "audio_url"
	OutputKindText     OutputKind = "text"
	OutputKindBase64   OutputKind = "base64" // S9.5 still uploads to our CDN even when upstream returns base64
)

// ─────────────────────────────────────────────────────────────────────────
// Capabilities
// ─────────────────────────────────────────────────────────────────────────

// ProviderCaps describes which optional features an adapter supports for a model.
// Per-model (H2 fix): BFL launched webhooks for flux-1.1-pro mid-2026 but not
// flux-pro; static-per-adapter caps would have lied. Static per (adapter, model)
// is still cheap and structural.
type ProviderCaps struct {
	SupportsWebhook   bool
	SupportsCancel    bool
	SupportsStreaming bool

	// MaxConcurrent is a hint for our scheduler; 0 means no hint.
	MaxConcurrent int
}

// ─────────────────────────────────────────────────────────────────────────
// Webhook verification
// ─────────────────────────────────────────────────────────────────────────

// WebhookVerification is the result of verifying an inbound webhook.
// Returned by VerifyWebhook so the gateway can advance the FSM
// without each adapter reinventing HMAC logic in HTTP-handler code.
type WebhookVerification struct {
	UpstreamRef UpstreamRef // The task this webhook is reporting on
	Result      PollResult  // The polling-equivalent translation
}

// ─────────────────────────────────────────────────────────────────────────
// The interface
// ─────────────────────────────────────────────────────────────────────────

// ProviderAdapter is the central abstraction. ONE implementation per upstream provider.
//
// Per ADR-003, this interface MUST stay provider-agnostic. Reviewer guard:
// if you can identify which upstream provider an adapter targets just by
// reading the interface signature, the interface has leaked.
//
// Implementations live in subpackages: internal/adapter/bfl, internal/adapter/googleai,
// internal/adapter/openai. Each implementation registers itself with the registry
// in its package init() — but the registry itself ships in S2.5, not here.
type ProviderAdapter interface {
	// Key returns the registry key for this adapter. Stable for the lifetime of the process.
	Key() ProviderKey

	// Submit forwards a generation request to the upstream provider.
	// params is pre-validated against ModelManifest.InputSchema by the caller.
	// idempotencyKey is forwarded to upstream as a HINT (per C4 contract).
	// Returns SubmitResult — type switch on AsyncSubmit vs SyncSubmit.
	Submit(ctx context.Context, model ModelKey, params Params, idempotencyKey IdempotencyKey) (SubmitResult, error)

	// Poll fetches current state of a previously-submitted async task.
	// model is included (C3 fix) because some upstreams (e.g., Google Vertex AI)
	// embed the model name in the polling URL path.
	// Callers MUST use exponential backoff with jitter; the adapter MUST NOT
	// add its own internal sleep loop.
	Poll(ctx context.Context, model ModelKey, ref UpstreamRef) (PollResult, error)

	// Cancel attempts to cancel an in-flight task.
	// model is included (C3 fix) for the same reason as Poll.
	// Returns ErrUnsupported when Capabilities(model).SupportsCancel == false.
	Cancel(ctx context.Context, model ModelKey, ref UpstreamRef) error

	// EstimateCost predicts the cost of a Submit call BEFORE it's made.
	// MUST return a value <= MaxCostUSD; the wallet rejects beyond that ceiling.
	// MAY exceed actual cost; Settle reconciles. Implementations should
	// over-estimate to avoid under-Hold debits.
	EstimateCost(model ModelKey, params Params) (CostUSD, error)

	// Capabilities returns per-model feature flags (H2 fix).
	// Static per (adapter, model). MUST NOT vary between calls for the same model.
	Capabilities(model ModelKey) ProviderCaps

	// NormalizeResult translates a raw upstream response body into our canonical shape.
	// Exposed (H3 fix) for direct unit testing against golden files captured from
	// real upstream responses, without needing HTTP mocks.
	// Submit and Poll call this internally; tests call it directly.
	NormalizeResult(model ModelKey, raw []byte) (*NormalizedResult, error)

	// VerifyWebhook authenticates and parses an inbound webhook from this provider.
	// Returns the (ref, poll-equivalent) pair so the gateway can advance the FSM
	// without per-adapter HMAC reimplementation (M2 fix).
	// Returns ErrUnsupported when this adapter does not handle webhooks.
	VerifyWebhook(headers http.Header, body []byte) (*WebhookVerification, error)
}

// ─────────────────────────────────────────────────────────────────────────
// Sentinel errors
// ─────────────────────────────────────────────────────────────────────────

// ErrUnsupported is returned by optional methods (Cancel, VerifyWebhook) when
// the adapter does not implement the operation. Callers MUST handle this
// distinctly from genuine errors.
var ErrUnsupported = errors.New("adapter: operation not supported")

// ErrNotConfigured is returned when the adapter lacks required configuration
// (missing API key, missing service account, etc.). Distinct from ErrClassAuth
// which is a runtime auth rejection by upstream.
var ErrNotConfigured = errors.New("adapter: not configured")

// ErrInvalidParams is returned by Submit if params fail provider-specific
// validation that couldn't be expressed in ModelManifest.InputSchema. This is
// a 400-class error, not a runtime failure; surfaced to user with sanitized message.
var ErrInvalidParams = errors.New("adapter: invalid params")

// ErrCostCeilingExceeded is returned by the wrapper layer when EstimateCost
// returns a value exceeding MaxCostUSD. Should never happen with well-formed
// manifests; presence indicates either a manifest bug or a cost-formula bug.
var ErrCostCeilingExceeded = errors.New("adapter: cost estimate exceeds MaxCostUSD ceiling")
