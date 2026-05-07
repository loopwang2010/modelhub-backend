package bfl

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
// Helpers
// ─────────────────────────────────────────────────────────────────────────

// mustReadGolden reads a file from testdata/ and returns its bytes.
func mustReadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// newTestAdapter spins up an httptest.Server, returns a configured BFLAdapter
// pointing at it, and a slice of captured requests for inspection. Caller
// must call Close on the returned cleanup function.
type capturedRequest struct {
	method     string
	path       string
	authHeader string
	idemHeader string
	body       []byte
}

type fakeServer struct {
	srv      *httptest.Server
	requests []capturedRequest
	// pollResponses is a queue of responses to return for sequential GET polls.
	// status code paired with body.
	pollResponses []serverResponse
	pollIdx       atomic.Int64
	submitResp    serverResponse
}

type serverResponse struct {
	status int
	body   []byte
}

func newFakeServer() *fakeServer {
	fs := &fakeServer{}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.handle))
	return fs
}

func (fs *fakeServer) Close() { fs.srv.Close() }

func (fs *fakeServer) URL() string { return fs.srv.URL }

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	fs.requests = append(fs.requests, capturedRequest{
		method:     r.Method,
		path:       r.URL.Path,
		authHeader: r.Header.Get(authHeader),
		idemHeader: r.Header.Get(idempotencyHeader),
		body:       body,
	})
	switch r.Method {
	case http.MethodPost:
		// Submit
		resp := fs.submitResp
		if resp.status == 0 {
			resp.status = http.StatusOK
		}
		if len(resp.body) == 0 {
			// Default success: rewrite the polling URL to point back at this server
			resp.body = []byte(`{"id":"abc-123","polling_url":"` + fs.URL() + `/v1/get_result?id=abc-123"}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write(resp.body)
	case http.MethodGet:
		// Poll
		idx := fs.pollIdx.Add(1) - 1
		var resp serverResponse
		if int(idx) < len(fs.pollResponses) {
			resp = fs.pollResponses[idx]
		} else if len(fs.pollResponses) > 0 {
			// Return the last response repeatedly once we run out.
			resp = fs.pollResponses[len(fs.pollResponses)-1]
		} else {
			resp = serverResponse{status: 200, body: []byte(`{"status":"Pending"}`)}
		}
		if resp.status == 0 {
			resp.status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write(resp.body)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Key
// ─────────────────────────────────────────────────────────────────────────

func TestKey(t *testing.T) {
	a := New("http://x", "k")
	if a.Key() != "bfl" {
		t.Errorf("Key() = %q, want bfl", a.Key())
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Submit
// ─────────────────────────────────────────────────────────────────────────

func TestSubmit_HappyPath(t *testing.T) {
	fs := newFakeServer()
	defer fs.Close()

	a := New(fs.URL(), "test-key")
	res, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "a cat"}, "idem-1")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	async, ok := res.(adapter.AsyncSubmit)
	if !ok {
		t.Fatalf("expected AsyncSubmit, got %T", res)
	}
	id, pollURL, err := decodeUpstreamRef(async.UpstreamRef)
	if err != nil {
		t.Fatalf("decode ref: %v", err)
	}
	if id != "abc-123" {
		t.Errorf("id = %q, want abc-123", id)
	}
	if !strings.Contains(pollURL, "/v1/get_result?id=abc-123") {
		t.Errorf("polling url = %q does not contain expected path", pollURL)
	}
	if async.At.IsZero() {
		t.Error("AcceptedAt is zero")
	}
}

func TestSubmit_AttachesAuthAndIdempotencyHeaders(t *testing.T) {
	fs := newFakeServer()
	defer fs.Close()

	a := New(fs.URL(), "secret-key")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "x"}, "idem-42")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(fs.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(fs.requests))
	}
	got := fs.requests[0]
	if got.authHeader != "secret-key" {
		t.Errorf("x-key header = %q, want secret-key", got.authHeader)
	}
	if got.idemHeader != "idem-42" {
		t.Errorf("idempotency header = %q, want idem-42", got.idemHeader)
	}
	if got.path != "/v1/flux-pro-1.1" {
		t.Errorf("path = %q, want /v1/flux-pro-1.1", got.path)
	}
	// Body should be JSON containing prompt
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if body["prompt"] != "x" {
		t.Errorf("body.prompt = %v, want x", body["prompt"])
	}
}

func TestSubmit_StripsInternalKeys(t *testing.T) {
	fs := newFakeServer()
	defer fs.Close()

	a := New(fs.URL(), "k")
	params := adapter.Params{
		"prompt":              "y",
		"_internal_trace":     "should-not-leak",
		"x-modelhub-trace-id": "should-not-leak",
	}
	_, err := a.Submit(context.Background(), "flux-pro-1.1", params, "idem")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	got := fs.requests[0].body
	if strings.Contains(string(got), "should-not-leak") {
		t.Errorf("internal key leaked into upstream body: %s", got)
	}
}

func TestSubmit_RejectsEmptyIdempotencyKey(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "p"}, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestSubmit_RejectsNilParams(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", nil, "k")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestSubmit_UnknownModel(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.Submit(context.Background(), "not-a-flux-model", adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Errorf("err = %v, want unknown model", err)
	}
}

func TestSubmit_AuthErrorFromUpstream(t *testing.T) {
	fs := newFakeServer()
	fs.submitResp = serverResponse{status: 401, body: []byte(`{"error":"unauthorized"}`)}
	defer fs.Close()

	a := New(fs.URL(), "bad-key")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "x"}, "idem")
	if err == nil {
		t.Fatal("expected error")
	}
	cls, ok := ErrorClass(err)
	if !ok {
		t.Fatalf("ErrorClass returned false on %v", err)
	}
	if cls != adapter.ErrClassAuth {
		t.Errorf("class = %q, want auth", cls)
	}
}

func TestSubmit_RateLimitErrorFromUpstream(t *testing.T) {
	fs := newFakeServer()
	fs.submitResp = serverResponse{status: 429, body: []byte(`{"error":"rate-limited"}`)}
	defer fs.Close()

	a := New(fs.URL(), "k")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "x"}, "idem")
	if err == nil {
		t.Fatal("expected error")
	}
	cls, _ := ErrorClass(err)
	if cls != adapter.ErrClassRateLimit {
		t.Errorf("class = %q, want rate_limit", cls)
	}
}

func TestSubmit_PaymentErrorFromUpstream(t *testing.T) {
	fs := newFakeServer()
	fs.submitResp = serverResponse{status: 402, body: []byte(`{"error":"out of credit"}`)}
	defer fs.Close()

	a := New(fs.URL(), "k")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "x"}, "idem")
	cls, _ := ErrorClass(err)
	if cls != adapter.ErrClassPayment {
		t.Errorf("class = %q, want payment", cls)
	}
}

func TestSubmit_5xxErrorFromUpstream(t *testing.T) {
	fs := newFakeServer()
	fs.submitResp = serverResponse{status: 503, body: []byte(`gateway`)}
	defer fs.Close()

	a := New(fs.URL(), "k")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "x"}, "idem")
	cls, _ := ErrorClass(err)
	if cls != adapter.ErrClassUpstream {
		t.Errorf("class = %q, want upstream", cls)
	}
}

func TestSubmit_MissingIDOrPollingURL(t *testing.T) {
	fs := newFakeServer()
	fs.submitResp = serverResponse{status: 200, body: []byte(`{"id":""}`)}
	defer fs.Close()

	a := New(fs.URL(), "k")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "x"}, "idem")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "missing id") {
		t.Errorf("err = %v, want missing id", err)
	}
}

func TestSubmit_RespectsContextCancellation(t *testing.T) {
	fs := newFakeServer()
	defer fs.Close()
	a := New(fs.URL(), "k")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Submit(ctx, "flux-pro-1.1", adapter.Params{"prompt": "x"}, "idem")
	if err == nil {
		t.Fatal("expected ctx error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Poll
// ─────────────────────────────────────────────────────────────────────────

func TestPoll_PendingThenReady(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 200, body: mustReadGolden(t, "bfl_pending.json")},
		{status: 200, body: mustReadGolden(t, "bfl_pending.json")},
		{status: 200, body: mustReadGolden(t, "bfl_ready.json")},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/v1/get_result?id=abc-123")

	// First poll: pending
	pr, err := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll1: %v", err)
	}
	if pr.Status != adapter.PollPending {
		t.Errorf("status = %q, want pending", pr.Status)
	}

	// Second poll: still pending
	pr, err = a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll2: %v", err)
	}
	if pr.Status != adapter.PollPending {
		t.Errorf("status = %q, want pending", pr.Status)
	}

	// Third poll: ready
	pr, err = a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll3: %v", err)
	}
	if pr.Status != adapter.PollSucceeded {
		t.Errorf("status = %q, want succeeded", pr.Status)
	}
	if pr.Result == nil {
		t.Fatal("Result is nil")
	}
	if len(pr.Result.Outputs) != 1 {
		t.Fatalf("outputs len = %d, want 1", len(pr.Result.Outputs))
	}
	out := pr.Result.Outputs[0]
	if out.Kind != adapter.OutputKindImageURL {
		t.Errorf("kind = %q, want image_url", out.Kind)
	}
	if out.MimeType != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", out.MimeType)
	}
	// AP-19: the URL is upstream-shaped here. S9.5 will rewrite it.
	if !strings.HasPrefix(out.URL, "https://delivery-eu1.bfl.ai/") {
		t.Errorf("URL = %q, want delivery URL", out.URL)
	}
}

func TestPoll_ContentModerated(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 200, body: mustReadGolden(t, "bfl_content_moderated.json")},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	pr, err := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollFailed {
		t.Errorf("status = %q, want failed", pr.Status)
	}
	if pr.Error == nil {
		t.Fatal("Error is nil")
	}
	if pr.Error.Class != adapter.ErrClassContentPolicy {
		t.Errorf("class = %q, want content_policy", pr.Error.Class)
	}
}

func TestPoll_TaskNotFound(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 200, body: mustReadGolden(t, "bfl_task_not_found.json")},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	pr, err := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollFailed {
		t.Errorf("status = %q, want failed", pr.Status)
	}
	if pr.Error.Class != adapter.ErrClassNotFound {
		t.Errorf("class = %q, want not_found", pr.Error.Class)
	}
}

func TestPoll_ErrorWithDetails(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 200, body: mustReadGolden(t, "bfl_error.json")},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	pr, err := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollFailed {
		t.Errorf("status = %q, want failed", pr.Status)
	}
	if pr.Error.Class != adapter.ErrClassUpstream {
		t.Errorf("class = %q, want upstream", pr.Error.Class)
	}
	if pr.Error.Message != "model overloaded" {
		t.Errorf("message = %q, want 'model overloaded'", pr.Error.Message)
	}
}

func TestPoll_HTTP500(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 503, body: []byte(`gateway`)},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	pr, err := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pr.Status != adapter.PollFailed {
		t.Errorf("status = %q, want failed", pr.Status)
	}
	if pr.Error.Class != adapter.ErrClassUpstream {
		t.Errorf("class = %q, want upstream", pr.Error.Class)
	}
}

func TestPoll_HTTP404MapsToNotFound(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 404, body: []byte(`gone`)},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	pr, _ := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if pr.Error.Class != adapter.ErrClassNotFound {
		t.Errorf("class = %q, want not_found", pr.Error.Class)
	}
}

func TestPoll_MalformedRef(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.Poll(context.Background(), "flux-pro-1.1", "no-pipe-here")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestPoll_UnknownStatusTreatedAsRunning(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 200, body: []byte(`{"status":"NewBFLState"}`)},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	pr, err := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// Defensive: never fail-out on an unknown status — keep polling.
	if pr.Status != adapter.PollRunning {
		t.Errorf("status = %q, want running", pr.Status)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Cancel
// ─────────────────────────────────────────────────────────────────────────

func TestCancel_AlwaysReturnsUnsupported(t *testing.T) {
	a := New("http://x", "k")
	if err := a.Cancel(context.Background(), "flux-pro-1.1", "ref"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("Cancel err = %v, want ErrUnsupported", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// EstimateCost
// ─────────────────────────────────────────────────────────────────────────

func TestEstimateCost_AllKnownModels_AreNonZero(t *testing.T) {
	a := New("http://x", "k")
	for _, model := range []adapter.ModelKey{"flux-pro-1.1", "flux-dev", "flux-schnell-v1.5", "flux-2-pro"} {
		cost, err := a.EstimateCost(model, adapter.Params{"prompt": "x"})
		if err != nil {
			t.Errorf("model %q: err = %v", model, err)
		}
		if cost <= 0 {
			t.Errorf("model %q: cost = %d, want > 0", model, cost)
		}
		if cost > adapter.MaxCostUSD {
			t.Errorf("model %q: cost = %d, exceeds MaxCostUSD", model, cost)
		}
	}
}

func TestEstimateCost_ScalesWithNumImages(t *testing.T) {
	a := New("http://x", "k")
	c1, _ := a.EstimateCost("flux-pro-1.1", adapter.Params{"num_images": 1})
	c4, _ := a.EstimateCost("flux-pro-1.1", adapter.Params{"num_images": 4})
	if c4 != 4*c1 {
		t.Errorf("cost@4 = %d, cost@1 = %d, ratio not 4x", c4, c1)
	}
}

func TestEstimateCost_FloatNumImages(t *testing.T) {
	// JSON-decoded params have num_images as float64
	a := New("http://x", "k")
	c, err := a.EstimateCost("flux-pro-1.1", adapter.Params{"num_images": float64(2)})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if c != 2*40_000 {
		t.Errorf("cost = %d, want 80000", c)
	}
}

func TestEstimateCost_JSONNumberNumImages(t *testing.T) {
	a := New("http://x", "k")
	c, _ := a.EstimateCost("flux-pro-1.1", adapter.Params{"num_images": json.Number("3")})
	if c != 3*40_000 {
		t.Errorf("cost = %d, want 120000", c)
	}
}

func TestEstimateCost_ClampsNumImages(t *testing.T) {
	a := New("http://x", "k")
	// 100 should clamp to 8
	c100, _ := a.EstimateCost("flux-pro-1.1", adapter.Params{"num_images": 100})
	c8, _ := a.EstimateCost("flux-pro-1.1", adapter.Params{"num_images": 8})
	if c100 != c8 {
		t.Errorf("cost@100 = %d != cost@8 = %d (should clamp)", c100, c8)
	}
}

func TestEstimateCost_UnknownModel(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.EstimateCost("not-a-flux", adapter.Params{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Capabilities
// ─────────────────────────────────────────────────────────────────────────

func TestCapabilities_DefaultIsConservative(t *testing.T) {
	a := New("http://x", "k")
	for _, model := range []adapter.ModelKey{"flux-pro-1.1", "flux-dev", "flux-schnell-v1.5", "flux-2-pro"} {
		caps := a.Capabilities(model)
		if caps.SupportsCancel {
			t.Errorf("%s: SupportsCancel = true, want false (BFL has no cancel)", model)
		}
		// SupportsWebhook is currently false pending T-004 verification; flag if it ever flips on
		// without our test list being updated.
		if caps.SupportsWebhook {
			t.Errorf("%s: SupportsWebhook = true unexpectedly — verify HMAC scheme then update this test", model)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// NormalizeResult
// ─────────────────────────────────────────────────────────────────────────

func TestNormalizeResult_ExtractsSampleURL(t *testing.T) {
	a := New("http://x", "k")
	body := mustReadGolden(t, "bfl_ready.json")
	nr, err := a.NormalizeResult("flux-pro-1.1", body)
	if err != nil {
		t.Fatalf("NormalizeResult: %v", err)
	}
	if nr.Modality != adapter.ModalityImage {
		t.Errorf("modality = %q, want image", nr.Modality)
	}
	if len(nr.Outputs) != 1 {
		t.Fatalf("outputs len = %d, want 1", len(nr.Outputs))
	}
	out := nr.Outputs[0]
	if out.URL != "https://delivery-eu1.bfl.ai/results/abc-123-deadbeef.jpg" {
		t.Errorf("URL = %q", out.URL)
	}
	if out.MimeType != "image/jpeg" {
		t.Errorf("mime = %q", out.MimeType)
	}
	if nr.Metadata["seed"] != int64(42) {
		t.Errorf("metadata.seed = %v, want 42", nr.Metadata["seed"])
	}
}

func TestNormalizeResult_NotReady(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.NormalizeResult("flux-pro-1.1", mustReadGolden(t, "bfl_pending.json"))
	if err == nil {
		t.Fatal("expected error for non-Ready response")
	}
}

func TestNormalizeResult_MissingSample(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.NormalizeResult("flux-pro-1.1", []byte(`{"status":"Ready","result":{}}`))
	if err == nil {
		t.Fatal("expected error for missing sample")
	}
}

func TestNormalizeResult_EmptyBody(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.NormalizeResult("flux-pro-1.1", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeResult_InvalidJSON(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.NormalizeResult("flux-pro-1.1", []byte(`not json`))
	if err == nil {
		t.Fatal("expected error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// VerifyWebhook
// ─────────────────────────────────────────────────────────────────────────

func TestVerifyWebhook_ReturnsUnsupported(t *testing.T) {
	a := New("http://x", "k")
	_, err := a.VerifyWebhook(http.Header{}, []byte(`{}`))
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// classifyPollStatus direct tests
// ─────────────────────────────────────────────────────────────────────────

func TestClassifyPollStatus_AllStatusesGoldenFiles(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		want    adapter.PollStatus
		errClas adapter.ErrorClass
	}{
		{"pending", "bfl_pending.json", adapter.PollPending, ""},
		{"ready", "bfl_ready.json", adapter.PollSucceeded, ""},
		{"content_moderated", "bfl_content_moderated.json", adapter.PollFailed, adapter.ErrClassContentPolicy},
		{"task_not_found", "bfl_task_not_found.json", adapter.PollFailed, adapter.ErrClassNotFound},
		{"error", "bfl_error.json", adapter.PollFailed, adapter.ErrClassUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := mustReadGolden(t, tt.file)
			pr, err := classifyPollStatus("flux-pro-1.1", body)
			if err != nil {
				t.Fatalf("classifyPollStatus: %v", err)
			}
			if pr.Status != tt.want {
				t.Errorf("status = %q, want %q", pr.Status, tt.want)
			}
			if tt.errClas != "" {
				if pr.Error == nil {
					t.Fatal("Error is nil")
				}
				if pr.Error.Class != tt.errClas {
					t.Errorf("class = %q, want %q", pr.Error.Class, tt.errClas)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// UpstreamRef encoding
// ─────────────────────────────────────────────────────────────────────────

func TestUpstreamRef_Roundtrip(t *testing.T) {
	id := "abc-123"
	url := "https://api.bfl.ai/v1/get_result?id=abc-123"
	ref := encodeUpstreamRef(id, url)
	gotID, gotURL, err := decodeUpstreamRef(ref)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotID != id || gotURL != url {
		t.Errorf("roundtrip mismatch: id=%q url=%q", gotID, gotURL)
	}
}

func TestUpstreamRef_DecodeMissingSeparator(t *testing.T) {
	_, _, err := decodeUpstreamRef("just-an-id")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Manifest seeds
// ─────────────────────────────────────────────────────────────────────────

func TestSeedManifests_AllValidate(t *testing.T) {
	for _, m := range SeedManifests() {
		if err := m.Validate(); err != nil {
			t.Errorf("manifest %s: validate: %v", m.Key, err)
		}
		if m.Provider != providerKey {
			t.Errorf("manifest %s: Provider = %q, want bfl", m.Key, m.Provider)
		}
		if m.UpstreamModel == "" {
			t.Errorf("manifest %s: UpstreamModel empty", m.Key)
		}
	}
}

func TestSeedManifests_FluxDevHasApiOnlyTag(t *testing.T) {
	for _, m := range SeedManifests() {
		if m.Key != "flux-dev" {
			continue
		}
		found := false
		for _, tag := range m.Tags {
			if tag == "api-only" {
				found = true
			}
		}
		if !found {
			t.Errorf("flux-dev manifest missing 'api-only' tag (TOS §5: NCL forbids self-hosting commercially)")
		}
		return
	}
	t.Fatal("flux-dev manifest not found in seed list")
}

func TestSeedManifests_NoUpstreamModelInPublicJSON(t *testing.T) {
	// ADR-018: the `upstream_model` JSON field must NEVER appear in the JSON
	// the API ships — its tag is `json:"-"`. This test asserts the field-name
	// boundary, not the value (the value can legitimately appear inside e.g.
	// a tag like "flux" because public Keys carry similar branding).
	for _, m := range SeedManifests() {
		bs, err := m.PublicJSON()
		if err != nil {
			t.Fatalf("PublicJSON: %v", err)
		}
		// Decode to a map and check the keyset directly — substring-on-bytes
		// would falsely flag tags or names that happen to share a brand prefix.
		var top map[string]json.RawMessage
		if err := json.Unmarshal(bs, &top); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		if _, leaked := top["upstream_model"]; leaked {
			t.Errorf("manifest %s: PublicJSON leaks 'upstream_model' field", m.Key)
		}
	}
}

func TestAssetCostFractions_AllSeedModels(t *testing.T) {
	fractions := AssetCostFractions()
	for _, m := range SeedManifests() {
		f, ok := fractions[m.Key]
		if !ok {
			t.Errorf("model %s missing AssetCostFraction entry", m.Key)
			continue
		}
		if f < 0 || f > 1 {
			t.Errorf("model %s: fraction = %f, must be in [0,1]", m.Key, f)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Construction & env
// ─────────────────────────────────────────────────────────────────────────

func TestNewFromEnv_MissingKey(t *testing.T) {
	t.Setenv(envAPIKey, "")
	_, err := NewFromEnv()
	if err == nil {
		t.Fatal("expected ErrNotConfigured")
	}
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

func TestNewFromEnv_WithKey(t *testing.T) {
	t.Setenv(envAPIKey, "fake-key")
	a, err := NewFromEnv()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if a.client.apiKey != "fake-key" {
		t.Errorf("apiKey = %q", a.client.apiKey)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// AP-3: Poll does NOT sleep
// ─────────────────────────────────────────────────────────────────────────

func TestPoll_DoesNotSleepInternally(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 200, body: mustReadGolden(t, "bfl_pending.json")},
	}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")

	start := time.Now()
	_, _ = a.Poll(context.Background(), "flux-pro-1.1", ref)
	elapsed := time.Since(start)
	// AP-3: the adapter must not sleep. Even with httptest server overhead,
	// 250ms is wildly generous.
	if elapsed > 250*time.Millisecond {
		t.Errorf("Poll took %v — adapter is sleeping (AP-3 violation)", elapsed)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// AP-19: NormalizeResult must report image_url, not image_data inline
// ─────────────────────────────────────────────────────────────────────────

func TestNormalizeResult_ProducesImageURLOutputKind(t *testing.T) {
	a := New("http://x", "k")
	nr, err := a.NormalizeResult("flux-pro-1.1", mustReadGolden(t, "bfl_ready.json"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if nr.Outputs[0].Kind != adapter.OutputKindImageURL {
		t.Errorf("kind = %q, want image_url (so envelope rewrites the URL pre-S9.5)", nr.Outputs[0].Kind)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PollResponse error field cap (L4 fix)
// ─────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────
// bflError + ErrorClass extraction
// ─────────────────────────────────────────────────────────────────────────

func TestBFLError_ErrorAndUnwrap(t *testing.T) {
	inner := errors.New("inner failure")
	be := &bflError{class: adapter.ErrClassAuth, msg: "outer", wrapped: inner}
	if be.Error() != "outer" {
		t.Errorf("Error() = %q, want outer", be.Error())
	}
	if !errors.Is(be, inner) {
		t.Error("errors.Is should match wrapped")
	}
}

func TestErrorClass_NonBFLError(t *testing.T) {
	if _, ok := ErrorClass(errors.New("plain")); ok {
		t.Error("ErrorClass on plain error returned ok=true")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// numImagesFromParams edge cases
// ─────────────────────────────────────────────────────────────────────────

func TestNumImagesFromParams_NilAndMissingAndZeroAndNegative(t *testing.T) {
	if got := numImagesFromParams(nil); got != 1 {
		t.Errorf("nil → %d, want 1", got)
	}
	if got := numImagesFromParams(adapter.Params{}); got != 1 {
		t.Errorf("missing → %d, want 1", got)
	}
	if got := numImagesFromParams(adapter.Params{"num_images": 0}); got != 1 {
		t.Errorf("0 → %d, want 1 (clamp up)", got)
	}
	if got := numImagesFromParams(adapter.Params{"num_images": -3}); got != 1 {
		t.Errorf("-3 → %d, want 1 (clamp up)", got)
	}
	if got := numImagesFromParams(adapter.Params{"num_images": "not-a-number"}); got != 1 {
		t.Errorf("non-numeric → %d, want 1 (default)", got)
	}
	if got := numImagesFromParams(adapter.Params{"num_images": int64(5)}); got != 5 {
		t.Errorf("int64(5) → %d, want 5", got)
	}
	if got := numImagesFromParams(adapter.Params{"num_images": json.Number("not-int")}); got != 1 {
		t.Errorf("invalid json.Number → %d, want 1", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// classifyHTTPStatus full table
// ─────────────────────────────────────────────────────────────────────────

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		status int
		want   adapter.ErrorClass
	}{
		{401, adapter.ErrClassAuth},
		{402, adapter.ErrClassPayment},
		{429, adapter.ErrClassRateLimit},
		{500, adapter.ErrClassUpstream},
		{502, adapter.ErrClassUpstream},
		{503, adapter.ErrClassUpstream},
		{404, adapter.ErrClassNotFound},
		{418, adapter.ErrClassUnknown}, // teapot — falls through to unknown
		{400, adapter.ErrClassUnknown},
	}
	for _, tt := range tests {
		got := classifyHTTPStatus(tt.status)
		if got != tt.want {
			t.Errorf("status %d: got %q, want %q", tt.status, got, tt.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// truncateForError covers the long-string path
// ─────────────────────────────────────────────────────────────────────────

func TestTruncateForError(t *testing.T) {
	short := []byte("short body")
	got := truncateForError(short)
	if got != "short body" {
		t.Errorf("short = %q", got)
	}
	long := make([]byte, 1024)
	for i := range long {
		long[i] = 'a'
	}
	got = truncateForError(long)
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("long: missing truncation suffix: %s", got[len(got)-30:])
	}
	if len(got) > 256+len("...(truncated)") {
		t.Errorf("long: result %d chars, expected <= ~270", len(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Submit network error (transport failure, not HTTP error)
// ─────────────────────────────────────────────────────────────────────────

func TestSubmit_TransportError(t *testing.T) {
	// Server that closes connection immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	a := New(srv.URL, "k")
	_, err := a.Submit(context.Background(), "flux-pro-1.1", adapter.Params{"prompt": "x"}, "k")
	if err == nil {
		t.Fatal("expected error")
	}
	cls, _ := ErrorClass(err)
	if cls != adapter.ErrClassUpstream {
		t.Errorf("class = %q, want upstream", cls)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Poll: response body cannot be decoded as JSON (status 200, garbage body)
// ─────────────────────────────────────────────────────────────────────────

func TestPoll_BadJSON(t *testing.T) {
	fs := newFakeServer()
	fs.pollResponses = []serverResponse{
		{status: 200, body: []byte(`not json`)},
	}
	defer fs.Close()
	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	_, err := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// classifyPollStatus accepts an empty status string as Running
// ─────────────────────────────────────────────────────────────────────────

func TestClassifyPollStatus_EmptyStatus(t *testing.T) {
	pr, err := classifyPollStatus("flux-pro-1.1", []byte(`{}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr.Status != adapter.PollRunning {
		t.Errorf("status = %q, want running", pr.Status)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PollResponse error field cap (L4 fix)
// ─────────────────────────────────────────────────────────────────────────

func TestPollError_RawCappedAt8KiB(t *testing.T) {
	huge := make([]byte, adapter.MaxRawErrorBytes*2)
	for i := range huge {
		huge[i] = 'A'
	}
	// Wrap as a fake bfl_content_moderated body (status field still required).
	body := append([]byte(`{"status":"Content Moderated","extra":"`), huge...)
	body = append(body, []byte(`"}`)...)

	fs := newFakeServer()
	fs.pollResponses = []serverResponse{{status: 200, body: body}}
	defer fs.Close()

	a := New(fs.URL(), "k")
	ref := encodeUpstreamRef("abc-123", fs.URL()+"/poll")
	pr, _ := a.Poll(context.Background(), "flux-pro-1.1", ref)
	if pr.Error == nil {
		t.Fatal("Error is nil")
	}
	if len(pr.Error.Raw) > adapter.MaxRawErrorBytes {
		t.Errorf("Raw len = %d, want <= %d (L4 cap)", len(pr.Error.Raw), adapter.MaxRawErrorBytes)
	}
}
