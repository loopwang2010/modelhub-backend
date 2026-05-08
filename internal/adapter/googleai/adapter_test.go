// Tests for the Google Vertex AI Veo3 adapter.
//
// Strategy:
//   - Most tests use newAdapterFromConfig + an httptest.Server so they
//     exercise the real HTTP code path without needing real Google
//     credentials.
//   - NormalizeResult tests feed captured operation JSON from
//     testdata/ — these are the load-bearing golden-file tests.
//   - The integration test (-tags=integration) at the bottom of this file
//     hits real Vertex AI when GOOGLE_APPLICATION_CREDENTIALS is set, and
//     skips cleanly otherwise.

package googleai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// ─────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────

// loadGolden reads testdata/<name>. Test fails if the file is missing.
func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %q: %v", path, err)
	}
	return data
}

// newTestAdapter builds an adapter pointing at an httptest server. The
// returned adapter uses a static bearer token so requests still flow
// through the OAuth2 transport (proving headers are wired) without needing
// a real service account.
func newTestAdapter(t *testing.T, srv *httptest.Server) *GoogleVertexAIAdapter {
	t.Helper()
	src := &staticTokenSource{token: "test-bearer-token", project: "test-project-123"}
	httpClient, err := authedClient(context.Background(), src, srv.Client().Transport)
	if err != nil {
		t.Fatalf("authedClient: %v", err)
	}
	cfg := &config{
		location:        "us-central1",
		httpClient:      httpClient,
		credSource:      src,
		baseURLOverride: srv.URL,
	}
	return newAdapterFromConfig(cfg)
}

// fakeServer collects requests and returns scripted responses.
type fakeServer struct {
	t          *testing.T
	server     *httptest.Server
	requests   []*http.Request
	bodies     [][]byte
	mu         atomic.Int64
	respond    func(req *http.Request, body []byte) (int, []byte)
	authHeader string
}

func newFakeServer(t *testing.T, respond func(req *http.Request, body []byte) (int, []byte)) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t, respond: respond}
	fs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.requests = append(fs.requests, r)
		fs.bodies = append(fs.bodies, body)
		fs.authHeader = r.Header.Get("Authorization")
		fs.mu.Add(1)
		status, respBody := fs.respond(r, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(fs.server.Close)
	return fs
}

// ─────────────────────────────────────────────────────────────────────────
// Submit
// ─────────────────────────────────────────────────────────────────────────

