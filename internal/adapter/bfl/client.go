// Package bfl is the Black Forest Labs (api.bfl.ai) provider adapter.
//
// This package owns ALL knowledge of BFL's request/response shapes, auth
// header (`x-key`), polling URL convention, error vocabulary, and per-model
// capability matrix. Per AP-1, none of this leaks outside this package.
//
// Reference: plans/S7-S8-API-RESEARCH.md §1 (the implementation spec) and
// plans/TOS-RESEARCH.md §5 (license-tier matrix that distinguishes
// flux-pro / flux-schnell / flux-dev).
package bfl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// envAPIKey is the env var consulted at process start. Documented per S7
// research (§1, "Default env var convention: BFL_API_KEY").
const envAPIKey = "BFL_API_KEY"

// defaultBaseURL is the canonical production endpoint. Tests override this.
// Regional endpoints (api.eu.bfl.ai / api.us.bfl.ai) exist for latency only
// and do not change the request shape.
const defaultBaseURL = "https://api.bfl.ai"

// defaultHTTPTimeout caps a single HTTP round-trip. 30s is generous: BFL's
// Submit response is small (<200 bytes); Poll response too. Anything longer
// is almost certainly a stuck connection — fail fast and let the worker
// re-poll on the next tick (per AP-3 the adapter does NOT sleep itself).
const defaultHTTPTimeout = 30 * time.Second

// idempotencyHeader is the header BFL accepts as an upstream dedup hint.
// Note from research T-004: BFL has not publicly documented an idempotency
// header; we forward `X-Idempotency-Key` on the chance they accept it
// silently. The wrapper layer in S2.5 owns true dedup (per C4 contract),
// so a header mismatch costs us nothing.
const idempotencyHeader = "X-Idempotency-Key"

// authHeader is BFL's API-key header (NOT `Authorization: Bearer ...`).
const authHeader = "x-key"

// client is the minimal HTTP wrapper used by the adapter.
//
// Construction is via newClient* helpers, never the zero value: missing
// API key would race with init() and produce confusing nil-pointer panics
// inside Submit.
type client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// newClientFromEnv constructs a client from BFL_API_KEY. Returns
// adapter.ErrNotConfigured (wrapped) when the var is missing, so init()
// can decide to skip registration.
func newClientFromEnv() (*client, error) {
	key := os.Getenv(envAPIKey)
	if key == "" {
		return nil, fmt.Errorf("bfl: %w: %s is not set", adapter.ErrNotConfigured, envAPIKey)
	}
	return &client{
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		baseURL:    defaultBaseURL,
		apiKey:     key,
	}, nil
}

// newClientForTesting constructs a client pointing at a test HTTP server.
// Used only by tests in this package.
func newClientForTesting(baseURL, apiKey string) *client {
	return &client{
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}
}

// submitURL returns the per-model POST endpoint, e.g.,
// `https://api.bfl.ai/v1/flux-pro-1.1`.
func (c *client) submitURL(upstreamModel string) string {
	return fmt.Sprintf("%s/v1/%s", c.baseURL, upstreamModel)
}

// doJSON encodes body as JSON, POSTs to the URL with auth + idempotency
// headers, and decodes the response into out. errClass returns a
// best-effort adapter.ErrorClass when the upstream HTTP code maps cleanly.
func (c *client) doJSON(ctx context.Context, method, url string, idempotencyKey string, body any, out any) (status int, errClass adapter.ErrorClass, err error) {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, "", fmt.Errorf("bfl: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, "", fmt.Errorf("bfl: build request: %w", err)
	}
	req.Header.Set(authHeader, c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set(idempotencyHeader, idempotencyKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network error — surface as upstream class so the worker refunds.
		return 0, adapter.ErrClassUpstream, fmt.Errorf("bfl: http: %w", err)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return resp.StatusCode, adapter.ErrClassUpstream, fmt.Errorf("bfl: read response: %w", readErr)
	}

	if resp.StatusCode >= 400 {
		return resp.StatusCode, classifyHTTPStatus(resp.StatusCode), fmt.Errorf("bfl: upstream %d: %s", resp.StatusCode, truncateForError(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp.StatusCode, adapter.ErrClassUpstream, fmt.Errorf("bfl: decode response: %w", err)
		}
	}
	return resp.StatusCode, "", nil
}

// maxResponseBytes caps the response body we will read. BFL responses are
// small; this guards against a misbehaving upstream streaming megabytes.
const maxResponseBytes = 1 << 20 // 1 MiB

// classifyHTTPStatus maps HTTP status to ErrorClass per S7-S8-API-RESEARCH.md §1.
func classifyHTTPStatus(status int) adapter.ErrorClass {
	switch {
	case status == http.StatusUnauthorized:
		return adapter.ErrClassAuth
	case status == http.StatusPaymentRequired:
		return adapter.ErrClassPayment
	case status == http.StatusTooManyRequests:
		return adapter.ErrClassRateLimit
	case status >= 500:
		return adapter.ErrClassUpstream
	case status == http.StatusNotFound:
		return adapter.ErrClassNotFound
	default:
		return adapter.ErrClassUnknown
	}
}

// truncateForError keeps logs from blowing up if upstream returns a stack
// trace. 256 bytes is plenty to identify the failure shape for ops.
func truncateForError(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
