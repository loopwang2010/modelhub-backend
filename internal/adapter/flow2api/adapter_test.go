package flow2api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

func TestSubmit_PostsOpenAICompatibleRequestAndNormalizesImage(t *testing.T) {
	var seenAuth string
	var seenIdem string
	var seenBody chatCompletionBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		seenIdem = r.Header.Get("X-Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"![Generated Image](https://files.example/out.png)"}}]}`))
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
		_, _ = w.Write([]byte(`{"url":"https://files.example/video.mp4","choices":[{"message":{"content":"ok"}}]}`))
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
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
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
}

func TestNewFromEnvRequiresConfig(t *testing.T) {
	t.Setenv(envBaseURL, "")
	t.Setenv(envAPIKey, "")
	_, err := NewFromEnv()
	if err == nil || !strings.Contains(err.Error(), envBaseURL) {
		t.Fatalf("err = %v", err)
	}
}
