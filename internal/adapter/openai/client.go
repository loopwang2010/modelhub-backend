// Package openai implements the ProviderAdapter for OpenAI's image-edit API.
//
// This file wires the HTTP client and credential plumbing for the
// `Authorization: Bearer ...` pattern. The OpenAI image API uses a
// standard bearer token (never `x-api-key`, never `x-key`) — see
// plans/S9-OPENAI-API-RESEARCH.md for the auth comparison matrix.
//
// Configuration is environment-variable driven:
//
//	OPENAI_API_KEY  — required for the adapter to register (else
//	                  ErrNotConfigured is returned at construction)
//	OPENAI_API_BASE — optional override; defaults to https://api.openai.com
//
// The adapter never logs or echoes the key — the only place credentials
// touch the wire is the outbound Authorization header.

package openai

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// envAPIKey is the canonical environment variable for OpenAI credentials.
const envAPIKey = "OPENAI_API_KEY"

// envAPIBase optionally overrides the API base URL (e.g. for staging,
// regional endpoints, or a self-hosted compatible gateway in tests).
const envAPIBase = "OPENAI_API_BASE"

// defaultAPIBase is the canonical OpenAI API root.
const defaultAPIBase = "https://api.openai.com"

// editsPath is the image-edit endpoint relative to the base URL.
const editsPath = "/v1/images/edits"

// defaultHTTPTimeout is the per-request timeout. Image edits routinely
// take 5-15s — we allow generous headroom but cap to prevent zombie
// goroutines from leaked context cancellations.
const defaultHTTPTimeout = 60 * time.Second

// httpClient is the minimal HTTP surface this adapter uses. Carved out
// as an interface so tests can swap in httptest servers without spinning
// real net.Dialers. Production wires *http.Client directly.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// clientConfig holds the resolved configuration for the adapter.
// Field order: required first (apiKey), optional second (baseURL, http).
type clientConfig struct {
	apiKey  string
	baseURL string
	http    httpClient
}

// loadClientConfig reads OPENAI_API_KEY / OPENAI_API_BASE from the env
// and returns a populated clientConfig. Returns ErrNotConfigured when
// the required key is missing — callers in init() must treat this as a
// "skip registration" signal, not a fatal.
func loadClientConfig() (*clientConfig, error) {
	apiKey := strings.TrimSpace(os.Getenv(envAPIKey))
	if apiKey == "" {
		return nil, adapter.ErrNotConfigured
	}
	base := strings.TrimSpace(os.Getenv(envAPIBase))
	if base == "" {
		base = defaultAPIBase
	}
	base = strings.TrimRight(base, "/")
	return &clientConfig{
		apiKey:  apiKey,
		baseURL: base,
		http:    &http.Client{Timeout: defaultHTTPTimeout},
	}, nil
}

// withHTTPClient lets tests inject an httptest server's client (or any
// compatible Doer). Returns a copy so callers can keep the original
// config immutable.
func (c *clientConfig) withHTTPClient(h httpClient) *clientConfig {
	dup := *c
	dup.http = h
	return &dup
}

// editsURL is the absolute URL to POST multipart edit requests to.
func (c *clientConfig) editsURL() string {
	return c.baseURL + editsPath
}

// applyAuth installs the Authorization: Bearer header on the request.
// Pulled out so the multipart-build path and (future) JSON-build path
// share one auth surface.
func (c *clientConfig) applyAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}
