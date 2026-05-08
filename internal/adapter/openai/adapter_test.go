package openai

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// newTestAdapter constructs an OpenAIImageAdapter wired to the supplied
// httptest server URL with a memory fetcher seeded with a tiny PNG.
func newTestAdapter(t *testing.T, srv *httptest.Server) (*OpenAIImageAdapter, *memoryFetcher) {
	t.Helper()
	fetcher := newMemoryFetcher()
	fetcher.put("upload_abc", []byte("\x89PNG\r\n\x1a\nfake-png-bytes"), "image/png", "src.png")

	cfg := &clientConfig{
		apiKey:  "sk-test",
		baseURL: srv.URL,
		http:    srv.Client(),
	}
	return newWithConfig(cfg, fetcher), fetcher
}

func TestAdapter_KeyAndCapabilities(t *testing.T) {
	a := &OpenAIImageAdapter{}
	if a.Key() != ProviderKeyOpenAI {
		t.Errorf("Key() = %q, want %q", a.Key(), ProviderKeyOpenAI)
	}
	caps := a.Capabilities("gpt-image-1")
	if caps.SupportsWebhook || caps.SupportsCancel || caps.SupportsStreaming {
		t.Errorf("expected all sync caps false; got %+v", caps)
	}
	if caps.MaxConcurrent <= 0 {
		t.Errorf("expected MaxConcurrent > 0, got %d", caps.MaxConcurrent)
	}
}

func TestAdapter_Submit_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assertions on the upstream request shape.
		if r.URL.Path != editsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, editsPath)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth header = %q, want 'Bearer sk-test'", got)
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("content-type = %q, want multipart/form-data", ct)
		}

		// Parse multipart and assert required fields.
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("MultipartReader: %v", err)
		}
		seen := map[string]bool{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("NextPart: %v", err)
			}
			seen[part.FormName()] = true
			if part.FormName() == "image" {
				if part.FileName() == "" {
					t.Error("image filename missing")
				}
			}
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
		}
		for _, want := range []string{"model", "image", "prompt"} {
			if !seen[want] {
				t.Errorf("missing form field %q", want)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(readGolden(t, "response_b64_single.json"))
	}))
	defer srv.Close()

	a, _ := newTestAdapter(t, srv)
	res, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{
		"prompt":   "make it pop",
		"image_id": "upload_abc",
		"n":        1,
		"size":     "1024x1024",
	}, "idem-1")
	if err != nil {
		t.Fatalf("Submit err: %v", err)
	}
	sync, ok := res.(adapter.SyncSubmit)
	if !ok {
		t.Fatalf("Submit result = %T, want SyncSubmit", res)
	}
	if sync.Result == nil {
		t.Fatal("SyncSubmit.Result is nil")
	}
	if len(sync.Result.Outputs) != 1 || sync.Result.Outputs[0].Kind != adapter.OutputKindBase64 {
		t.Errorf("unexpected outputs: %+v", sync.Result.Outputs)
	}
	if sync.At.IsZero() {
		t.Error("SyncSubmit.At is zero")
	}
}

func TestAdapter_Submit_RejectsUnsupportedModel(t *testing.T) {
	a := newWithConfig(&clientConfig{apiKey: "x", baseURL: "http://nope", http: http.DefaultClient}, newMemoryFetcher())
	_, err := a.Submit(context.Background(), "claude-3-opus", adapter.Params{"prompt": "a", "image_id": "upload_abc"}, "idem-1")
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want wraps ErrInvalidParams", err)
	}
}

func TestAdapter_Submit_RejectsEmptyIdem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	a, _ := newTestAdapter(t, srv)
	_, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{"prompt": "x", "image_id": "upload_abc"}, "")
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestAdapter_Submit_RejectsEmptyPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	a, _ := newTestAdapter(t, srv)
	_, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{"prompt": "  ", "image_id": "upload_abc"}, "idem")
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestAdapter_Submit_RejectsMissingImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	a, _ := newTestAdapter(t, srv)
	_, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{"prompt": "x", "image_id": ""}, "idem")
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

