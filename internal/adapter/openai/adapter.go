// adapter.go — OpenAIImageAdapter implements adapter.ProviderAdapter.
//
// Scope: image-edit-only. The OpenAI image-edit endpoint
// `/v1/images/edits` is sync (response inline). Submit returns a
// SyncSubmit, Poll/Cancel return ErrUnsupported.
//
// AP guards enforced:
//
//   - AP-9 (mixed result shapes): NormalizeResult delegates to
//     normalize.go which handles BOTH base64 and URL responses.
//   - AP-13 (no-disk source images): the upload-fetch path streams
//     from object storage with a memory cap; never touches disk.
//   - AP-17 (source validation): defense-in-depth in upload_fetch.go.
//   - AP-19 (no upstream-URL leaks): NormalizeResult sets Output.URL
//     to "" — S9.5 fills the CDN URL before any user sees the result.
//
// Cost model: token-based. EstimateCost over-estimates from size +
// quality params; Settle reconciles using the response's
// `usage.total_tokens`. Documented inline below and in manifest_seed.go.

package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// ProviderKeyOpenAI is the canonical registry key for this adapter.
const ProviderKeyOpenAI adapter.ProviderKey = "openai"

// Static set of supported model keys. Submit rejects requests for
// any key outside this set with ErrInvalidParams (defense in depth —
// the registry routing should already have filtered).
var supportedModels = map[adapter.ModelKey]struct{}{
	"gpt-image-1":      {},
	"gpt-image-1-mini": {},
	"gpt-image-1.5":    {},
}

// Per-model max-concurrent hint. OpenAI's image tier-1 default sits
// around 50 RPM concurrent. We expose this on Capabilities() so the
// scheduler can rate-limit. Tier-specific overrides land via env in
// a future iteration.
const defaultMaxConcurrent = 50

// OpenAIImageAdapter is the ProviderAdapter implementation for OpenAI's
// image-edit endpoint. Construct via New() — the zero value is NOT
// usable (uploadFetcher is nil).
//
// Concurrency: methods are safe for concurrent use. cfg / fetcher are
// installed at construction and never mutated.
type OpenAIImageAdapter struct {
	cfg     *clientConfig
	fetcher uploadFetcher
}

// New constructs an OpenAIImageAdapter wired to the env-derived
// client config and the supplied upload fetcher.
//
// The fetcher MUST be non-nil — there's no safe default (we have no
// embedded object-storage client at this layer). Callers in init()
// should construct the fetcher from S9.5's storage package.
//
// Returns ErrNotConfigured if OPENAI_API_KEY is missing.
func New(fetcher uploadFetcher) (*OpenAIImageAdapter, error) {
	if fetcher == nil {
		return nil, fmt.Errorf("openai: nil uploadFetcher")
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return nil, err
	}
	return &OpenAIImageAdapter{cfg: cfg, fetcher: fetcher}, nil
}

// newWithConfig is a test seam — lets adapter_test.go inject an httptest
// server's URL and a memory fetcher without exporting cfg.
func newWithConfig(cfg *clientConfig, fetcher uploadFetcher) *OpenAIImageAdapter {
	return &OpenAIImageAdapter{cfg: cfg, fetcher: fetcher}
}

// Key returns the registry identifier for this adapter.
func (*OpenAIImageAdapter) Key() adapter.ProviderKey { return ProviderKeyOpenAI }

// Capabilities returns the per-model feature flags.
//
// All gpt-image-1 variants today are sync, no webhooks, no cancel,
// no streaming. MaxConcurrent is set to the OpenAI tier-1 default;
// a future PR will switch to env-configurable per-tier.
func (*OpenAIImageAdapter) Capabilities(model adapter.ModelKey) adapter.ProviderCaps {
	return adapter.ProviderCaps{
		SupportsWebhook:   false,
		SupportsCancel:    false,
		SupportsStreaming: false,
		MaxConcurrent:     defaultMaxConcurrent,
	}
}

