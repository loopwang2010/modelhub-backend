package bestai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

func TestSubmit_PostsOpenAICompatibleRequestAndNormalizesImage(t *testing.T) {
	var seenAuth string
	var seenIdem string
	var seenBody chatCompletionBody
	pollCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/v1/async/tasks/agt_1" {
				t.Fatalf("poll path = %s", r.URL.Path)
			}
			pollCount++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(asyncTaskEnvelope{
				ID:     "agt_1",
				Status: "succeeded",
				Response: &asyncTaskStoredResponse{
					StatusCode: http.StatusOK,
					Body:       json.RawMessage(`{"choices":[{"message":{"role":"assistant","content":"![Generated Image](https://files.example/out.png)"}}]}`),
				},
			})
			return
		}
		if r.URL.Path != "/v1/async/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		seenIdem = r.Header.Get("X-Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(asyncTaskEnvelope{ID: "agt_1", Status: "queued"})
	}))
	defer srv.Close()

	a, err := New(srv.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Submit(
		context.Background(),
		"gemini-3.1-flash-image-landscape",
		adapter.Params{"prompt": "make an image"},
		"idem-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer test-key" {
		t.Fatalf("auth = %q", seenAuth)
	}
	if seenIdem != "idem-1" {
		t.Fatalf("idempotency = %q", seenIdem)
	}
	if seenBody.Model != "gemini-3.1-flash-image-landscape" || seenBody.Stream {
		t.Fatalf("body = %+v", seenBody)
	}
	if pollCount != 1 {
		t.Fatalf("pollCount = %d", pollCount)
	}
	sync, ok := got.(adapter.SyncSubmit)
	if !ok {
		t.Fatalf("submit kind = %T", got)
	}
	if sync.Result.Outputs[0].Kind != adapter.OutputKindImageURL {
		t.Fatalf("kind = %s", sync.Result.Outputs[0].Kind)
	}
	if sync.Result.Outputs[0].URL != "https://files.example/out.png" {
		t.Fatalf("url = %s", sync.Result.Outputs[0].URL)
	}
}

func TestSubmit_RetriesAsyncSubmitWithIdempotencyKey(t *testing.T) {
	var submitCount int
	var seenIdempotency []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/async/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		submitCount++
		seenIdempotency = append(seenIdempotency, r.Header.Get("X-Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		if submitCount == 1 {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"retryable":true,"retry_after":1,"status":504}`))
			return
		}
		_ = json.NewEncoder(w).Encode(asyncTaskEnvelope{
			ID:     "agt_retry_1",
			Status: "succeeded",
			Response: &asyncTaskStoredResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"choices":[{"message":{"content":"https://files.example/retry.png"}}]}`),
			},
		})
	}))
	defer srv.Close()

	a, err := New(srv.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Submit(
		context.Background(),
		"gemini-3.1-flash-image-landscape",
		adapter.Params{"prompt": "retry image"},
		"idem-retry-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if submitCount != 2 {
		t.Fatalf("submitCount = %d", submitCount)
	}
	for _, idem := range seenIdempotency {
		if idem != "idem-retry-1" {
			t.Fatalf("idempotency = %#v", seenIdempotency)
		}
	}
	sync, ok := got.(adapter.SyncSubmit)
	if !ok {
		t.Fatalf("submit kind = %T", got)
	}
	if sync.Result.Outputs[0].URL != "https://files.example/retry.png" {
		t.Fatalf("url = %s", sync.Result.Outputs[0].URL)
	}
}

func TestSubmit_ForwardsImageURLParts(t *testing.T) {
	var messageContent any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messageContent = body.Messages[0].Content
		_ = json.NewEncoder(w).Encode(asyncTaskEnvelope{
			ID:     "agt_2",
			Status: "succeeded",
			Response: &asyncTaskStoredResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"url":"https://files.example/video.mp4","choices":[{"message":{"content":"ok"}}]}`),
			},
		})
	}))
	defer srv.Close()

	a, err := New(srv.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Submit(
		context.Background(),
		"veo_3_1_i2v_s_fast_fl",
		adapter.Params{
			"prompt":     "animate it",
			"image_urls": []any{"https://files.example/in.png"},
		},
		"idem-2",
	)
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := messageContent.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v", messageContent)
	}
	imagePart := parts[1].(map[string]any)
	if imagePart["type"] != "image_url" {
		t.Fatalf("image part = %#v", imagePart)
	}
}