// TestAdapter_Submit_ErrorMapping covers all class transitions.
func TestAdapter_Submit_ErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCls  adapter.ErrorClass
	}{
		{"401 invalid_api_key", 401, "error_401.json", adapter.ErrClassAuth},
		{"402 insufficient_quota", 402, "error_402.json", adapter.ErrClassPayment},
		{"429 rate_limit_exceeded", 429, "error_429.json", adapter.ErrClassRateLimit},
		{"400 content_policy_violation", 400, "error_400_content_policy.json", adapter.ErrClassContentPolicy},
		{"500 server_error", 500, "error_500.json", adapter.ErrClassUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := readGolden(t, tc.body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			a, _ := newTestAdapter(t, srv)
			_, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{"prompt": "x", "image_id": "upload_abc"}, "idem")
			if err == nil {
				t.Fatalf("expected error for status %d", tc.status)
			}
			var oerr *Error
			if !errors.As(err, &oerr) {
				t.Fatalf("err = %v (%T), want *Error", err, err)
			}
			if oerr.Class != tc.wantCls {
				t.Errorf("class = %s, want %s (status %d)", oerr.Class, tc.wantCls, tc.status)
			}
			if oerr.Status != tc.status {
				t.Errorf("status = %d, want %d", oerr.Status, tc.status)
			}
		})
	}
}

// 403 maps to payment per OpenAI billing-block convention.
func TestAdapter_ClassifyHTTPError_403(t *testing.T) {
	err := classifyHTTPError(403, []byte(`{"error":{"code":"billing_disabled","message":"x"}}`))
	var oerr *Error
	if !errors.As(err, &oerr) {
		t.Fatal("expected *Error")
	}
	if oerr.Class != adapter.ErrClassPayment {
		t.Errorf("class = %s, want payment", oerr.Class)
	}
}

// Body-code refinement: status 400 + invalid_api_key code → auth (rare
// in practice but defends against weird OpenAI edge cases).
func TestAdapter_ClassifyHTTPError_CodeOverridesStatus(t *testing.T) {
	err := classifyHTTPError(400, []byte(`{"error":{"code":"invalid_api_key","message":"bad key"}}`))
	var oerr *Error
	errors.As(err, &oerr)
	if oerr.Class != adapter.ErrClassAuth {
		t.Errorf("class = %s, want auth", oerr.Class)
	}
}

func TestAdapter_ClassifyTransportError_Timeout(t *testing.T) {
	err := classifyTransportError(errors.New("Get url: context deadline exceeded"))
	var oerr *Error
	errors.As(err, &oerr)
	if oerr.Class != adapter.ErrClassTimeout {
		t.Errorf("class = %s, want timeout", oerr.Class)
	}
}

func TestAdapter_ClassifyTransportError_Generic(t *testing.T) {
	err := classifyTransportError(errors.New("dial tcp: no such host"))
	var oerr *Error
	errors.As(err, &oerr)
	if oerr.Class != adapter.ErrClassUpstream {
		t.Errorf("class = %s, want upstream", oerr.Class)
	}
}

func TestAdapter_ClassifyTransportError_Nil(t *testing.T) {
	if classifyTransportError(nil) != nil {
		t.Error("nil in → nil out")
	}
}

// Submit fails fast with ErrInvalidParams when the upload_id can't be resolved.
func TestAdapter_Submit_UnknownUploadID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be reached")
	}))
	defer srv.Close()
	a, _ := newTestAdapter(t, srv)
	_, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{"prompt": "x", "image_id": "upload_does_not_exist"}, "idem")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAdapter_PollUnsupported(t *testing.T) {
	a := &OpenAIImageAdapter{}
	_, err := a.Poll(context.Background(), "gpt-image-1", "ref")
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("Poll err = %v, want ErrUnsupported", err)
	}
}

func TestAdapter_CancelUnsupported(t *testing.T) {
	a := &OpenAIImageAdapter{}
	err := a.Cancel(context.Background(), "gpt-image-1", "ref")
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("Cancel err = %v, want ErrUnsupported", err)
	}
}