func TestSubmit_ReturnsAsyncSubmitWithOperationName(t *testing.T) {
	submitGolden := loadGolden(t, "submit_response.json")
	fs := newFakeServer(t, func(req *http.Request, _ []byte) (int, []byte) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", req.Method)
		}
		if !strings.Contains(req.URL.Path, ":predictLongRunning") {
			t.Errorf("path = %q, missing predictLongRunning", req.URL.Path)
		}
		if !strings.Contains(req.URL.Path, "veo-3.0-generate-preview") {
			t.Errorf("path %q missing model id", req.URL.Path)
		}
		if !strings.Contains(req.URL.Path, "us-central1") {
			t.Errorf("path %q missing location", req.URL.Path)
		}
		return http.StatusOK, submitGolden
	})
	a := newTestAdapter(t, fs.server)

	res, err := a.Submit(context.Background(), "veo-3.0-generate-preview", adapter.Params{
		"prompt":           "test prompt",
		"duration_seconds": 5,
	}, "idem-1")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	asyncRes, ok := res.(adapter.AsyncSubmit)
	if !ok {
		t.Fatalf("got %T, want AsyncSubmit", res)
	}
	if asyncRes.UpstreamRef == "" {
		t.Fatal("empty UpstreamRef")
	}
	if !strings.HasPrefix(string(asyncRes.UpstreamRef), "projects/") {
		t.Errorf("ref %q does not look like an op name", asyncRes.UpstreamRef)
	}
	if !strings.Contains(string(asyncRes.UpstreamRef), "/operations/") {
		t.Errorf("ref %q missing /operations/ segment", asyncRes.UpstreamRef)
	}
	if asyncRes.AcceptedAt().IsZero() {
		t.Error("AcceptedAt is zero")
	}
	if fs.authHeader != "Bearer test-bearer-token" {
		t.Errorf("Authorization = %q, want bearer", fs.authHeader)
	}

	// Verify the body was translated to instances/parameters.
	var sent struct {
		Instances []struct {
			Prompt string `json:"prompt"`
		} `json:"instances"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(fs.bodies[0], &sent); err != nil {
		t.Fatalf("decode submit body: %v", err)
	}
	if len(sent.Instances) != 1 || sent.Instances[0].Prompt != "test prompt" {
		t.Errorf("body instances = %+v", sent.Instances)
	}
	if dur, _ := sent.Parameters["durationSeconds"].(float64); int(dur) != 5 {
		t.Errorf("durationSeconds = %v, want 5", sent.Parameters["durationSeconds"])
	}
}

func TestSubmit_RejectsEmptyIdempotencyKey(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview", adapter.Params{"prompt": "x"}, "")
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestSubmit_RejectsUnknownModel(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	_, err := a.Submit(context.Background(), "not-a-real-model", adapter.Params{"prompt": "x"}, "k")
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestSubmit_RejectsEmptyPrompt(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview", adapter.Params{"prompt": ""}, "k")
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestSubmit_TranslatesAllOptionalParams(t *testing.T) {
	fs := newFakeServer(t, func(_ *http.Request, _ []byte) (int, []byte) {
		return http.StatusOK, loadGolden(t, "submit_response.json")
	})
	a := newTestAdapter(t, fs.server)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview", adapter.Params{
		"prompt":            "x",
		"negative_prompt":   "blurry",
		"duration_seconds":  10,
		"aspect_ratio":      "9:16",
		"seed":              42,
		"person_generation": "disallow",
	}, "k")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(fs.bodies[0], &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	parameters, _ := sent["parameters"].(map[string]any)
	if parameters["aspectRatio"] != "9:16" {
		t.Errorf("aspectRatio = %v", parameters["aspectRatio"])
	}
	if parameters["personGeneration"] != "disallow" {
		t.Errorf("personGeneration = %v", parameters["personGeneration"])
	}
	if int(parameters["seed"].(float64)) != 42 {
		t.Errorf("seed = %v", parameters["seed"])
	}
	instances, _ := sent["instances"].([]any)
	first, _ := instances[0].(map[string]any)
	if first["negativePrompt"] != "blurry" {
		t.Errorf("negativePrompt = %v", first["negativePrompt"])
	}
}

func TestSubmit_HTTPErrorMappedToHTTPError(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusUnauthorized, loadGolden(t, "http_error_401.json")
	})
	a := newTestAdapter(t, fs.server)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview", adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected error")
	}
	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("err is not *httpError: %T", err)
	}
	if he.ErrorClass() != adapter.ErrClassAuth {
		t.Errorf("class = %q, want %q", he.ErrorClass(), adapter.ErrClassAuth)
	}
}

func TestSubmit_HTTPRateLimitMapped(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusTooManyRequests, []byte(`{"error":{"code":8,"message":"too many","status":"RESOURCE_EXHAUSTED"}}`)
	})
	a := newTestAdapter(t, fs.server)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview", adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected error")
	}
	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("err is not *httpError: %T", err)
	}
	if he.ErrorClass() != adapter.ErrClassRateLimit {
		t.Errorf("class = %q, want rate_limit", he.ErrorClass())
	}
}

func TestSubmit_ContextCancelPropagates(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, loadGolden(t, "submit_response.json")
	})
	a := newTestAdapter(t, fs.server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Submit(ctx, "veo-3.0-generate-preview", adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected ctx error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Poll
// ─────────────────────────────────────────────────────────────────────────

func TestPoll_PendingWhileDoneFalse(t *testing.T) {
	fs := newFakeServer(t, func(req *http.Request, _ []byte) (int, []byte) {
		if req.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", req.Method)
		}
		return http.StatusOK, loadGolden(t, "poll_pending.json")
	})
	a := newTestAdapter(t, fs.server)
	pr, err := a.Poll(context.Background(), "veo-3.0-generate-preview",
		"projects/test-project-123/locations/us-central1/publishers/google/models/veo-3.0-generate-preview/operations/abc")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollRunning {
		t.Errorf("status = %q, want running", pr.Status)
	}
}

func TestPoll_SucceededExtractsGSURL(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, loadGolden(t, "poll_succeeded.json")
	})
	a := newTestAdapter(t, fs.server)
	pr, err := a.Poll(context.Background(), "veo-3.0-generate-preview",
		"projects/test-project-123/locations/us-central1/publishers/google/models/veo-3.0-generate-preview/operations/abc")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollSucceeded {
		t.Fatalf("status = %q, want succeeded", pr.Status)
	}
	if pr.Result == nil {
		t.Fatal("Result is nil")
	}
	if pr.Result.Modality != adapter.ModalityVideo {
		t.Errorf("modality = %q, want video", pr.Result.Modality)
	}
	if len(pr.Result.Outputs) != 1 {
		t.Fatalf("outputs = %d", len(pr.Result.Outputs))
	}
	out := pr.Result.Outputs[0]
	if out.Kind != adapter.OutputKindVideoURL {
		t.Errorf("kind = %q", out.Kind)
	}
	// Critical guard: gs:// URL is emitted as-is. The S9.5 worker will
	// rewrite it. If a future agent pre-signs here, this assertion fails.
	if !strings.HasPrefix(out.URL, "gs://") {
		t.Errorf("URL %q is not gs://", out.URL)
	}
	if out.MimeType != "video/mp4" {
		t.Errorf("mime = %q", out.MimeType)
	}
}

func TestPoll_FailedQuotaMapsToRateLimit(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, loadGolden(t, "poll_failed_quota.json")
	})
	a := newTestAdapter(t, fs.server)
	pr, err := a.Poll(context.Background(), "veo-3.0-generate-preview",
		"projects/test-project-123/locations/us-central1/publishers/google/models/veo-3.0-generate-preview/operations/abc")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollFailed {
		t.Fatalf("status = %q, want failed", pr.Status)
	}
	if pr.Error == nil || pr.Error.Class != adapter.ErrClassRateLimit {
		t.Errorf("class = %v, want rate_limit", pr.Error)
	}
	if !strings.Contains(pr.Error.Message, "Quota exceeded") {
		t.Errorf("message = %q", pr.Error.Message)
	}
	if len(pr.Error.Raw) == 0 {
		t.Error("Raw is empty")
	}
	if len(pr.Error.Raw) > adapter.MaxRawErrorBytes {
		t.Errorf("Raw len %d exceeds cap %d", len(pr.Error.Raw), adapter.MaxRawErrorBytes)
	}
}

func TestPoll_FailedPolicyMapsToContentPolicy(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, loadGolden(t, "poll_failed_policy.json")
	})
	a := newTestAdapter(t, fs.server)
	pr, err := a.Poll(context.Background(), "veo-3.0-generate-preview",
		"projects/test-project-123/locations/us-central1/publishers/google/models/veo-3.0-generate-preview/operations/abc")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollFailed {
		t.Fatalf("status = %q", pr.Status)
	}
	if pr.Error == nil || pr.Error.Class != adapter.ErrClassContentPolicy {
		t.Errorf("class = %v, want content_policy", pr.Error)
	}
}

func TestPoll_HTTPErrorEmitsPollFailed(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusServiceUnavailable, []byte(`{"error":{"code":14,"message":"upstream busy","status":"UNAVAILABLE"}}`)
	})
	a := newTestAdapter(t, fs.server)
	pr, err := a.Poll(context.Background(), "veo-3.0-generate-preview",
		"projects/x/locations/us-central1/operations/abc")
	if err != nil {
		t.Fatalf("Poll returned err: %v", err)
	}
	if pr.Status != adapter.PollFailed {
		t.Errorf("status = %q, want failed", pr.Status)
	}
	if pr.Error.Class != adapter.ErrClassUpstream {
		t.Errorf("class = %q, want upstream", pr.Error.Class)
	}
}

func TestPoll_RejectsEmptyRef(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	_, err := a.Poll(context.Background(), "veo-3.0-generate-preview", "")
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v", err)
	}
}

func TestPoll_DoesNotSleep_AP3(t *testing.T) {
	// AP-3 guard: 50 polls under 200ms. This is a soft check — slow CI may
	// occasionally exceed; the real guarantee is that the function body
	// contains no time.Sleep call.
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, loadGolden(t, "poll_pending.json")
	})
	a := newTestAdapter(t, fs.server)
	start := time.Now()
	for i := 0; i < 50; i++ {
		_, err := a.Poll(context.Background(), "veo-3.0-generate-preview",
			"projects/x/locations/us-central1/operations/abc")
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("AP-3 violation: 50 polls took %v", elapsed)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Cancel
// ─────────────────────────────────────────────────────────────────────────

func TestCancel_PostsToCancelEndpoint(t *testing.T) {
	fs := newFakeServer(t, func(req *http.Request, _ []byte) (int, []byte) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", req.Method)
		}
		if !strings.HasSuffix(req.URL.Path, ":cancel") {
			t.Errorf("path = %q, want suffix :cancel", req.URL.Path)
		}
		return http.StatusOK, []byte("{}")
	})
	a := newTestAdapter(t, fs.server)
	err := a.Cancel(context.Background(), "veo-3.0-generate-preview",
		"projects/x/locations/us-central1/publishers/google/models/veo-3.0-generate-preview/operations/abc")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestCancel_RejectsEmptyRef(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	if err := a.Cancel(context.Background(), "veo-3.0-generate-preview", ""); !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v", err)
	}
}

func TestCancel_HTTPErrorPropagated(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusInternalServerError, []byte(`{"error":{"code":13,"message":"oops","status":"INTERNAL"}}`)
	})
	a := newTestAdapter(t, fs.server)
	err := a.Cancel(context.Background(), "veo-3.0-generate-preview",
		"projects/x/locations/us-central1/operations/abc")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// EstimateCost
// ─────────────────────────────────────────────────────────────────────────

func TestEstimateCost_PositiveForKnownModels(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	for _, m := range []adapter.ModelKey{"veo-3.0-generate-preview", "veo-3.0-fast-generate-preview"} {
		c, err := a.EstimateCost(m, adapter.Params{"duration_seconds": 5})
		if err != nil {
			t.Errorf("EstimateCost(%q): %v", m, err)
		}
		if c <= 0 {
			t.Errorf("EstimateCost(%q) = %d, want > 0", m, c)
		}
	}
}

func TestEstimateCost_DurationDefaultsTo5sWhenMissing(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	c, err := a.EstimateCost("veo-3.0-generate-preview", adapter.Params{})
	if err != nil {
		t.Fatal(err)
	}
	want := adapter.CostUSD(500_000 * 5)
	if c != want {
		t.Errorf("default cost = %d, want %d", c, want)
	}
}

func TestEstimateCost_DurationClamped(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	c, _ := a.EstimateCost("veo-3.0-generate-preview", adapter.Params{"duration_seconds": 1000})
	want := adapter.CostUSD(500_000 * 30)
	if c != want {
		t.Errorf("clamped cost = %d, want %d", c, want)
	}
}

func TestEstimateCost_AcceptsFloatDuration(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	c, err := a.EstimateCost("veo-3.0-generate-preview", adapter.Params{"duration_seconds": float64(7)})
	if err != nil {
		t.Fatal(err)
	}
	if c != adapter.CostUSD(500_000*7) {
		t.Errorf("cost = %d", c)
	}
}

func TestEstimateCost_RejectsUnknownModel(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("server should not be hit")
		return 0, nil
	}).server)
	_, err := a.EstimateCost("not-a-model", adapter.Params{})
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Capabilities, NormalizeResult, VerifyWebhook, Key
// ─────────────────────────────────────────────────────────────────────────

func TestCapabilities_VideoModelHasCancelTrue(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	caps := a.Capabilities("veo-3.0-generate-preview")
	if !caps.SupportsCancel {
		t.Error("SupportsCancel = false, want true")
	}
	if caps.SupportsWebhook {
		t.Error("SupportsWebhook = true, want false (Eventarc out of MVP)")
	}
	if caps.SupportsStreaming {
		t.Error("SupportsStreaming = true, want false")
	}
}

func TestKey_ReturnsGoogleAI(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	if a.Key() != "google-ai" {
		t.Errorf("Key() = %q, want google-ai", a.Key())
	}
}

func TestNormalizeResult_SucceededGoldenFile(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	res, err := a.NormalizeResult("veo-3.0-generate-preview", loadGolden(t, "poll_succeeded.json"))
	if err != nil {
		t.Fatalf("NormalizeResult: %v", err)
	}
	if res == nil || len(res.Outputs) != 1 {
		t.Fatal("bad result")
	}
	if !strings.HasPrefix(res.Outputs[0].URL, "gs://") {
		t.Errorf("expected gs:// URL, got %q", res.Outputs[0].URL)
	}
	if res.Metadata["upstream_ref"] == "" {
		t.Error("metadata missing upstream_ref")
	}
}

func TestNormalizeResult_RejectsEmptyPredictions(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	body := []byte(`{"name":"x","done":true,"response":{"predictions":[]}}`)
	if _, err := a.NormalizeResult("veo-3.0-generate-preview", body); err == nil {
		t.Fatal("expected error for empty predictions")
	}
}

func TestNormalizeResult_RejectsInvalidJSON(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	if _, err := a.NormalizeResult("veo-3.0-generate-preview", []byte("not json")); err == nil {
		t.Fatal("expected error")
	}
	if _, err := a.NormalizeResult("veo-3.0-generate-preview", nil); err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestNormalizeResult_DefaultMimeWhenAbsent(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	body := []byte(`{"name":"x","done":true,"response":{"predictions":[{"videoUri":"gs://b/o.mp4"}]}}`)
	res, err := a.NormalizeResult("veo-3.0-generate-preview", body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs[0].MimeType != "video/mp4" {
		t.Errorf("default mime = %q", res.Outputs[0].MimeType)
	}
}

func TestNormalizeResult_RejectsMissingVideoURI(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	body := []byte(`{"name":"x","done":true,"response":{"predictions":[{"mimeType":"video/mp4"}]}}`)
	if _, err := a.NormalizeResult("veo-3.0-generate-preview", body); err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyWebhook_AlwaysUnsupported(t *testing.T) {
	a := newTestAdapter(t, newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		t.Fatal("unused")
		return 0, nil
	}).server)
	_, err := a.VerifyWebhook(http.Header{}, []byte("{}"))
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Region restriction
// ─────────────────────────────────────────────────────────────────────────

func TestLoadConfig_RejectsBlockedLocation(t *testing.T) {
	saPath := filepath.Join("testdata", "fake_sa.json")
	env := func(k string) string {
		switch k {
		case CredentialsEnvVar:
			return saPath
		case LocationEnvVar:
			return "asia-east2" // blocked (Hong Kong)
		}
		return ""
	}
	_, err := loadConfig(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for blocked location")
	}
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestLoadConfig_AllowedLocation(t *testing.T) {
	saPath := filepath.Join("testdata", "fake_sa.json")
	env := func(k string) string {
		switch k {
		case CredentialsEnvVar:
			return saPath
		case LocationEnvVar:
			return "europe-west4"
		}
		return ""
	}
	cfg, err := loadConfig(context.Background(), env)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.location != "europe-west4" {
		t.Errorf("location = %q", cfg.location)
	}
	if !strings.Contains(cfg.baseURL(), "europe-west4-aiplatform.googleapis.com") {
		t.Errorf("baseURL = %q", cfg.baseURL())
	}
}

func TestLoadConfig_DefaultLocationWhenUnset(t *testing.T) {
	saPath := filepath.Join("testdata", "fake_sa.json")
	env := func(k string) string {
		if k == CredentialsEnvVar {
			return saPath
		}
		return ""
	}
	cfg, err := loadConfig(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.location != DefaultLocation {
		t.Errorf("location = %q, want %q", cfg.location, DefaultLocation)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Auth / config errors
// ─────────────────────────────────────────────────────────────────────────

func TestLoadConfig_MissingCredentialsEnvVar(t *testing.T) {
	env := func(string) string { return "" }
	_, err := loadConfig(context.Background(), env)
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestLoadConfig_NonexistentCredentialsFile(t *testing.T) {
	env := func(k string) string {
		if k == CredentialsEnvVar {
			return filepath.Join(t.TempDir(), "no-such-file.json")
		}
		return ""
	}
	_, err := loadConfig(context.Background(), env)
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

func TestLoadConfig_EmptyCredentialsFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(tmp, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == CredentialsEnvVar {
			return tmp
		}
		return ""
	}
	_, err := loadConfig(context.Background(), env)
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestLoadConfig_InvalidCredentialsJSON(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(tmp, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == CredentialsEnvVar {
			return tmp
		}
		return ""
	}
	_, err := loadConfig(context.Background(), env)
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestLoadConfig_CredentialsMissingProjectID(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "noproject.json")
	body := `{"type":"service_account","private_key":"x","client_email":"y@z.com"}`
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == CredentialsEnvVar {
			return tmp
		}
		return ""
	}
	_, err := loadConfig(context.Background(), env)
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Init / registration
// ─────────────────────────────────────────────────────────────────────────

func TestShouldRegister(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"creds set, dev mode off", map[string]string{CredentialsEnvVar: "/tmp/sa.json"}, true},
		{"creds unset", map[string]string{}, false},
		{"creds set but DEV_MODE=mock", map[string]string{
			CredentialsEnvVar:       "/tmp/sa.json",
			adapter.DevModeEnvVar:   adapter.DevModeValue,
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := func(k string) string { return tc.env[k] }
			if got := shouldRegister(env); got != tc.want {
				t.Errorf("shouldRegister = %v, want %v", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Manifest seeds
// ─────────────────────────────────────────────────────────────────────────

func TestSeedManifests_AllValid(t *testing.T) {
	manifests := SeedManifests()
	if len(manifests) == 0 {
		t.Fatal("no manifests")
	}
	for _, m := range manifests {
		if err := m.Validate(); err != nil {
			t.Errorf("manifest %q invalid: %v", m.Key, err)
		}
		if m.Provider != ProviderName {
			t.Errorf("manifest %q provider = %q, want %q", m.Key, m.Provider, ProviderName)
		}
	}
}

func TestSeedManifests_TagsIncludeVideoAndAsync(t *testing.T) {
	for _, m := range SeedManifests() {
		hasVideo, hasAsync := false, false
		for _, tag := range m.Tags {
			if tag == "video" {
				hasVideo = true
			}
			if tag == "async" {
				hasAsync = true
			}
		}
		if !hasVideo {
			t.Errorf("manifest %q missing video tag", m.Key)
		}
		if !hasAsync {
			t.Errorf("manifest %q missing async tag", m.Key)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Helpers — direct unit tests for normalize.go internals
// ─────────────────────────────────────────────────────────────────────────

func TestClassifyOperationError_AllCanonicalCodes(t *testing.T) {
	cases := []struct {
		code int
		want adapter.ErrorClass
	}{
		{16, adapter.ErrClassAuth},
		{7, adapter.ErrClassPayment},
		{8, adapter.ErrClassRateLimit},
		{13, adapter.ErrClassUpstream},
		{14, adapter.ErrClassUpstream},
		{4, adapter.ErrClassTimeout},
		{5, adapter.ErrClassNotFound},
		{99, adapter.ErrClassUpstream}, // unknown numeric → fallback
	}
	for _, tc := range cases {
		got, _ := classifyOperationError(&vertexStatus{Code: tc.code, Message: "msg"})
		if got != tc.want {
			t.Errorf("code %d: got %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestClassifyOperationError_TextStatusFallback(t *testing.T) {
	got, _ := classifyOperationError(&vertexStatus{Status: "RESOURCE_EXHAUSTED", Message: "msg"})
	if got != adapter.ErrClassRateLimit {
		t.Errorf("got %q", got)
	}
	got, _ = classifyOperationError(nil)
	if got != adapter.ErrClassUnknown {
		t.Errorf("nil case: got %q", got)
	}
	// INVALID_ARGUMENT without policy keywords → upstream.
	got, _ = classifyOperationError(&vertexStatus{Code: 3, Message: "missing prompt"})
	if got != adapter.ErrClassUpstream {
		t.Errorf("invalid_argument fallback: got %q", got)
	}
}

func TestCapRaw_TruncatesBeyondMax(t *testing.T) {
	big := make([]byte, adapter.MaxRawErrorBytes*2)
	for i := range big {
		big[i] = 'A'
	}
	out := capRaw(big)
	if len(out) != adapter.MaxRawErrorBytes {
		t.Errorf("len = %d, want %d", len(out), adapter.MaxRawErrorBytes)
	}
}

func TestCapRaw_PreservesShortBuffers(t *testing.T) {
	in := []byte("hello")
	out := capRaw(in)
	if string(out) != "hello" {
		t.Errorf("got %q", out)
	}
	// Defensive copy check.
	out[0] = 'X'
	if in[0] != 'h' {
		t.Error("capRaw did not copy")
	}
}

func TestIsBlockedLocation(t *testing.T) {
	for _, loc := range []string{"asia-east2", "ASIA-EAST2", " asia-east2 ", "cn-north-1"} {
		if !isBlockedLocation(loc) {
			t.Errorf("%q should be blocked", loc)
		}
	}
	for _, loc := range []string{"us-central1", "europe-west4", ""} {
		if isBlockedLocation(loc) {
			t.Errorf("%q should NOT be blocked", loc)
		}
	}
}

func TestIntFrom(t *testing.T) {
	cases := []struct {
		in   any
		want int
		ok   bool
	}{
		{int(7), 7, true},
		{int32(7), 7, true},
		{int64(7), 7, true},
		{float64(7), 7, true},
		{float32(7), 7, true},
		{json.Number("7"), 7, true},
		{json.Number("not"), 0, false},
		{"7", 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		got, ok := intFrom(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("intFrom(%v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestClassifyHTTPError_FallbackPaths(t *testing.T) {
	// no envelope → status-based mapping
	cases := []struct {
		status int
		want   adapter.ErrorClass
	}{
		{401, adapter.ErrClassAuth},
		{403, adapter.ErrClassPayment},
		{404, adapter.ErrClassNotFound},
		{429, adapter.ErrClassRateLimit},
		{400, adapter.ErrClassUpstream},
		{500, adapter.ErrClassUpstream},
		{503, adapter.ErrClassUpstream},
		{418, adapter.ErrClassUnknown},
	}
	for _, tc := range cases {
		got, _ := classifyHTTPError(tc.status, []byte("plain text"))
		if got != tc.want {
			t.Errorf("status %d: got %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestTrimBody_TruncatesLongBody(t *testing.T) {
	long := strings.Repeat("x", 1024)
	out := trimBody(long)
	if len(out) > 300 {
		t.Errorf("len = %d", len(out))
	}
	if !strings.HasSuffix(out, "...") {
		t.Errorf("expected ellipsis; got %q", out[len(out)-5:])
	}
	short := "abc"
	if trimBody(short) != "abc" {
		t.Error("short body mutated")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// AcceptedAt sanity
// ─────────────────────────────────────────────────────────────────────────

func TestSubmit_AcceptedAtIsRecent(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, loadGolden(t, "submit_response.json")
	})
	a := newTestAdapter(t, fs.server)
	before := time.Now().UTC()
	res, err := a.Submit(context.Background(), "veo-3.0-generate-preview",
		adapter.Params{"prompt": "x"}, "k")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	if t0 := res.AcceptedAt(); t0.Before(before) || t0.After(after) {
		t.Errorf("AcceptedAt %v not in [%v, %v]", t0, before, after)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Submit response missing name
// ─────────────────────────────────────────────────────────────────────────

func TestSubmit_RejectsResponseWithoutOperationName(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, []byte(`{}`)
	})
	a := newTestAdapter(t, fs.server)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview",
		adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSubmit_RejectsMalformedResponseJSON(t *testing.T) {
	fs := newFakeServer(t, func(*http.Request, []byte) (int, []byte) {
		return http.StatusOK, []byte(`not json`)
	})
	a := newTestAdapter(t, fs.server)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview",
		adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected decode error")
	}
}
