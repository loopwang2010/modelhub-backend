// Package googleai implements the ProviderAdapter for Google Vertex AI's
// Veo3 video-generation models.
//
// Architecture notes:
//   - Veo3 is async: Submit returns an operation name; the worker polls.
//   - Service-account auth via GOOGLE_APPLICATION_CREDENTIALS file (NEVER
//     inline JSON env). See auth.go.
//   - Endpoint shapes (predictLongRunning, GET /v1/{name}, :cancel) live in
//     client.go and never leak outside this package.
//   - Response normalization lives in normalize.go and is unit-tested
//     against captured golden files in testdata/.
//   - Pricing is in EstimateCost; manifests in manifest_seed.go.
//   - Webhooks are NOT supported (Eventarc is a separate integration that's
//     out of MVP scope per S7-S8 research §2).
//
// AP-3 guard: nothing in this file calls time.Sleep. The worker owns
// backoff. Submit/Poll/Cancel each issue exactly one HTTP round-trip.

package googleai

import (
	"bytes"
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

// ProviderName is the registry key for this adapter.
const ProviderName adapter.ProviderKey = "google-ai"

// veo3PerSecondCostMicroUSD maps an upstreamModel id to micro-USD per
// generated second. See manifest_seed.go for human-readable formula.
//
// Source: cloud.google.com/vertex-ai/generative-ai/pricing (verified 2026-05).
// If pricing changes, this map is the single point of update.
var veo3PerSecondCostMicroUSD = map[string]adapter.CostUSD{
	"veo-3.0-generate-preview":      500_000, // $0.50/s
	"veo-3.0-fast-generate-preview": 250_000, // $0.25/s
}

// GoogleVertexAIAdapter is the ProviderAdapter for Veo3-family models.
// Construct via NewGoogleVertexAIAdapter; the zero value is unusable.
type GoogleVertexAIAdapter struct {
	cfg      *config
	manifest map[adapter.ModelKey]string // model key → upstream model id
}

// NewGoogleVertexAIAdapter builds an adapter from process env. Returns
// adapter.ErrNotConfigured if GOOGLE_APPLICATION_CREDENTIALS is missing or
// the credentials file is unreadable; adapter.ErrInvalidParams if the
// configured location is on the geo deny-list.
func NewGoogleVertexAIAdapter(ctx context.Context) (*GoogleVertexAIAdapter, error) {
	cfg, err := loadConfig(ctx, getenvLookup)
	if err != nil {
		return nil, err
	}
	return newAdapter(cfg), nil
}

// newAdapterFromConfig is the test-friendly constructor — accepts a
// pre-built config (notably one whose baseURLOverride points at httptest).
func newAdapterFromConfig(cfg *config) *GoogleVertexAIAdapter {
	return newAdapter(cfg)
}

func newAdapter(cfg *config) *GoogleVertexAIAdapter {
	a := &GoogleVertexAIAdapter{
		cfg:      cfg,
		manifest: make(map[adapter.ModelKey]string, 4),
	}
	for _, m := range SeedManifests() {
		a.manifest[m.Key] = m.UpstreamModel
	}
	return a
}

// Key returns the registry identifier.
func (a *GoogleVertexAIAdapter) Key() adapter.ProviderKey { return ProviderName }

// Submit issues a predictLongRunning POST and returns an AsyncSubmit whose
// UpstreamRef is the full operation name. The op name embeds the model and
// location; Poll re-uses it directly.
func (a *GoogleVertexAIAdapter) Submit(
	ctx context.Context,
	model adapter.ModelKey,
	params adapter.Params,
	idempotencyKey adapter.IdempotencyKey,
) (adapter.SubmitResult, error) {
	if idempotencyKey == "" {
		return nil, fmt.Errorf("googleai: %w: empty idempotency key", adapter.ErrInvalidParams)
	}
	upstreamModel, ok := a.manifest[model]
	if !ok {
		return nil, fmt.Errorf("googleai: %w: unknown model %q", adapter.ErrInvalidParams, model)
	}

	body, err := buildSubmitBody(params)
	if err != nil {
		return nil, err
	}

	url, err := a.cfg.submitURL(upstreamModel)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("googleai: build submit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Vertex AI doesn't honour an X-Idempotency-Key header for
	// predictLongRunning; we still attach our key so it shows up in
	// upstream request logs for cross-correlation. (Per C4: forwarding is a
	// hint; dedup is owned by the wrapper layer.)
	req.Header.Set("X-Modelhub-Idempotency", string(idempotencyKey))

	resp, raw, err := a.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		class, msg := classifyHTTPError(resp.StatusCode, raw)
		return nil, &httpError{Class: class, Status: resp.StatusCode, Msg: msg}
	}
	var op struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &op); err != nil {
		return nil, fmt.Errorf("googleai: decode submit response: %w", err)
	}
	if op.Name == "" {
		return nil, errors.New("googleai: submit response missing operation name")
	}
	return adapter.AsyncSubmit{
		UpstreamRef: adapter.UpstreamRef(op.Name),
		At:          time.Now().UTC(),
	}, nil
}

