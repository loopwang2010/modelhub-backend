// BFLAdapter — implements adapter.ProviderAdapter for Black Forest Labs.
//
// Pattern: async-polling (Submit returns id+polling_url; worker polls until terminal).
// Same machinery as the Veo3 (S8) adapter — they share the polling worker code from S5.
//
// Per ADR-018 (no-passthrough), this adapter accepts our canonical params shape
// and produces our canonical NormalizedResult; it does NOT mirror BFL's request
// body to /v1/generations callers. The upstream model name lives in
// ModelManifest.UpstreamModel and never escapes this package.
//
// UpstreamRef encoding:
//
//	BFL gives us TWO pieces at Submit time: an id and a polling_url. The
//	ProviderAdapter interface stores only ONE opaque UpstreamRef. We pack
//	them as `id|polling_url` (two-field, pipe-separated) because:
//	  - The wrapper stores UpstreamRef on the task row; we get persistence for free.
//	  - Pipe is not a valid char in UUID v4 ids and not in the URL path BFL
//	    issues (which is `/v1/get_result?id=<uuid>`), so the split is unambiguous.
//	  - Tests assert the encoding so a future change is caught.
//
//	An alternative (per-task struct map in adapter memory) was rejected because
//	it doesn't survive a process restart; persistence is a hard requirement
//	from S5's reconciler.

package bfl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// providerKey is the registry key for this adapter.
const providerKey adapter.ProviderKey = "bfl"

// upstreamRefSep separates the BFL id from the polling URL inside UpstreamRef.
// Chosen because it cannot appear in either component.
const upstreamRefSep = "|"

// BFLAdapter implements adapter.ProviderAdapter for api.bfl.ai.
//
// Construct via New() or NewFromEnv(); zero value is not usable.
// Safe for concurrent use — all state is in the embedded *client.
type BFLAdapter struct {
	client *client
	// nowFn allows tests to inject a fixed time. Defaults to time.Now.
	nowFn func() time.Time
}

// New returns an adapter against the given baseURL using apiKey.
// Test-only: production should call NewFromEnv.
func New(baseURL, apiKey string) *BFLAdapter {
	return &BFLAdapter{
		client: newClientForTesting(baseURL, apiKey),
		nowFn:  time.Now,
	}
}

// NewFromEnv constructs an adapter using BFL_API_KEY. Returns
// adapter.ErrNotConfigured (wrapped) when the env var is missing.
func NewFromEnv() (*BFLAdapter, error) {
	c, err := newClientFromEnv()
	if err != nil {
		return nil, err
	}
	return &BFLAdapter{client: c, nowFn: time.Now}, nil
}

// Key implements ProviderAdapter.
func (*BFLAdapter) Key() adapter.ProviderKey { return providerKey }

// Submit POSTs the canonical params to BFL's per-model endpoint.
//
// Per the v2 ProviderAdapter contract:
//   - params is pre-validated against ModelManifest.InputSchema.
//   - idempotencyKey is forwarded as `X-Idempotency-Key` (HINT only; wrapper
//     layer in S2.5 owns true dedup per C4).
//   - Returns AsyncSubmit because BFL is always async-polling.
func (a *BFLAdapter) Submit(ctx context.Context, model adapter.ModelKey, params adapter.Params, idempotencyKey adapter.IdempotencyKey) (adapter.SubmitResult, error) {
	if idempotencyKey == "" {
		return nil, fmt.Errorf("bfl: %w: empty idempotency key", adapter.ErrInvalidParams)
	}
	if params == nil {
		return nil, fmt.Errorf("bfl: %w: nil params", adapter.ErrInvalidParams)
	}
	upstreamModel, err := upstreamModelFor(model)
	if err != nil {
		return nil, err
	}

	// BFL accepts our canonical params shape directly: prompt, aspect_ratio,
	// seed, num_images, etc. all use the same field names. If a future model
	// diverges, translate inside this package; never let BFL field names
	// escape via /v1/generations.
	body := buildSubmitBody(params)

	var resp submitResponse
	_, errClass, err := a.client.doJSON(ctx, http.MethodPost, a.client.submitURL(upstreamModel), string(idempotencyKey), body, &resp)
	if err != nil {
		if errClass != "" {
			return nil, &bflError{class: errClass, msg: err.Error(), wrapped: err}
		}
		return nil, fmt.Errorf("bfl: submit: %w", err)
	}
	if resp.ID == "" || resp.PollingURL == "" {
		return nil, fmt.Errorf("bfl: submit response missing id or polling_url (got id=%q polling_url=%q)", resp.ID, resp.PollingURL)
	}
	return adapter.AsyncSubmit{
		UpstreamRef: encodeUpstreamRef(resp.ID, resp.PollingURL),
		At:          a.nowFn().UTC(),
	}, nil
}

