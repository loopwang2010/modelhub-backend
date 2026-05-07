package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// fakeManifestStore implements ManifestLookup for tests.
type fakeManifestStore struct {
	m map[adapter.ModelKey]*catalog.ModelManifest
}

func newFakeManifestStore(ms ...*catalog.ModelManifest) *fakeManifestStore {
	s := &fakeManifestStore{m: make(map[adapter.ModelKey]*catalog.ModelManifest)}
	for _, manifest := range ms {
		s.m[manifest.Key] = manifest
	}
	return s
}

func (s *fakeManifestStore) FindByKey(key adapter.ModelKey) (*catalog.ModelManifest, bool) {
	v, ok := s.m[key]
	return v, ok
}

// fakeRegistry implements AdapterRegistry for tests.
type fakeRegistry struct {
	keys map[adapter.ProviderKey]adapter.ProviderAdapter
}

func newFakeRegistry(keys ...adapter.ProviderKey) *fakeRegistry {
	r := &fakeRegistry{keys: make(map[adapter.ProviderKey]adapter.ProviderAdapter)}
	for _, k := range keys {
		r.keys[k] = adapter.NewMockSyncAdapter()
	}
	return r
}

func (r *fakeRegistry) Get(key adapter.ProviderKey) (adapter.ProviderAdapter, bool) {
	v, ok := r.keys[key]
	return v, ok
}

func enabledManifest() *catalog.ModelManifest {
	return &catalog.ModelManifest{
		Key:         "flux-pro-1.1",
		Name:        "Flux Pro 1.1",
		Modality:    adapter.ModalityImage,
		TaskKind:    adapter.TaskKindAsync,
		Provider:    "bfl",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Enabled:     true,
	}
}

func disabledManifest() *catalog.ModelManifest {
	m := enabledManifest()
	m.Enabled = false
	return m
}

func runMiddleware(t *testing.T, m *catalog.ModelManifest, providers []adapter.ProviderKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	store := newFakeManifestStore(m)
	reg := newFakeRegistry(providers...)
	mw := RequireModelExists(store, reg)
	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// Body should still be readable.
		bytes, _ := io.ReadAll(r.Body)
		if len(bytes) == 0 {
			t.Error("downstream got empty body — middleware did not re-attach")
		}
		// Context should carry the parsed request and manifest.
		if _, ok := RequestFromContext(r.Context()); !ok {
			t.Error("RequestFromContext returned false")
		}
		if _, ok := ManifestFromContext(r.Context()); !ok {
			t.Error("ManifestFromContext returned false")
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(final)
	req := httptest.NewRequest(http.MethodPost, "/v1/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK && !called {
		t.Error("downstream not invoked despite 200")
	}
	return rr
}

func TestRequireModelExists_HappyPath(t *testing.T) {
	rr := runMiddleware(t,
		enabledManifest(),
		[]adapter.ProviderKey{"bfl"},
		`{"model":"flux-pro-1.1","params":{"prompt":"x"}}`,
	)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRequireModelExists_ModelNotFound(t *testing.T) {
	store := newFakeManifestStore() // empty
	reg := newFakeRegistry("bfl")
	handler := RequireModelExists(store, reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream invoked despite missing manifest")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/generations", strings.NewReader(`{"model":"missing","params":{"a":1}}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "model_not_found") {
		t.Errorf("body missing model_not_found code: %s", rr.Body.String())
	}
}

func TestRequireModelExists_DisabledModel(t *testing.T) {
	store := newFakeManifestStore(disabledManifest())
	reg := newFakeRegistry("bfl")
	handler := RequireModelExists(store, reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream invoked despite disabled model")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/generations", strings.NewReader(`{"model":"flux-pro-1.1","params":{}}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestRequireModelExists_ProviderUnregistered(t *testing.T) {
	store := newFakeManifestStore(enabledManifest())
	reg := newFakeRegistry() // no providers
	handler := RequireModelExists(store, reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream invoked despite missing provider")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/generations", strings.NewReader(`{"model":"flux-pro-1.1","params":{}}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestRequireModelExists_InvalidJSON(t *testing.T) {
	store := newFakeManifestStore(enabledManifest())
	reg := newFakeRegistry("bfl")
	handler := RequireModelExists(store, reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream invoked")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/generations", strings.NewReader(`not-json`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid_json") {
		t.Errorf("expected invalid_json: %s", rr.Body.String())
	}
}

func TestRequireModelExists_StructurallyInvalid(t *testing.T) {
	store := newFakeManifestStore(enabledManifest())
	reg := newFakeRegistry("bfl")
	handler := RequireModelExists(store, reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream invoked")
	}))
	// Empty model key
	req := httptest.NewRequest(http.MethodPost, "/v1/generations", strings.NewReader(`{"params":{"x":1}}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestRequireModelExists_PanicsOnNilDeps(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil ManifestLookup")
		}
	}()
	RequireModelExists(nil, newFakeRegistry())
}

func TestRequireModelExists_PanicsOnNilRegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil AdapterRegistry")
		}
	}()
	RequireModelExists(newFakeManifestStore(), nil)
}

func TestRequestFromContext_AbsentReturnsFalse(t *testing.T) {
	if _, ok := RequestFromContext(context.Background()); ok {
		t.Error("expected false on bare context")
	}
}

func TestManifestFromContext_AbsentReturnsFalse(t *testing.T) {
	if _, ok := ManifestFromContext(context.Background()); ok {
		t.Error("expected false on bare context")
	}
}