// Poll fetches operation status. Returns PollRunning while done=false,
// PollSucceeded with the normalized result on success, PollFailed on
// upstream error.
func (a *GoogleVertexAIAdapter) Poll(
	ctx context.Context,
	model adapter.ModelKey,
	ref adapter.UpstreamRef,
) (adapter.PollResult, error) {
	if ref == "" {
		return adapter.PollResult{}, fmt.Errorf("googleai: %w: empty upstream ref", adapter.ErrInvalidParams)
	}
	url, err := a.cfg.pollURL(string(ref))
	if err != nil {
		return adapter.PollResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return adapter.PollResult{}, fmt.Errorf("googleai: build poll request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, raw, err := a.do(req)
	if err != nil {
		return adapter.PollResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		class, msg := classifyHTTPError(resp.StatusCode, raw)
		return adapter.PollResult{
			Status: adapter.PollFailed,
			Error: &adapter.PollError{
				Class:   class,
				Message: msg,
				Raw:     capRaw(raw),
			},
		}, nil
	}
	op, err := parseOperation(raw)
	if err != nil {
		return adapter.PollResult{}, err
	}
	return pollFromOperation(model, op, raw)
}

// Cancel POSTs to {operation}:cancel. Vertex AI returns 200 with an empty
// body when cancellation is accepted; the operation may still complete
// shortly thereafter (Cancel is best-effort, per Google's spec).
func (a *GoogleVertexAIAdapter) Cancel(
	ctx context.Context,
	model adapter.ModelKey,
	ref adapter.UpstreamRef,
) error {
	if ref == "" {
		return fmt.Errorf("googleai: %w: empty upstream ref", adapter.ErrInvalidParams)
	}
	url, err := a.cfg.cancelURL(string(ref))
	if err != nil {
		return err
	}
	// :cancel takes an empty JSON body per the LRO spec.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("googleai: build cancel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, raw, err := a.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		_, msg := classifyHTTPError(resp.StatusCode, raw)
		return fmt.Errorf("googleai: cancel failed: %s", msg)
	}
	return nil
}

// EstimateCost returns the cost in micro-USD for a single Submit. Rounds up
// to the nearest second; if duration_seconds is missing or out of range,
// uses the default of 5s (matches the input-schema default).
func (a *GoogleVertexAIAdapter) EstimateCost(model adapter.ModelKey, params adapter.Params) (adapter.CostUSD, error) {
	upstream, ok := a.manifest[model]
	if !ok {
		return 0, fmt.Errorf("googleai: %w: unknown model %q", adapter.ErrInvalidParams, model)
	}
	perSec, ok := veo3PerSecondCostMicroUSD[upstream]
	if !ok {
		return 0, fmt.Errorf("googleai: no pricing table entry for upstream model %q", upstream)
	}
	dur := durationSecondsFrom(params)
	if dur <= 0 {
		dur = 5
	}
	if dur > 30 {
		dur = 30
	}
	cost := perSec * adapter.CostUSD(dur)
	if cost > adapter.MaxCostUSD {
		return 0, fmt.Errorf("googleai: %w", adapter.ErrCostCeilingExceeded)
	}
	return cost, nil
}

// Capabilities returns per-model feature flags.
func (a *GoogleVertexAIAdapter) Capabilities(model adapter.ModelKey) adapter.ProviderCaps {
	return adapter.ProviderCaps{
		SupportsWebhook:   false,
		SupportsCancel:    true,
		SupportsStreaming: false,
		// MaxConcurrent is intentionally 0 (= no hint). Real concurrency is
		// gated by per-project Vertex AI quota; ticket T-002 (S15) tracks
		// per-region/project tuning.
		MaxConcurrent: 0,
	}
}

// NormalizeResult parses the captured operation JSON in raw and returns a
// NormalizedResult. Used in tests against testdata/ goldens; the worker
// also calls it inside Poll on the success branch via pollFromOperation.
func (a *GoogleVertexAIAdapter) NormalizeResult(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	op, err := parseOperation(raw)
	if err != nil {
		return nil, err
	}
	return normalizeOperationResponse(model, op)
}

// VerifyWebhook returns ErrUnsupported. Vertex AI's Eventarc integration is
// out of MVP scope; a future agent who wires it up should keep this method
// in sync with that work.
func (a *GoogleVertexAIAdapter) VerifyWebhook(headers http.Header, body []byte) (*adapter.WebhookVerification, error) {
	return nil, adapter.ErrUnsupported
}

// do issues an HTTP request and returns (response, body, error). The body
// is read fully here so callers can branch on status/parse without holding
// open a network read. err is non-nil only for transport-level failures.
func (a *GoogleVertexAIAdapter) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := a.cfg.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("googleai: http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Read up to a generous cap (16 MiB) to absorb Vertex AI's
	// response-with-base64 path while staying memory-bounded.
	const maxResponseBytes = 16 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("googleai: read response body: %w", err)
	}
	return resp, body, nil
}