func TestAdapter_VerifyWebhookUnsupported(t *testing.T) {
	a := &OpenAIImageAdapter{}
	_, err := a.VerifyWebhook(http.Header{}, []byte("{}"))
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("VerifyWebhook err = %v, want ErrUnsupported", err)
	}
}

func TestAdapter_NormalizeResult_PassesThrough(t *testing.T) {
	a := &OpenAIImageAdapter{}
	got, err := a.NormalizeResult("gpt-image-1", readGolden(t, "response_b64_single.json"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got.Outputs) != 1 {
		t.Errorf("outputs len = %d", len(got.Outputs))
	}
}

// ─────────────────────────────────────────────────────────────────────
// EstimateCost
// ─────────────────────────────────────────────────────────────────────

func TestEstimateCost_OverEstimatesAcrossSizes(t *testing.T) {
	a := &OpenAIImageAdapter{}
	cases := []struct {
		size string
	}{
		{"1024x1024"},
		{"1024x1536"},
		{"2048x2048"},
	}
	for _, tc := range cases {
		t.Run(tc.size, func(t *testing.T) {
			cost, err := a.EstimateCost("gpt-image-1", adapter.Params{
				"size":    tc.size,
				"quality": "standard",
				"n":       1,
			})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if cost <= 0 {
				t.Errorf("cost = %d, want > 0", cost)
			}
			// All gpt-image-1 estimates must over-estimate the published
			// $0.04 baseline (40_000 micro-USD) — we add a safety margin.
			if cost < 40_000 {
				t.Errorf("cost %d below the $0.04 baseline — under-estimating!", cost)
			}
		})
	}
}

func TestEstimateCost_ScalesWithN(t *testing.T) {
	a := &OpenAIImageAdapter{}
	c1, _ := a.EstimateCost("gpt-image-1", adapter.Params{"n": 1, "size": "1024x1024"})
	c2, _ := a.EstimateCost("gpt-image-1", adapter.Params{"n": 4, "size": "1024x1024"})
	if c2 <= c1 {
		t.Errorf("n=4 cost %d <= n=1 cost %d, expected scaling", c2, c1)
	}
}

func TestEstimateCost_HighQualityCostsMore(t *testing.T) {
	a := &OpenAIImageAdapter{}
	std, _ := a.EstimateCost("gpt-image-1", adapter.Params{"quality": "standard"})
	hd, _ := a.EstimateCost("gpt-image-1", adapter.Params{"quality": "high"})
	if hd <= std {
		t.Errorf("high quality cost %d not higher than standard %d", hd, std)
	}
}

func TestEstimateCost_RespectsHardCap(t *testing.T) {
	a := &OpenAIImageAdapter{}
	// gpt-image-1.5 + premium size + high quality + n=10 = should still
	// stay under MaxCostUSD ($1000) for these reasonable inputs.
	cost, err := a.EstimateCost("gpt-image-1.5", adapter.Params{
		"size":    "2048x2048",
		"quality": "high",
		"n":       10,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cost > adapter.MaxCostUSD {
		t.Errorf("cost %d exceeds MaxCostUSD %d", cost, adapter.MaxCostUSD)
	}
}

func TestEstimateCost_NUnknownClampedTo10(t *testing.T) {
	a := &OpenAIImageAdapter{}
	cost10, _ := a.EstimateCost("gpt-image-1", adapter.Params{"n": 10})
	cost999, _ := a.EstimateCost("gpt-image-1", adapter.Params{"n": 999})
	if cost10 != cost999 {
		t.Errorf("n=10 (%d) and n=999 (%d) should produce identical cost (clamp)", cost10, cost999)
	}
}

func TestEstimateCost_RejectsUnknownModel(t *testing.T) {
	a := &OpenAIImageAdapter{}
	_, err := a.EstimateCost("dall-e-2", adapter.Params{})
	if !errors.Is(err, adapter.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Multipart body helpers (param parsing edges)
// ─────────────────────────────────────────────────────────────────────

func TestBuildMultipartBody_OnlyRequiredFields(t *testing.T) {
	src := &SourceImage{Bytes: []byte("png-bytes"), MimeType: "image/png", Filename: "x.png"}
	body, ct, err := buildMultipartBody("gpt-image-1", "draw", adapter.Params{}, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	mr := multipart.NewReader(body, parseBoundary(ct))
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("part: %v", err)
		}
		seen[part.FormName()] = true
		_, _ = io.Copy(io.Discard, part)
	}
	for _, want := range []string{"model", "prompt", "image"} {
		if !seen[want] {
			t.Errorf("missing %q", want)
		}
	}
	for _, optional := range []string{"n", "size", "quality", "response_format", "user"} {
		if seen[optional] {
			t.Errorf("unexpected optional field %q present", optional)
		}
	}
}

func TestBuildMultipartBody_AllOptionalFields(t *testing.T) {
	src := &SourceImage{Bytes: []byte("data"), MimeType: "image/png", Filename: "x.png"}
	params := adapter.Params{
		"n":               2,
		"size":            "1024x1024",
		"quality":         "high",
		"response_format": "b64_json",
		"user":            "end-user-id",
	}
	body, ct, err := buildMultipartBody("gpt-image-1", "draw", params, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	mr := multipart.NewReader(body, parseBoundary(ct))
	got := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("part: %v", err)
		}
		buf, _ := io.ReadAll(part)
		got[part.FormName()] = string(buf)
	}
	if got["n"] != "2" || got["size"] != "1024x1024" || got["quality"] != "high" {
		t.Errorf("optional fields not forwarded: %v", got)
	}
	if got["user"] != "end-user-id" {
		t.Errorf("user field = %q", got["user"])
	}
}

// parseBoundary extracts the multipart boundary from a Content-Type.
func parseBoundary(contentType string) string {
	const marker = "boundary="
	idx := strings.Index(contentType, marker)
	if idx < 0 {
		return ""
	}
	return contentType[idx+len(marker):]
}

// ─────────────────────────────────────────────────────────────────────
// New(): construction tests
// ─────────────────────────────────────────────────────────────────────

func TestNew_RejectsNilFetcher(t *testing.T) {
	t.Setenv(envAPIKey, "sk-x")
	_, err := New(nil)
	if err == nil {
		t.Error("expected error for nil fetcher")
	}
}

func TestNew_RejectsMissingKey(t *testing.T) {
	t.Setenv(envAPIKey, "")
	_, err := New(newMemoryFetcher())
	if !errors.Is(err, adapter.ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

func TestNew_Happy(t *testing.T) {
	t.Setenv(envAPIKey, "sk-x")
	t.Setenv(envAPIBase, "https://example.com/")
	a, err := New(newMemoryFetcher())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Trailing slash stripped.
	if a.cfg.baseURL != "https://example.com" {
		t.Errorf("baseURL = %q, want trimmed", a.cfg.baseURL)
	}
	if a.cfg.apiKey != "sk-x" {
		t.Errorf("apiKey not loaded")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Manifest seeds
// ─────────────────────────────────────────────────────────────────────

func TestSeedManifests_AllValid(t *testing.T) {
	manifests, err := SeedManifests()
	if err != nil {
		t.Fatalf("seed err: %v", err)
	}
	if len(manifests) != 3 {
		t.Errorf("len = %d, want 3", len(manifests))
	}
	keys := map[adapter.ModelKey]bool{}
	for _, m := range manifests {
		keys[m.Key] = true
		if m.Provider != ProviderKeyOpenAI {
			t.Errorf("manifest %s provider = %s", m.Key, m.Provider)
		}
		if m.TaskKind != adapter.TaskKindSync {
			t.Errorf("manifest %s task_kind = %s", m.Key, m.TaskKind)
		}
		if m.Modality != adapter.ModalityEdit {
			t.Errorf("manifest %s modality = %s", m.Key, m.Modality)
		}
		if !containsTag(m.Tags, "image-edit") {
			t.Errorf("manifest %s missing 'image-edit' tag", m.Key)
		}
		if !containsTag(m.Tags, "sync") {
			t.Errorf("manifest %s missing 'sync' tag", m.Key)
		}
		if !containsTag(m.Tags, "multipart-via-upload") {
			t.Errorf("manifest %s missing 'multipart-via-upload' tag", m.Key)
		}
	}
	for _, want := range []adapter.ModelKey{"gpt-image-1", "gpt-image-1-mini", "gpt-image-1.5"} {
		if !keys[want] {
			t.Errorf("missing manifest %s", want)
		}
	}
}

func containsTag(tags []string, want string) bool {
	for _, tg := range tags {
		if tg == want {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// Init / shouldRegister
// ─────────────────────────────────────────────────────────────────────

func TestShouldRegister_DevModeFalsy(t *testing.T) {
	t.Setenv(adapter.DevModeEnvVar, adapter.DevModeValue)
	t.Setenv(envAPIKey, "sk-x")
	if shouldRegister() {
		t.Error("DEV_MODE=mock should disable register")
	}
}

func TestShouldRegister_NoKey(t *testing.T) {
	t.Setenv(adapter.DevModeEnvVar, "")
	t.Setenv(envAPIKey, "")
	if shouldRegister() {
		t.Error("missing key should disable register")
	}
}

func TestShouldRegister_KeyOnly(t *testing.T) {
	t.Setenv(adapter.DevModeEnvVar, "")
	t.Setenv(envAPIKey, "sk-x")
	if !shouldRegister() {
		t.Error("env-set, dev-off should enable register")
	}
}

func TestRegisterWithFetcher_DevModeIsNoOp(t *testing.T) {
	t.Setenv(adapter.DevModeEnvVar, adapter.DevModeValue)
	t.Setenv(envAPIKey, "sk-x")
	if err := RegisterWithFetcher(newMemoryFetcher()); err != nil {
		t.Errorf("dev mode should silently skip, got: %v", err)
	}
}

func TestRegisterWithFetcher_RegistersIntoOwnRegistry(t *testing.T) {
	t.Setenv(adapter.DevModeEnvVar, "")
	t.Setenv(envAPIKey, "sk-test-register")
	t.Setenv(envAPIBase, "")
	// We can't isolate the default registry without exporting it; use
	// Replace semantics by running register twice and checking the
	// final adapter is ours.
	if err := RegisterWithFetcher(newMemoryFetcher()); err != nil {
		t.Fatalf("first register: %v", err)
	}
	got, ok := adapter.Get(ProviderKeyOpenAI)
	if !ok {
		t.Fatal("adapter not in default registry after Register")
	}
	if _, ok := got.(*OpenAIImageAdapter); !ok {
		t.Errorf("registered adapter wrong type: %T", got)
	}
	if err := RegisterWithFetcher(newMemoryFetcher()); err != nil {
		t.Errorf("idempotent re-register: %v", err)
	}
	// Best-effort cleanup so we don't leak into other tests' state.
	adapter.DefaultRegistry().Unregister(ProviderKeyOpenAI)
}

// ─────────────────────────────────────────────────────────────────────
// Submit timing edge: HTTP returns then context cancels mid-read.
// ─────────────────────────────────────────────────────────────────────

func TestAdapter_Submit_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	a, _ := newTestAdapter(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := a.Submit(ctx, "gpt-image-1", adapter.Params{"prompt": "x", "image_id": "upload_abc"}, "idem")
	if err == nil {
		t.Error("expected ctx-deadline error")
	}
}

func TestAdapter_Submit_HappyPath_OnUpstreamMissingUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Same payload but stripped usage block.
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	a, _ := newTestAdapter(t, srv)
	res, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{"prompt": "x", "image_id": "upload_abc"}, "idem")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	sync := res.(adapter.SyncSubmit)
	if _, has := sync.Result.Metadata[metadataUsage]; has {
		t.Error("usage should be absent when upstream omits it")
	}
}

func TestAdapter_Submit_HappyPath_NormalizeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty data array — normalize will return errEmptyResponseData,
		// adapter wraps as ErrClassUpstream.
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	a, _ := newTestAdapter(t, srv)
	_, err := a.Submit(context.Background(), "gpt-image-1", adapter.Params{"prompt": "x", "image_id": "upload_abc"}, "idem")
	if err == nil {
		t.Fatal("expected normalize error")
	}
	var oerr *Error
	if !errors.As(err, &oerr) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if oerr.Class != adapter.ErrClassUpstream {
		t.Errorf("class = %s, want upstream", oerr.Class)
	}
}

// Param helpers — keep simple coverage.
func TestStringParam_Variants(t *testing.T) {
	if stringParam(adapter.Params{"k": "v"}, "k") != "v" {
		t.Error("string read failed")
	}
	if stringParam(adapter.Params{"k": 1}, "k") != "" {
		t.Error("non-string should be ignored")
	}
	if stringParam(adapter.Params{}, "k") != "" {
		t.Error("missing should be empty")
	}
}

func TestIntParam_Variants(t *testing.T) {
	if intParam(adapter.Params{"k": 5}, "k", 0) != 5 {
		t.Error("int read failed")
	}
	if intParam(adapter.Params{"k": int64(7)}, "k", 0) != 7 {
		t.Error("int64 read failed")
	}
	if intParam(adapter.Params{"k": float64(9)}, "k", 0) != 9 {
		t.Error("float64 read failed")
	}
	if intParam(adapter.Params{"k": "x"}, "k", 99) != 99 {
		t.Error("string should fall back")
	}
	if intParam(adapter.Params{}, "k", 42) != 42 {
		t.Error("missing should fall back")
	}
}

// Coverage backstop: exercise every documented size + quality value
// that the multiplier helpers branch on. Keeps the cost formula
// auditable and detects accidental table edits.
func TestSizeMultiplier_AllBranches(t *testing.T) {
	cases := map[string]float64{
		"":          1.0,
		"auto":      1.0,
		"1024x1024": 1.0,
		"1024x1536": 1.5,
		"1536x1024": 1.5,
		"1536x1536": 2.25,
		"2048x2048": 4.0,
		"256x256":   0.25,
		"512x512":   0.5,
		"unknown":   2.0,
		"   AUTO ":  1.0, // case-insensitive + trimmed
	}
	for in, want := range cases {
		if got := sizeMultiplier(in); got != want {
			t.Errorf("sizeMultiplier(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestQualityMultiplier_AllBranches(t *testing.T) {
	cases := map[string]float64{
		"":         1.0,
		"auto":     1.0,
		"medium":   1.0,
		"standard": 1.0,
		"high":     2.0,
		"hd":       2.0,
		"low":      0.5,
		"weird":    2.0,
		"  HIGH ":  2.0,
	}
	for in, want := range cases {
		if got := qualityMultiplier(in); got != want {
			t.Errorf("qualityMultiplier(%q) = %v, want %v", in, got, want)
		}
	}
}

// Test the n=0 / n<1 clamp branch in EstimateCost.
func TestEstimateCost_NegativeOrZeroNNormalized(t *testing.T) {
	a := &OpenAIImageAdapter{}
	c1, _ := a.EstimateCost("gpt-image-1", adapter.Params{"n": 0})
	c2, _ := a.EstimateCost("gpt-image-1", adapter.Params{"n": -3})
	c3, _ := a.EstimateCost("gpt-image-1", adapter.Params{"n": 1})
	if c1 != c3 || c2 != c3 {
		t.Errorf("non-positive n should clamp to 1: c1=%d c2=%d c3=%d", c1, c2, c3)
	}
}

// Cost ceiling overflow path: synthesize an absurd base by feeding
// every boost (size + quality + n) at once on a premium model.
// With current tables 80_000 * 4 * 2 * 10 * 1.5 = 9_600_000 (still
// under MaxCostUSD = 1_000_000_000). To hit the ceiling we'd need to
// engineer a rogue manifest — out of scope. Cover at least the
// non-error path explicitly.
func TestEstimateCost_NoCeilingHitForReasonableInputs(t *testing.T) {
	a := &OpenAIImageAdapter{}
	cost, err := a.EstimateCost("gpt-image-1.5", adapter.Params{"n": 10, "size": "2048x2048", "quality": "high"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cost <= 0 || cost >= adapter.MaxCostUSD {
		t.Errorf("cost %d out of bounds", cost)
	}
}

// withHTTPClient is currently uncovered by tests but is part of the
// test seam contract. Pin its semantics.
func TestClientConfig_WithHTTPClient(t *testing.T) {
	cfg := &clientConfig{apiKey: "x", baseURL: "https://e.com", http: http.DefaultClient}
	custom := &http.Client{Timeout: time.Second}
	dup := cfg.withHTTPClient(custom)
	if dup == cfg {
		t.Error("withHTTPClient should return a copy, not the original")
	}
	if dup.http != custom {
		t.Error("custom client not installed on copy")
	}
	if cfg.http == custom {
		t.Error("original mutated")
	}
}

// classifyHTTPError 408 path (timeout via status).
func TestAdapter_ClassifyHTTPError_408_Timeout(t *testing.T) {
	err := classifyHTTPError(408, []byte(""))
	var oerr *Error
	errors.As(err, &oerr)
	if oerr.Class != adapter.ErrClassTimeout {
		t.Errorf("class = %s, want timeout", oerr.Class)
	}
}

// 502 / 503 / 504 → upstream class regardless of body.
func TestAdapter_ClassifyHTTPError_5xxAlwaysUpstream(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		err := classifyHTTPError(status, []byte(`{"error":{"code":"rate_limit_exceeded"}}`))
		var oerr *Error
		errors.As(err, &oerr)
		// 504 → first set to timeout by status, then 5xx-fallthrough
		// re-overrides to upstream. Validate the documented behavior:
		// 5xx ALWAYS wins over body-code refinement.
		if oerr.Class != adapter.ErrClassUpstream {
			t.Errorf("status %d class = %s, want upstream", status, oerr.Class)
		}
	}
}

// Multipart body: upload of large source produces correctly-sized form.
func TestBuildMultipartBody_LargeSource(t *testing.T) {
	bigBytes := make([]byte, 1024*1024) // 1 MB
	for i := range bigBytes {
		bigBytes[i] = byte(i & 0xFF)
	}
	src := &SourceImage{Bytes: bigBytes, MimeType: "image/png", Filename: "big.png"}
	body, ct, err := buildMultipartBody("gpt-image-1", "edit", adapter.Params{"n": 1}, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	mr := multipart.NewReader(body, parseBoundary(ct))
	totalImageBytes := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("part: %v", err)
		}
		if part.FormName() == "image" {
			n, _ := io.Copy(io.Discard, part)
			totalImageBytes = int(n)
		} else {
			_, _ = io.Copy(io.Discard, part)
		}
	}
	if totalImageBytes != len(bigBytes) {
		t.Errorf("image bytes round-tripped %d, want %d", totalImageBytes, len(bigBytes))
	}
}

// SeedManifests should fail loudly if a manifest is malformed. We can't
// easily inject malformed without exposing internals, but we can call
// Validate() directly to confirm the schema-required fields shape works.
func TestManifests_AllValidate(t *testing.T) {
	manifests, err := SeedManifests()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, m := range manifests {
		if err := m.Validate(); err != nil {
			t.Errorf("manifest %s validate: %v", m.Key, err)
		}
		if len(m.InputSchema) == 0 {
			t.Errorf("manifest %s has empty input schema", m.Key)
		}
	}
}

// Quick sanity on Error.Error() format.
func TestError_Format(t *testing.T) {
	e := &Error{Class: adapter.ErrClassAuth, Status: 401, Code: "invalid_api_key", Message: "bad"}
	s := e.Error()
	if !strings.Contains(s, "401") || !strings.Contains(s, "auth") || !strings.Contains(s, "invalid_api_key") {
		t.Errorf("Error() format unexpected: %s", s)
	}
}