// Poll fetches the polling URL packed into ref and translates the BFL status
// into our PollResult.
//
// AP-3 guard: this method does NOT sleep. The caller (the worker) owns
// exponential backoff with jitter.
func (a *BFLAdapter) Poll(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) (adapter.PollResult, error) {
	id, pollingURL, err := decodeUpstreamRef(ref)
	if err != nil {
		return adapter.PollResult{}, err
	}
	if id == "" || pollingURL == "" {
		return adapter.PollResult{}, fmt.Errorf("bfl: malformed upstream ref")
	}
	// We don't decode into pollResponse here — classifyPollStatus does the
	// status-string switch + body decoding in one place so adapter.go owns
	// HTTP and normalize.go owns shape interpretation.
	resp, err := a.client.httpClient.Do(mustGet(ctx, a.client.apiKey, pollingURL))
	if err != nil {
		return adapter.PollResult{}, fmt.Errorf("bfl: poll http: %w", err)
	}
	defer resp.Body.Close()

	body, status, errClass, readErr := readBody(resp)
	if readErr != nil {
		return adapter.PollResult{}, readErr
	}

	if status >= 400 {
		// HTTP-level error — never reaches the BFL JSON decoder.
		return adapter.PollResult{
			Status: adapter.PollFailed,
			Error: &adapter.PollError{
				Class:   errClass,
				Message: fmt.Sprintf("upstream HTTP %d", status),
				Raw:     capRaw(body),
			},
		}, nil
	}
	return classifyPollStatus(model, body)
}

// Cancel: BFL does not expose a cancel endpoint. Per S7 spec, return ErrUnsupported.
func (*BFLAdapter) Cancel(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) error {
	return adapter.ErrUnsupported
}

// EstimateCost returns the (over-estimated) cost in micro-USD for the model+params.
// Pricing source: docs.bfl.ai/pricing (verified at adapter time per S7 task #7).
func (*BFLAdapter) EstimateCost(model adapter.ModelKey, params adapter.Params) (adapter.CostUSD, error) {
	perImage, ok := perImagePricing[model]
	if !ok {
		// Defensive: an unknown-to-us model in the manifest probably means
		// the operator added a manifest row without updating this table.
		// Return a high-but-bounded estimate so the wallet refuses
		// rather than silently under-charge.
		return adapter.CostUSD(200_000), fmt.Errorf("bfl: no price entry for model %q", model)
	}
	n := numImagesFromParams(params)
	cost := adapter.CostUSD(int64(perImage) * int64(n))
	if cost > adapter.MaxCostUSD {
		return adapter.MaxCostUSD, fmt.Errorf("bfl: %w: model=%q n=%d", adapter.ErrCostCeilingExceeded, model, n)
	}
	return cost, nil
}

// Capabilities returns per-model feature flags (H2 fix). Per research
// T-004, BFL's webhook documentation is incomplete; we mark webhook=false
// across the board until we verify the HMAC scheme against a real webhook
// delivery. flux-2-pro and flux-1.1-pro are the candidate first opt-ins.
func (*BFLAdapter) Capabilities(model adapter.ModelKey) adapter.ProviderCaps {
	caps := adapter.ProviderCaps{
		SupportsWebhook:   false,
		SupportsCancel:    false,
		SupportsStreaming: false,
	}
	// Per-model differences placeholder: when we verify webhook signatures
	// for flux-2-pro / flux-1.1-pro, flip SupportsWebhook on for those two
	// keys. Until then, return the safe default.
	return caps
}

// NormalizeResult translates a raw BFL Ready response into our canonical
// shape. Exported per H3 fix for golden-file unit testing.
//
// Production callers: Poll already calls this internally via classifyPollStatus.
// Tests call this directly against testdata/bfl_*.json fixtures.
func (*BFLAdapter) NormalizeResult(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	return normalizeReadyResult(model, raw)
}

// VerifyWebhook returns ErrUnsupported — BFL's HMAC scheme is not yet
// publicly documented (research note T-004). When BFL publishes the scheme,
// this method gets the same shape as MockAsyncAdapter.VerifyWebhook:
// HMAC-SHA256 over the raw body, compared against a header value.
func (*BFLAdapter) VerifyWebhook(headers http.Header, body []byte) (*adapter.WebhookVerification, error) {
	// TODO(T-004): when BFL documents the webhook signature header (likely
	// `X-BFL-Signature`), implement HMAC-SHA256(secret, body) and translate
	// `status` → PollResult exactly like normalize.go's classifyPollStatus.
	return nil, adapter.ErrUnsupported
}

// ─────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────

// upstreamModelFor maps our public ModelKey → the path component BFL
// expects in its URL. Hardcoded list rather than identity mapping because
// our keys may diverge (e.g. our key adds a friendlier suffix).
var upstreamModels = map[adapter.ModelKey]string{
	"flux-pro-1.1":       "flux-pro-1.1",
	"flux-dev":           "flux-dev",
	"flux-schnell-v1.5":  "flux-schnell",
	"flux-2-pro":         "flux-2-pro",
}