// httpError lets the wrapper layer retrieve the ErrorClass without
// importing this package's internal helpers. Callers can errors.As against
// *httpError to recover the class.
type httpError struct {
	Class  adapter.ErrorClass
	Status int
	Msg    string
}

func (e *httpError) Error() string { return e.Msg }

// ErrorClass is exposed via this method so wrapper code can switch on it
// without depending on the concrete struct shape.
func (e *httpError) ErrorClass() adapter.ErrorClass { return e.Class }

// buildSubmitBody translates our canonical Params into the upstream wire
// format. Vertex AI predictLongRunning expects:
//
//	{"instances":[{"prompt":"..."}],"parameters":{"sampleCount":1,...}}
//
// instances/parameters are intentionally constructed here, NOT exposed as
// Params keys, to keep the public interface upstream-agnostic.
func buildSubmitBody(params adapter.Params) ([]byte, error) {
	prompt, _ := params["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("googleai: %w: prompt is required", adapter.ErrInvalidParams)
	}
	instance := map[string]any{"prompt": prompt}
	if neg, ok := params["negative_prompt"].(string); ok && neg != "" {
		instance["negativePrompt"] = neg
	}

	parameters := map[string]any{
		"sampleCount": 1,
	}
	if dur := durationSecondsFrom(params); dur > 0 {
		parameters["durationSeconds"] = dur
	}
	if aspect, ok := params["aspect_ratio"].(string); ok && aspect != "" {
		parameters["aspectRatio"] = aspect
	}
	if seed, ok := intFrom(params["seed"]); ok {
		parameters["seed"] = seed
	}
	if pg, ok := params["person_generation"].(string); ok && pg != "" {
		parameters["personGeneration"] = pg
	}

	return json.Marshal(map[string]any{
		"instances":  []any{instance},
		"parameters": parameters,
	})
}

func durationSecondsFrom(params adapter.Params) int {
	if v, ok := intFrom(params["duration_seconds"]); ok {
		return v
	}
	return 0
}

// intFrom coerces JSON-decoded numbers (which may be float64) to int.
func intFrom(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int32:
		return int(t), true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case float32:
		return int(t), true
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}
