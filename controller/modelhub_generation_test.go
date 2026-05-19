package controller

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
	modelhubapi "github.com/QuantumNous/new-api/internal/api"
	"github.com/QuantumNous/new-api/internal/catalog"
)

func resetGenerationCacheForTest() {
	generationCache.Lock()
	defer generationCache.Unlock()
	generationCache.byID = make(map[string]generationCacheEntry)
	generationCache.byIdem = make(map[string]string)
}

func installGenerationRouteForTest(t *testing.T, model adapter.ModelKey) *adapter.MockSyncAdapter {
	t.Helper()
	resetGenerationCacheForTest()
	SetWallet(nil, nil, nil)

	mock := adapter.NewMockSyncAdapter()
	prev, err := adapter.DefaultRegistry().Replace(mock)
	if err != nil {
		t.Fatalf("install mock adapter: %v", err)
	}
	t.Cleanup(func() {
		if prev != nil {
			_, _ = adapter.DefaultRegistry().Replace(prev)
		} else {
			adapter.DefaultRegistry().Unregister(mock.Key())
		}
		resetGenerationCacheForTest()
		SetWallet(nil, nil, nil)
	})

	if err := catalog.Register(catalog.ModelManifest{
		Key:           model,
		Name:          "Controller Test Model",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindSync,
		Provider:      mock.Key(),
		UpstreamModel: "controller-test-upstream",
		InputSchema:   json.RawMessage(`{"type":"object"}`),
		PriceFormula:  "test",
	}); err != nil {
		t.Fatalf("register test manifest: %v", err)
	}

	return mock
}

func submitGenerationForTest(t *testing.T, userID int, body map[string]any) (int, modelhubapi.GenerationResponse) {
	t.Helper()
	c, w := newGinCtx(http.MethodPost, "/v1/generations", body, userID, 0)

	ModelhubSubmitGeneration(c)

	var resp modelhubapi.GenerationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response status=%d body=%s: %v", w.Code, w.Body.String(), err)
	}
	return w.Code, resp
}

func waitForSubmitCount(t *testing.T, mock *adapter.MockSyncAdapter, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := mock.SubmitCount.Load(); got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("submit count = %d, want at least %d", mock.SubmitCount.Load(), want)
}

func TestModelhubSubmitGeneration_NoExplicitIdempotencyDoesNotDedup(t *testing.T) {
	model := adapter.ModelKey("controller-test-no-idem")
	mock := installGenerationRouteForTest(t, model)
	body := map[string]any{
		"model": string(model),
		"params": map[string]any{
			"prompt": "same prompt",
		},
	}

	code1, resp1 := submitGenerationForTest(t, 7001, body)
	code2, resp2 := submitGenerationForTest(t, 7001, body)

	if code1 != http.StatusAccepted || code2 != http.StatusAccepted {
		t.Fatalf("statuses = %d/%d, want 202/202", code1, code2)
	}
	if resp1.ID == "" || resp2.ID == "" {
		t.Fatalf("generation ids must be non-empty: %q/%q", resp1.ID, resp2.ID)
	}
	if resp1.ID == resp2.ID {
		t.Fatalf("two ordinary batch submissions reused generation id %q", resp1.ID)
	}
	waitForSubmitCount(t, mock, 2)
}

func TestModelhubSubmitGeneration_ExplicitIdempotencyStillDedups(t *testing.T) {
	model := adapter.ModelKey("controller-test-explicit-idem")
	mock := installGenerationRouteForTest(t, model)
	body := map[string]any{
		"model":           string(model),
		"idempotency_key": "controller-test-idem-key",
		"params": map[string]any{
			"prompt": "same prompt",
		},
	}

	_, resp1 := submitGenerationForTest(t, 7002, body)
	_, resp2 := submitGenerationForTest(t, 7002, body)

	if resp1.ID == "" || resp2.ID == "" {
		t.Fatalf("generation ids must be non-empty: %q/%q", resp1.ID, resp2.ID)
	}
	if resp1.ID != resp2.ID {
		t.Fatalf("explicit idempotency key did not reuse generation id: %q/%q", resp1.ID, resp2.ID)
	}
	waitForSubmitCount(t, mock, 1)
	if got := mock.SubmitCount.Load(); got != 1 {
		t.Fatalf("submit count = %d, want 1", got)
	}
}