// EstimateCost over-estimates the cost of a single image edit so the
// wallet's Hold is never under-debited. Settle reconciles to actual
// using the response's `usage.total_tokens`.
//
// Pricing formula (over-estimate, micro-USD):
//
//	base_cost     = base_micro_usd_per_model[model]
//	size_factor   = sizeMultiplier(params["size"])  // 1024² baseline = 1.0
//	quality_factor= qualityMultiplier(params["quality"]) // standard=1.0, high=2.0
//	n             = clamp(params["n"], 1, 10)
//	estimate      = base_cost * size_factor * quality_factor * n * SAFETY_MARGIN
//
// SAFETY_MARGIN = 1.5 — empirical buffer against token-based pricing
// volatility. We'd rather over-Hold $0.06 and refund $0.04 than
// under-Hold and chase a delinquent wallet.
//
// All values capped to MaxCostUSD ($1000); a request exceeding that
// is rejected by the wallet layer per H4.
func (a *OpenAIImageAdapter) EstimateCost(model adapter.ModelKey, params adapter.Params) (adapter.CostUSD, error) {
	if _, ok := supportedModels[model]; !ok {
		return 0, fmt.Errorf("%w: model %q not supported", adapter.ErrInvalidParams, model)
	}
	base := baseCostMicroUSD[model]
	if base == 0 {
		return 0, fmt.Errorf("openai: no base cost configured for model %q", model)
	}
	sizeFactor := sizeMultiplier(stringParam(params, "size"))
	qualityFactor := qualityMultiplier(stringParam(params, "quality"))
	n := intParam(params, "n", 1)
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10 // OpenAI's hard cap
	}
	const safetyMargin = 1.5

	cost := float64(base) * sizeFactor * qualityFactor * float64(n) * safetyMargin
	if cost > float64(adapter.MaxCostUSD) {
		return adapter.MaxCostUSD, fmt.Errorf("%w: estimate %.0f exceeds ceiling", adapter.ErrCostCeilingExceeded, cost)
	}
	return adapter.CostUSD(cost), nil
}

// baseCostMicroUSD is the per-output baseline (1024² standard quality, n=1)
// in micro-USD. Values are over-estimates relative to OpenAI's published
// pricing and are reconciled at Settle via response.usage.total_tokens.
var baseCostMicroUSD = map[adapter.ModelKey]adapter.CostUSD{
	"gpt-image-1":      40_000, // $0.040
	"gpt-image-1-mini": 15_000, // $0.015 (cheaper variant)
	"gpt-image-1.5":    80_000, // $0.080 (premium variant)
}

// sizeMultiplier derives the cost multiplier from the OpenAI `size`
// param. Treats unknown / empty as 1.0 baseline. Larger images cost
// more tokens linearly with pixel count.
func sizeMultiplier(size string) float64 {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "", "auto", "1024x1024":
		return 1.0
	case "1024x1536", "1536x1024":
		return 1.5
	case "1536x1536":
		return 2.25
	case "2048x2048":
		return 4.0
	case "256x256":
		return 0.25
	case "512x512":
		return 0.5
	default:
		// Unknown size — treat as 2x baseline to over-estimate.
		return 2.0
	}
}

// qualityMultiplier derives the cost multiplier from the OpenAI
// `quality` param. "high" doubles the output token budget.
func qualityMultiplier(quality string) float64 {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "high", "hd":
		return 2.0
	case "low":
		return 0.5
	case "", "auto", "medium", "standard":
		return 1.0
	default:
		// Over-estimate unknowns.
		return 2.0
	}
}