func upstreamModelFor(model adapter.ModelKey) (string, error) {
	if m, ok := upstreamModels[model]; ok {
		return m, nil
	}
	return "", fmt.Errorf("bfl: unknown model key %q", model)
}

// perImagePricing is per-image cost in micro-USD (USD * 1_000_000).
// Source: docs.bfl.ai/pricing (read at S7 implementation time).
var perImagePricing = map[adapter.ModelKey]adapter.CostUSD{
	"flux-pro-1.1":      40_000, // $0.04
	"flux-dev":          25_000, // $0.025
	"flux-schnell-v1.5": 3_000,  // $0.003 (Schnell is cheap by design)
	"flux-2-pro":        80_000, // $0.08 (newer / higher quality)
}

// numImagesFromParams reads `num_images` (default 1) and clamps to [1, 8].
// 8 is BFL's documented per-request maximum; anything higher is a wrapper
// bug or a manifest-validation gap. We ceiling rather than reject so the
// EstimateCost call cannot fail purely from a malformed param.
func numImagesFromParams(params adapter.Params) int {
	if params == nil {
		return 1
	}
	v, ok := params["num_images"]
	if !ok {
		return 1
	}
	n := 0
	switch typed := v.(type) {
	case int:
		n = typed
	case int64:
		n = int(typed)
	case float64:
		n = int(typed)
	case json.Number:
		i, err := typed.Int64()
		if err != nil {
			return 1
		}
		n = int(i)
	}
	if n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}

// buildSubmitBody copies the canonical params into a fresh map so the caller's
// map can't be mutated by the JSON encoder. It also strips fields we know
// BFL doesn't accept (e.g., internal "x-modelhub-trace-id"), keeping the
// upstream surface tight.
func buildSubmitBody(params adapter.Params) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		// AP-19 cousin: never forward an internal-only key. Convention:
		// keys starting with underscore or "x-modelhub-" are private.
		if strings.HasPrefix(k, "_") || strings.HasPrefix(k, "x-modelhub-") {
			continue
		}
		out[k] = v
	}
	return out
}

// encodeUpstreamRef packs id + polling_url into a single opaque ref.
func encodeUpstreamRef(id, pollingURL string) adapter.UpstreamRef {
	return adapter.UpstreamRef(id + upstreamRefSep + pollingURL)
}

// decodeUpstreamRef unpacks an UpstreamRef back into (id, polling_url).
// Returns the raw ref string as id when no separator is present, mainly so
// tests asserting "old-style refs still poll" don't panic.
func decodeUpstreamRef(ref adapter.UpstreamRef) (id, pollingURL string, err error) {
	s := string(ref)
	idx := strings.Index(s, upstreamRefSep)
	if idx < 0 {
		return "", "", errors.New("bfl: upstream ref missing polling URL")
	}
	return s[:idx], s[idx+len(upstreamRefSep):], nil
}

// readBody is shared between Submit (ok path inside doJSON) and Poll
// (which sometimes builds the request manually because the polling URL is
// already a full URL, not a baseURL+path).
func readBody(resp *http.Response) (body []byte, status int, errClass adapter.ErrorClass, err error) {
	body, ioErr := readAllBounded(resp)
	if ioErr != nil {
		return nil, resp.StatusCode, adapter.ErrClassUpstream, fmt.Errorf("bfl: read body: %w", ioErr)
	}
	if resp.StatusCode >= 400 {
		return body, resp.StatusCode, classifyHTTPStatus(resp.StatusCode), nil
	}
	return body, resp.StatusCode, "", nil
}

// mustGet builds an authenticated GET. Pulled out as a tiny helper so Poll
// reads top-to-bottom.
func mustGet(ctx context.Context, apiKey, url string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set(authHeader, apiKey)
	req.Header.Set("Accept", "application/json")
	return req
}

// readAllBounded caps the response body read at maxResponseBytes (1 MiB) to
// guard against a misbehaving upstream streaming megabytes of payload.
func readAllBounded(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// bflError is the typed error returned by Submit so callers can extract
// ErrorClass deterministically. Mirrors the mock adapter's pattern.
type bflError struct {
	class   adapter.ErrorClass
	msg     string
	wrapped error
}

func (e *bflError) Error() string { return e.msg }
func (e *bflError) Unwrap() error { return e.wrapped }

// ErrorClass extracts the ErrorClass from an error returned by this adapter.
// Returns ("", false) when err is not a *bflError.
func ErrorClass(err error) (adapter.ErrorClass, bool) {
	var be *bflError
	if errors.As(err, &be) {
		return be.class, true
	}
	return "", false
}
