//go:build integration

// Integration tests against the real api.bfl.ai. Requires BFL_API_KEY in env.
//
// Run: BFL_API_KEY=... go test -tags=integration -v -run Integration ./internal/adapter/bfl/...
//
// These tests are intentionally minimal — they prove the adapter end-to-end
// against the real upstream without driving up the test budget.

package bfl

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

func TestIntegration_GenerateOneImage(t *testing.T) {
	if os.Getenv(envAPIKey) == "" {
		t.Skipf("%s not set; skipping integration test", envAPIKey)
	}
	a, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := a.Submit(ctx, "flux-schnell-v1.5", adapter.Params{
		"prompt":       "a small red barn at the edge of a wheat field",
		"aspect_ratio": "1:1",
	}, "integration-smoke-1")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	async, ok := res.(adapter.AsyncSubmit)
	if !ok {
		t.Fatalf("expected AsyncSubmit, got %T", res)
	}
	t.Logf("submitted upstream ref: %s", async.UpstreamRef)

	// Poll with a generous backoff. The adapter does NOT sleep (AP-3); the test
	// is the worker stand-in here.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		pr, err := a.Poll(ctx, "flux-schnell-v1.5", async.UpstreamRef)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		switch pr.Status {
		case adapter.PollSucceeded:
			if pr.Result == nil || len(pr.Result.Outputs) == 0 {
				t.Fatal("Result missing or empty Outputs")
			}
			out := pr.Result.Outputs[0]
			if !strings.HasPrefix(out.URL, "https://") {
				t.Fatalf("output URL not https: %s", out.URL)
			}
			t.Logf("succeeded: %s", out.URL)
			return
		case adapter.PollFailed:
			t.Fatalf("upstream failed: %+v", pr.Error)
		}
		time.Sleep(2 * time.Second) // test-side backoff, not adapter-internal
	}
	t.Fatal("timeout waiting for image")
}

func TestIntegration_AuthFailure(t *testing.T) {
	if os.Getenv(envAPIKey) == "" {
		t.Skipf("%s not set; skipping integration test", envAPIKey)
	}
	// Override with a wrong key — the env-loaded one is fine.
	a := New("https://api.bfl.ai", "definitely-not-a-key")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := a.Submit(ctx, "flux-pro-1.1", adapter.Params{"prompt": "x"}, "integration-auth-1")
	if err == nil {
		t.Fatal("expected auth error")
	}
	cls, _ := ErrorClass(err)
	if cls != adapter.ErrClassAuth {
		t.Errorf("class = %q, want auth", cls)
	}
}