func TestSubmit_OpenAIImageUsesOpenAIKeyAndNormalizesBase64(t *testing.T) {
	var seenAuth string
	var seenPath string
	var seenBody imageGenerationsBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(asyncTaskEnvelope{
			ID:     "agt_openai_1",
			Status: "succeeded",
			Response: &asyncTaskStoredResponse{
				StatusCode: http.StatusOK,
				Body:       json.RawMessage(`{"created":1,"data":[{"b64_json":"aW1hZ2UtYnl0ZXM=","revised_prompt":"revised"}]}`),
			},
		})
	}))
	defer srv.Close()

	a, err := NewWithKeys(srv.URL, "flow-key", "openai-key")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Submit(
		context.Background(),
		"gpt-image-2",
		adapter.Params{"prompt": "make an image", "size": "1024x1024", "n": 1},
		"idem-openai-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/v1/async/images/generations" {
		t.Fatalf("path = %s", seenPath)
	}
	if seenAuth != "Bearer openai-key" {
		t.Fatalf("auth = %q", seenAuth)
	}
	if seenBody.Model != "gpt-image-2" || seenBody.Prompt != "make an image" || seenBody.Size != "1024x1024" || seenBody.N != 1 {
		t.Fatalf("body = %+v", seenBody)
	}
	sync, ok := got.(adapter.SyncSubmit)
	if !ok {
		t.Fatalf("submit kind = %T", got)
	}
	if sync.Result.Outputs[0].Kind != adapter.OutputKindBase64 {
		t.Fatalf("kind = %s", sync.Result.Outputs[0].Kind)
	}
	if sync.Result.Metadata["base64"] != "aW1hZ2UtYnl0ZXM=" {
		t.Fatalf("metadata = %#v", sync.Result.Metadata)
	}
}

func TestNormalizeVideoHTML(t *testing.T) {
	a, err := New("https://flow.example", "key")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"choices":[{"message":{"content":"` + "```html\\n<video src='https://files.example/out.mp4' controls></video>\\n```" + `"}}]}`)
	got, err := a.NormalizeResult("veo_3_1_t2v_fast_landscape", body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Modality != adapter.ModalityVideo {
		t.Fatalf("modality = %s", got.Modality)
	}
	if got.Outputs[0].Kind != adapter.OutputKindVideoURL {
		t.Fatalf("kind = %s", got.Outputs[0].Kind)
	}
	if got.Outputs[0].MimeType != "video/mp4" {
		t.Fatalf("mime = %s", got.Outputs[0].MimeType)
	}
}

func TestSubmitHTTPErrorIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"},"retry_after":1}`))
	}))
	defer srv.Close()

	a, err := New(srv.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Submit(
		context.Background(),
		"gemini-3.1-flash-image-landscape",
		adapter.Params{"prompt": "x"},
		"idem-3",
	)
	if err == nil {
		t.Fatal("expected error")
	}
	var flowErr *Error
	if !errors.As(err, &flowErr) {
		t.Fatalf("err = %T", err)
	}
	if flowErr.Class != adapter.ErrClassRateLimit {
		t.Fatalf("class = %s", flowErr.Class)
	}
}

func TestShouldRetryBestAIHonorsRetryableCloudflareErrors(t *testing.T) {
	body := []byte(`{"cloudflare_error":true,"retryable":true,"status":524}`)
	if !shouldRetryBestAI(524, adapter.ErrClassUpstream, body) {
		t.Fatal("expected retry for retryable Cloudflare 524")
	}
}

func TestEstimateCost(t *testing.T) {
	a, err := New("https://flow.example", "key")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.EstimateCost("gemini-3.1-flash-image-landscape-4k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 150_000 {
		t.Fatalf("4k image cost = %d", got)
	}
	got, err = a.EstimateCost("veo_3_1_t2v_lite_landscape", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 200_000 {
		t.Fatalf("lite video cost = %d", got)
	}
	for _, spec := range generatedFlow2APIModels {
		got, err := a.EstimateCost(spec.Key, nil)
		if err != nil {
			t.Fatalf("EstimateCost(%s): %v", spec.Key, err)
		}
		if got <= 0 {
			t.Fatalf("EstimateCost(%s) = %d; want positive", spec.Key, got)
		}
	}
}

func TestNewFromEnvRequiresConfig(t *testing.T) {
	t.Setenv(envBaseURL, "")
	t.Setenv(envFlow2APIKey, "")
	t.Setenv(envOpenAIAPIKey, "")
	t.Setenv(legacyEnvBaseURL, "")
	t.Setenv(legacyEnvFlow2APIKey, "")
	t.Setenv(legacyEnvOpenAIImageKey, "")
	_, err := NewFromEnv()
	if err == nil || !strings.Contains(err.Error(), envBaseURL) {
		t.Fatalf("err = %v", err)
	}
}

func TestGeneratedFlow2APIModelsCoverSourceModelConfig(t *testing.T) {
	sourcePath := filepath.Clean(filepath.Join(
		"..", "..", "..", "..", "..",
		"ai-manage", "flow2api", "src", "services", "generation_handler.py",
	))
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("Flow2API source not present at %s", sourcePath)
		}
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^    "([^"]+)":\s*\{\s*$`)
	matches := re.FindAllStringSubmatch(string(source), -1)
	sourceKeys := make(map[adapter.ModelKey]struct{}, len(matches))
	for _, match := range matches {
		sourceKeys[adapter.ModelKey(match[1])] = struct{}{}
	}

	generatedKeys := flow2APISpecKeys()
	if len(generatedKeys) != len(sourceKeys) {
		t.Fatalf("generated Flow2API model count = %d; source MODEL_CONFIG count = %d", len(generatedKeys), len(sourceKeys))
	}
	for key := range sourceKeys {
		if _, ok := generatedKeys[key]; !ok {
			t.Fatalf("source model %s missing from generated catalog", key)
		}
	}
	for key := range generatedKeys {
		if _, ok := sourceKeys[key]; !ok {
			t.Fatalf("generated model %s not present in source MODEL_CONFIG", key)
		}
	}
}

