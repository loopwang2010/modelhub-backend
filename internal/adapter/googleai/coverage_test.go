// Coverage-targeted tests covering error branches that the main adapter
// tests don't naturally exercise. Kept in a separate file so the main test
// flow stays readable.

package googleai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// ─────────────────────────────────────────────────────────────────────────
// URL builders — empty-input branches
// ─────────────────────────────────────────────────────────────────────────

func TestSubmitURL_RejectsEmptyModel(t *testing.T) {
	cfg := &config{
		location:   "us-central1",
		credSource: &staticTokenSource{token: "t", project: "proj"},
	}
	if _, err := cfg.submitURL(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestSubmitURL_RejectsEmptyProjectID(t *testing.T) {
	cfg := &config{
		location:   "us-central1",
		credSource: &staticTokenSource{token: "t"}, // empty project
	}
	if _, err := cfg.submitURL("model"); !errors.Is(err, adapter.ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestPollURL_RejectsEmptyOpName(t *testing.T) {
	cfg := &config{location: "us-central1"}
	if _, err := cfg.pollURL(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestCancelURL_RejectsEmptyOpName(t *testing.T) {
	cfg := &config{location: "us-central1"}
	if _, err := cfg.cancelURL(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestBaseURL_HonoursOverride(t *testing.T) {
	cfg := &config{location: "us-central1", baseURLOverride: "https://example.test/"}
	got := cfg.baseURL()
	if !strings.HasPrefix(got, "https://example.test") {
		t.Errorf("got %q", got)
	}
	// Trailing slash should be trimmed.
	if strings.HasSuffix(got, "/") {
		t.Errorf("trailing slash not trimmed: %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// auth.go branches
// ─────────────────────────────────────────────────────────────────────────

func TestStaticTokenSource_RejectsEmptyToken(t *testing.T) {
	src := &staticTokenSource{token: ""}
	_, err := src.tokenSource(context.Background())
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestStaticTokenSource_ProjectID(t *testing.T) {
	src := &staticTokenSource{project: "p"}
	if src.projectID() != "p" {
		t.Error("projectID lost")
	}
}

func TestAuthedClient_RejectsNilSource(t *testing.T) {
	_, err := authedClient(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAuthedClient_PropagatesTokenError(t *testing.T) {
	src := &staticTokenSource{token: ""} // empty token → err
	_, err := authedClient(context.Background(), src, nil)
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestFileCredentialSource_EmptyPath(t *testing.T) {
	src := newFileCredentialSource("")
	_, err := src.tokenSource(context.Background())
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
	if src.projectID() != "" {
		t.Errorf("projectID = %q", src.projectID())
	}
}

// ─────────────────────────────────────────────────────────────────────────
// httpError.Error()
// ─────────────────────────────────────────────────────────────────────────

func TestHTTPError_ErrorMethod(t *testing.T) {
	e := &httpError{Class: adapter.ErrClassAuth, Status: 401, Msg: "boom"}
	if e.Error() != "boom" {
		t.Errorf("Error() = %q", e.Error())
	}
}

// ─────────────────────────────────────────────────────────────────────────
// classifyOperationError — additional fallback branches
// ─────────────────────────────────────────────────────────────────────────

func TestClassifyOperationError_StatusFallbacks(t *testing.T) {
	cases := []struct {
		status string
		want   adapter.ErrorClass
	}{
		{"UNAUTHENTICATED", adapter.ErrClassAuth},
		{"PERMISSION_DENIED", adapter.ErrClassPayment},
		{"INTERNAL", adapter.ErrClassUpstream},
		{"UNAVAILABLE", adapter.ErrClassUpstream},
		{"DEADLINE_EXCEEDED", adapter.ErrClassTimeout},
		{"NOT_FOUND", adapter.ErrClassNotFound},
	}
	for _, tc := range cases {
		got, _ := classifyOperationError(&vertexStatus{Status: tc.status, Message: "m"})
		if got != tc.want {
			t.Errorf("status %q: got %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestClassifyOperationError_EmptyMessageFallback(t *testing.T) {
	_, msg := classifyOperationError(&vertexStatus{Code: 13})
	if msg == "" {
		t.Error("message should default to non-empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Cancel branches
// ─────────────────────────────────────────────────────────────────────────

func TestCancel_TransportError(t *testing.T) {
	// Return immediately by closing the server before the test runs the
	// request. We achieve this by pointing baseURLOverride at a closed
	// server.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	src := &staticTokenSource{token: "t", project: "p"}
	cfg := &config{
		location:        "us-central1",
		baseURLOverride: closed.URL,
		credSource:      src,
	}
	httpClient, err := authedClient(context.Background(), src, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	cfg.httpClient = httpClient
	a := newAdapterFromConfig(cfg)
	err = a.Cancel(context.Background(), "veo-3.0-generate-preview",
		"projects/p/locations/us-central1/operations/abc")
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestPoll_TransportError(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	src := &staticTokenSource{token: "t", project: "p"}
	cfg := &config{
		location:        "us-central1",
		baseURLOverride: closed.URL,
		credSource:      src,
	}
	httpClient, _ := authedClient(context.Background(), src, http.DefaultTransport)
	cfg.httpClient = httpClient
	a := newAdapterFromConfig(cfg)
	_, err := a.Poll(context.Background(), "veo-3.0-generate-preview",
		"projects/p/locations/us-central1/operations/abc")
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestSubmit_TransportError(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	src := &staticTokenSource{token: "t", project: "p"}
	cfg := &config{
		location:        "us-central1",
		baseURLOverride: closed.URL,
		credSource:      src,
	}
	httpClient, _ := authedClient(context.Background(), src, http.DefaultTransport)
	cfg.httpClient = httpClient
	a := newAdapterFromConfig(cfg)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview",
		adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected transport error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// pollFromOperation invalid response paths (raw error bytes from upstream
// that have done=true but response is malformed)
// ─────────────────────────────────────────────────────────────────────────

func TestPollFromOperation_DoneButMalformedResponseBubblesError(t *testing.T) {
	op := &vertexOperation{
		Name: "x",
		Done: true,
		Response: &vertexResponse{
			Predictions: []vertexPrediction{},
		},
	}
	_, err := pollFromOperation("m", op, []byte("{}"))
	if err == nil {
		t.Fatal("expected error from empty predictions")
	}
}

func TestPollFromOperation_ErrorWithCodeZeroFallsThrough(t *testing.T) {
	// done=true with error.code=0 should be ignored and fall to the
	// success branch. Predictions still required.
	op := &vertexOperation{
		Name:  "x",
		Done:  true,
		Error: &vertexStatus{Code: 0},
		Response: &vertexResponse{
			Predictions: []vertexPrediction{{VideoURI: "gs://b/o.mp4"}},
		},
	}
	pr, err := pollFromOperation("m", op, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if pr.Status != adapter.PollSucceeded {
		t.Errorf("status = %q", pr.Status)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Submit guard for project-id at runtime
// ─────────────────────────────────────────────────────────────────────────

func TestSubmit_BubblesURLBuildError(t *testing.T) {
	// staticTokenSource with empty project triggers submitURL's
	// "project id unavailable" branch.
	src := &staticTokenSource{token: "t"}
	cfg := &config{
		location:   "us-central1",
		credSource: src,
	}
	httpClient, _ := authedClient(context.Background(), src, http.DefaultTransport)
	cfg.httpClient = httpClient
	a := newAdapterFromConfig(cfg)
	_, err := a.Submit(context.Background(), "veo-3.0-generate-preview",
		adapter.Params{"prompt": "x"}, "k")
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Manifest seed sanity — every model must appear in the cost map
// ─────────────────────────────────────────────────────────────────────────

func TestSeedManifests_PricingTableCoverage(t *testing.T) {
	for _, m := range SeedManifests() {
		if _, ok := veo3PerSecondCostMicroUSD[m.UpstreamModel]; !ok {
			t.Errorf("manifest %q (upstream %q) missing pricing entry", m.Key, m.UpstreamModel)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Public constructor — exercises NewGoogleVertexAIAdapter via an env shim
// ─────────────────────────────────────────────────────────────────────────

func TestNewGoogleVertexAIAdapter_HappyAndUnconfigured(t *testing.T) {
	// Save and restore the env-lookup function the package consults.
	originalLookup := getenvLookup
	t.Cleanup(func() { getenvLookup = originalLookup })

	// Unconfigured branch: no GOOGLE_APPLICATION_CREDENTIALS.
	getenvLookup = func(string) string { return "" }
	if _, err := NewGoogleVertexAIAdapter(context.Background()); !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("unconfigured: err = %v", err)
	}

	// Configured branch: point at fake_sa.json. No HTTP traffic occurs in
	// the constructor, so this is safe even without a real Google project.
	getenvLookup = func(k string) string {
		if k == CredentialsEnvVar {
			return "testdata/fake_sa.json"
		}
		return ""
	}
	a, err := NewGoogleVertexAIAdapter(context.Background())
	if err != nil {
		t.Fatalf("happy: err = %v", err)
	}
	if a.Key() != ProviderName {
		t.Errorf("Key() = %q", a.Key())
	}
}

func TestEstimateCost_UnknownUpstreamPricing(t *testing.T) {
	// Construct an adapter whose manifest references an upstream id that
	// the pricing table doesn't know about. This exercises the missing-
	// pricing-entry branch in EstimateCost.
	src := &staticTokenSource{token: "t", project: "p"}
	cfg := &config{
		location:   "us-central1",
		credSource: src,
	}
	httpClient, _ := authedClient(context.Background(), src, http.DefaultTransport)
	cfg.httpClient = httpClient
	a := &GoogleVertexAIAdapter{
		cfg: cfg,
		manifest: map[adapter.ModelKey]string{
			"phantom-model": "phantom-upstream-not-priced",
		},
	}
	if _, err := a.EstimateCost("phantom-model", adapter.Params{"duration_seconds": 5}); err == nil {
		t.Fatal("expected error for missing pricing entry")
	}
}

func TestPollFromOperation_DoneFalseEmitsRunning(t *testing.T) {
	pr, err := pollFromOperation("m", &vertexOperation{Name: "x", Done: false}, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if pr.Status != adapter.PollRunning {
		t.Errorf("status = %q", pr.Status)
	}
}
