// Middleware for the envelope-aware /v1 surface.
//
// RequireModelExists is the gateway-side validation that blocks requests
// for models we haven't registered. Without this, S5's worker would
// attempt to dispatch through a missing adapter and the failure mode
// would be a 500 — not a clean 400.
//
// Design notes:
//   - These middlewares are framework-agnostic — they take an http.Handler
//     and return one. The router (gin in our case) wraps them at mount.
//   - Manifest lookup is decoupled via a ManifestLookup interface so tests
//     can inject any backing store. S4's catalog package will provide the
//     concrete implementation.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// ManifestLookup is a small interface satisfied by S4's catalog service.
// Defined here (not in catalog) following accept-interfaces-return-structs.
type ManifestLookup interface {
	// FindByKey returns the manifest for the given model key, or nil + false
	// when no such model exists. Lookups are O(1) and safe for concurrent use.
	FindByKey(key adapter.ModelKey) (*catalog.ModelManifest, bool)
}

// AdapterRegistry is the subset of adapter.Registry that middleware needs.
// Defined as an interface so tests can inject fakes.
type AdapterRegistry interface {
	Get(key adapter.ProviderKey) (adapter.ProviderAdapter, bool)
}

// RequireModelExists is HTTP middleware for POST /v1/generations. It:
//  1. Parses GenerationRequest from the body
//  2. Verifies request structural validity
//  3. Looks up the manifest by Model
//  4. Verifies the manifest's Provider has a registered adapter
//  5. Re-attaches the consumed body so the downstream handler can read it
//     again (request body is single-shot otherwise).
//
// On any failure, writes a 400/403/404/503 JSON error response and short-circuits.
func RequireModelExists(manifests ManifestLookup, registry AdapterRegistry) func(http.Handler) http.Handler {
	if manifests == nil {
		panic("api: RequireModelExists requires non-nil ManifestLookup")
	}
	if registry == nil {
		panic("api: RequireModelExists requires non-nil AdapterRegistry")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "failed to read body")
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			var req GenerationRequest
			if err := json.Unmarshal(body, &req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
				return
			}
			if err := req.Validate(); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}

			manifest, ok := manifests.FindByKey(req.Model)
			if !ok {
				writeJSONError(w, http.StatusNotFound, "model_not_found", "model "+string(req.Model)+" is not registered")
				return
			}
			if !manifest.Enabled {
				writeJSONError(w, http.StatusForbidden, "model_disabled", "model "+string(req.Model)+" is currently disabled")
				return
			}
			if _, ok := registry.Get(manifest.Provider); !ok {
				// Manifest exists but no adapter — production misconfiguration.
				// 503 is more correct than 500: retry-able from the client side.
				writeJSONError(w, http.StatusServiceUnavailable, "provider_unavailable", "no adapter registered for this model's provider")
				return
			}

			// Stash the parsed request and manifest in the request context for
			// downstream handlers via the typed accessors below.
			ctx := withRequest(r.Context(), &req)
			ctx = withManifest(ctx, manifest)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeJSONError writes the standard error envelope. The envelope's outer
// shape is a top-level "error" key matching the Error struct so clients can
// branch on `error.code`.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := map[string]any{
		"error": Error{Code: code, Message: message},
	}
	_ = json.NewEncoder(w).Encode(body)
}

// ─────────────────────────────────────────────────────────────────────────
// Context plumbing
// ─────────────────────────────────────────────────────────────────────────

type ctxKey int

const (
	ctxKeyRequest ctxKey = iota + 1
	ctxKeyManifest
)

func withRequest(ctx context.Context, req *GenerationRequest) context.Context {
	return context.WithValue(ctx, ctxKeyRequest, req)
}

func withManifest(ctx context.Context, m *catalog.ModelManifest) context.Context {
	return context.WithValue(ctx, ctxKeyManifest, m)
}

// RequestFromContext returns the parsed GenerationRequest stashed by
// RequireModelExists. Returns nil + false when the middleware did not run.
func RequestFromContext(ctx context.Context) (*GenerationRequest, bool) {
	v, ok := ctx.Value(ctxKeyRequest).(*GenerationRequest)
	return v, ok
}

// ManifestFromContext returns the resolved manifest, or nil + false.
func ManifestFromContext(ctx context.Context) (*catalog.ModelManifest, bool) {
	v, ok := ctx.Value(ctxKeyManifest).(*catalog.ModelManifest)
	return v, ok
}