func TestGeneratedFlow2APIInputValidation(t *testing.T) {
	a, err := New("https://flow.example", "key")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Validate("imagen-4.0-generate-preview-portrait", adapter.Params{
		"prompt":     "x",
		"image_urls": []any{"https://files.example/in.png"},
	}); err == nil || !strings.Contains(err.Error(), "does not accept image_urls") {
		t.Fatalf("imagen image_urls validation err = %v", err)
	}

	if err := a.Validate("veo_3_1_interpolation_lite_landscape", adapter.Params{"prompt": "x"}); err == nil || !strings.Contains(err.Error(), "requires at least 2") {
		t.Fatalf("interpolation missing images err = %v", err)
	}
	if err := a.Validate("veo_3_1_interpolation_lite_landscape", adapter.Params{
		"prompt":     "x",
		"image_urls": []any{"https://files.example/first.png", "https://files.example/last.png"},
	}); err != nil {
		t.Fatalf("interpolation valid params: %v", err)
	}

	if err := a.Validate("veo_3_1_r2v_fast", adapter.Params{
		"prompt":     "x",
		"image_urls": []any{"1", "2", "3", "4"},
	}); err == nil || !strings.Contains(err.Error(), "accepts at most 3") {
		t.Fatalf("r2v max-images err = %v", err)
	}
	if err := a.Validate("veo_3_1_r2v_fast", adapter.Params{"prompt": "x"}); err != nil {
		t.Fatalf("r2v with no reference images should be allowed by Flow2API config: %v", err)
	}

	if err := a.Validate("gemini-3.0-pro-image-portrait", adapter.Params{
		"prompt":     "x",
		"image_urls": []any{"1", "2", "3", "4", "5", "6", "7", "8", "9"},
	}); err == nil || !strings.Contains(err.Error(), "accepts at most 8") {
		t.Fatalf("gemini max-images err = %v", err)
	}
}

func TestFlow2APIVisibilityKnobs(t *testing.T) {
	defaultSpec := modelSpecs["gemini-3.1-flash-image-landscape"]
	advancedSpec := modelSpecs["gemini-3.0-pro-image-landscape"]
	longSpec := modelSpecs["veo_3_1_t2v_fast_4k"]

	t.Setenv(envExposeFlow2APIAdvanced, "")
	t.Setenv(envExposeFlow2APIUpsample, "")
	if !catalogEnabled(defaultSpec) {
		t.Fatal("default visible Flow2API model should be enabled")
	}
	if !catalogEnabled(advancedSpec) {
		t.Fatal("advanced Flow2API models should be enabled by default")
	}
	if catalogEnabled(longSpec) {
		t.Fatal("long-running Flow2API upsample models should be disabled by default")
	}

	t.Setenv(envExposeFlow2APIAdvanced, "false")
	if catalogEnabled(advancedSpec) {
		t.Fatal("advanced Flow2API model should honor MODELHUB_FLOW2API_EXPOSE_ADVANCED=false")
	}
	if !catalogEnabled(defaultSpec) {
		t.Fatal("default visible Flow2API model should ignore advanced=false")
	}

	t.Setenv(envExposeFlow2APIUpsample, "true")
	if !catalogEnabled(longSpec) {
		t.Fatal("long-running Flow2API model should honor MODELHUB_FLOW2API_EXPOSE_UPSAMPLE=true")
	}
}