// stringParam reads a string-valued param tolerantly. Returns "" when
// the key is missing or the value isn't a string.
func stringParam(params adapter.Params, key string) string {
	v, ok := params[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// intParam reads an int-valued param tolerantly. Accepts int, int64,
// float64 (JSON's default numeric type). Returns fallback on miss.
func intParam(params adapter.Params, key string, fallback int) int {
	v, ok := params[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return fallback
	}
}

// Submit POSTs a multipart edit request to OpenAI and returns a
// SyncSubmit with the inline-decoded result.
//
// Steps:
//  1. Validate model + required params
//  2. Resolve image_id → fetch source bytes from object storage
//  3. Build multipart/form-data body
//  4. POST with Authorization: Bearer header
//  5. Read response, classify errors, normalize on success
//  6. Return SyncSubmit{Result, At}
//
// Returns a typed *Error for upstream failures so wrappers can
// inspect ErrClass (auth/payment/rate_limit/content_policy/upstream).
func (a *OpenAIImageAdapter) Submit(ctx context.Context, model adapter.ModelKey, params adapter.Params, idem adapter.IdempotencyKey) (adapter.SubmitResult, error) {
	if _, ok := supportedModels[model]; !ok {
		return nil, fmt.Errorf("%w: model %q not supported", adapter.ErrInvalidParams, model)
	}
	if idem == "" {
		return nil, fmt.Errorf("%w: empty idempotency key", adapter.ErrInvalidParams)
	}
	prompt := stringParam(params, "prompt")
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("%w: prompt is required", adapter.ErrInvalidParams)
	}
	imageID := stringParam(params, "image_id")
	if strings.TrimSpace(imageID) == "" {
		return nil, fmt.Errorf("%w: image_id is required", adapter.ErrInvalidParams)
	}

	src, err := fetchSourceImage(ctx, a.fetcher, imageID)
	if err != nil {
		return nil, fmt.Errorf("openai: source fetch: %w", err)
	}

	body, contentType, err := buildMultipartBody(model, prompt, params, src)
	if err != nil {
		return nil, fmt.Errorf("openai: build multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.editsURL(), body)
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	a.cfg.applyAuth(req)

	resp, err := a.cfg.http.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()

	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, &Error{Class: adapter.ErrClassUpstream, Status: resp.StatusCode, Message: "read response: " + readErr.Error()}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyHTTPError(resp.StatusCode, rawBody)
	}

	result, err := normalizeImagesResponse(model, rawBody)
	if err != nil {
		return nil, &Error{Class: adapter.ErrClassUpstream, Status: resp.StatusCode, Message: "normalize: " + err.Error()}
	}
	return adapter.SyncSubmit{
		Result: result,
		At:     time.Now().UTC(),
	}, nil
}

// Poll returns ErrUnsupported — gpt-image-1 is sync, response is inline.
// The worker FSM should never call Poll for sync tasks (Created → Held
// → Submitted → Succeeded all happen in one tick).
func (*OpenAIImageAdapter) Poll(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) (adapter.PollResult, error) {
	return adapter.PollResult{}, adapter.ErrUnsupported
}

// Cancel returns ErrUnsupported — sync model has nothing to cancel.
func (*OpenAIImageAdapter) Cancel(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) error {
	return adapter.ErrUnsupported
}

// NormalizeResult exposes the response normalizer for golden-file
// testing per H3. Submit calls this internally; tests call it directly
// against captured testdata/*.json fixtures.
func (*OpenAIImageAdapter) NormalizeResult(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	return normalizeImagesResponse(model, raw)
}

// VerifyWebhook returns ErrUnsupported — OpenAI image API doesn't push.
func (*OpenAIImageAdapter) VerifyWebhook(headers http.Header, body []byte) (*adapter.WebhookVerification, error) {
	return nil, adapter.ErrUnsupported
}

// ─────────────────────────────────────────────────────────────────────
// Multipart body construction
// ─────────────────────────────────────────────────────────────────────

// maxResponseBytes caps the response body we'll buffer. OpenAI's b64
// for a 1024² PNG is ~3 MB; we allow 32 MB headroom to cover up to
// 2048² + multiple n. Beyond that we treat as upstream malfunction.
const maxResponseBytes int64 = 32 * 1024 * 1024

// buildMultipartBody builds the multipart/form-data body for an image
// edit request. Returns the body reader, the Content-Type with boundary,
// and any build error.
//
// Required fields per OpenAI:
//   - model     (string) — translated from our adapter.ModelKey
//   - prompt    (string)
//   - image     (file)   — source bytes from object storage
//
// Optional fields we forward when present in params:
//   - n         (int)
//   - size      (string)
//   - quality   (string)
//   - response_format (string) — "b64_json" or "url" (gpt-image-1.5+)
//   - user      (string) — End-User identifier per OpenAI guidance
//   - mask      (file ref via params["mask_id"]) — future, not implemented yet
func buildMultipartBody(model adapter.ModelKey, prompt string, params adapter.Params, src *SourceImage) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("model", string(model)); err != nil {
		return nil, "", err
	}
	if err := mw.WriteField("prompt", prompt); err != nil {
		return nil, "", err
	}
	// image — the binary part with explicit Content-Type header on
	// the part itself (some older proxies drop it from the file form).
	imagePart, err := mw.CreateFormFile("image", src.Filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := imagePart.Write(src.Bytes); err != nil {
		return nil, "", err
	}

	// Optional integer / string fields. Only forward when the caller
	// supplied them; OpenAI documents sensible defaults.
	if n := intParam(params, "n", 0); n > 0 {
		if err := mw.WriteField("n", fmt.Sprintf("%d", n)); err != nil {
			return nil, "", err
		}
	}
	if size := stringParam(params, "size"); size != "" {
		if err := mw.WriteField("size", size); err != nil {
			return nil, "", err
		}
	}
	if quality := stringParam(params, "quality"); quality != "" {
		if err := mw.WriteField("quality", quality); err != nil {
			return nil, "", err
		}
	}
	if rf := stringParam(params, "response_format"); rf != "" {
		if err := mw.WriteField("response_format", rf); err != nil {
			return nil, "", err
		}
	}
	if user := stringParam(params, "user"); user != "" {
		if err := mw.WriteField("user", user); err != nil {
			return nil, "", err
		}
	}

	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

// ─────────────────────────────────────────────────────────────────────
// Error classification
// ─────────────────────────────────────────────────────────────────────

// Error is the adapter's typed-error envelope for upstream failures.
// Carries the canonical ErrorClass plus the original HTTP status and a
// sanitized message. Raw response body is captured (capped) for log-side
// triage but never surfaced verbatim to users.
type Error struct {
	Class   adapter.ErrorClass
	Status  int
	Code    string
	Message string
	Raw     []byte
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("openai: status=%d class=%s code=%s: %s", e.Status, e.Class, e.Code, e.Message)
}

// classifyTransportError maps net/http errors (DNS, connect, timeout)
// into our taxonomy. Most fall under ErrClassUpstream; ctx cancellation
// is treated as ErrClassTimeout when deadline-driven.
func classifyTransportError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	cls := adapter.ErrClassUpstream
	switch {
	case strings.Contains(msg, "context deadline exceeded"):
		cls = adapter.ErrClassTimeout
	case strings.Contains(msg, "context canceled"):
		cls = adapter.ErrClassUnknown
	}
	return &Error{Class: cls, Status: 0, Message: msg}
}

// classifyHTTPError maps an upstream non-2xx response to our taxonomy.
// Both HTTP status code and the OpenAI-specific `error.code` body are
// inspected — code refines status (e.g., 400 + content_policy_violation
// → ErrClassContentPolicy, not generic ErrClassUpstream).
func classifyHTTPError(status int, raw []byte) error {
	cls := adapter.ErrClassUnknown
	code := ""
	message := http.StatusText(status)

	// Try to parse OpenAI's structured error body.
	var probe struct {
		Error rawImageErrorEntry `json:"error"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &probe)
		if probe.Error.Code != "" {
			code = probe.Error.Code
		}
		if probe.Error.Message != "" {
			message = probe.Error.Message
		}
	}

	switch status {
	case http.StatusUnauthorized:
		cls = adapter.ErrClassAuth
	case http.StatusPaymentRequired, http.StatusForbidden:
		cls = adapter.ErrClassPayment
	case http.StatusTooManyRequests:
		cls = adapter.ErrClassRateLimit
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		cls = adapter.ErrClassTimeout
	case http.StatusBadRequest:
		// Default to ErrClassUpstream for 400s; the body's `code`
		// refines into ErrClassContentPolicy when applicable.
		cls = adapter.ErrClassUpstream
	}
	// Code-based refinement overrides status-based when present.
	switch strings.ToLower(code) {
	case "insufficient_quota":
		cls = adapter.ErrClassPayment
	case "rate_limit_exceeded":
		cls = adapter.ErrClassRateLimit
	case "content_policy_violation", "moderation_blocked", "image_safety_violation":
		cls = adapter.ErrClassContentPolicy
	case "invalid_api_key", "incorrect_api_key", "invalid_authorization":
		cls = adapter.ErrClassAuth
	}
	if status >= 500 {
		cls = adapter.ErrClassUpstream
	}
	if len(raw) > adapter.MaxRawErrorBytes {
		raw = raw[:adapter.MaxRawErrorBytes]
	}
	return &Error{
		Class:   cls,
		Status:  status,
		Code:    code,
		Message: message,
		Raw:     raw,
	}
}
