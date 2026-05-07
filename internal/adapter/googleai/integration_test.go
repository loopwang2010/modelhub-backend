//go:build integration

// Real Vertex AI Veo3 integration test. Skipped unless built with
// `-tags=integration` AND GOOGLE_APPLICATION_CREDENTIALS is set.
//
// Run:
//   $env:GOOGLE_APPLICATION_CREDENTIALS="C:/path/to/sa.json"
//   go test -tags=integration -timeout 10m ./internal/adapter/googleai/...
//
// The test issues a real Submit (5-second clip), polls until done, and
// asserts the gs:// URL scheme. It is NOT part of the regular CI run; the
// per-call cost (~$2.50 at $0.50/s × 5s) is non-trivial.

package googleai

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

func TestIntegration_Veo3SmallClip(t *testing.T) {
	if os.Getenv(CredentialsEnvVar) == "" {
		t.Skipf("integration: %s unset; skipping live Veo3 test", CredentialsEnvVar)
	}

	a, err := NewGoogleVertexAIAdapter(context.Background())
	if err != nil {
		if errors.Is(err, adapter.ErrNotConfigured) {
			t.Skipf("integration: adapter not configured: %v", err)
		}
		t.Fatalf("NewGoogleVertexAIAdapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// Smallest reasonable test clip — 5s in 16:9.
	model := adapter.ModelKey("veo-3.0-generate-preview")
	params := adapter.Params{
		"prompt":           "A tiny robot waving hello, simple test clip",
		"duration_seconds": 5,
		"aspect_ratio":     "16:9",
	}
	cost, err := a.EstimateCost(model, params)
	if err != nil {
		t.Fatalf("EstimateCost: %v", err)
	}
	if cost > adapter.MaxCostUSD {
		t.Fatalf("estimated cost %d exceeds MaxCostUSD %d", cost, adapter.MaxCostUSD)
	}
	t.Logf("integration: estimated cost %d micro-USD", cost)

	res, err := a.Submit(ctx, model, params, "integration-test-key")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	asyncRes, ok := res.(adapter.AsyncSubmit)
	if !ok {
		t.Fatalf("got %T", res)
	}
	t.Logf("integration: ref=%s", asyncRes.UpstreamRef)

	// Poll with exponential backoff. AP-3: caller owns sleep, not adapter.
	delay := 5 * time.Second
	const maxDelay = 60 * time.Second
	deadline := time.Now().Add(7 * time.Minute)
	for {
		if time.Now().After(deadline) {
			t.Fatal("integration: poll deadline exceeded")
		}
		pr, err := a.Poll(ctx, model, asyncRes.UpstreamRef)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		t.Logf("integration: poll status=%s", pr.Status)
		switch pr.Status {
		case adapter.PollSucceeded:
			if pr.Result == nil || len(pr.Result.Outputs) != 1 {
				t.Fatalf("succeeded but result shape bad: %+v", pr.Result)
			}
			url := pr.Result.Outputs[0].URL
			if !strings.HasPrefix(url, "gs://") {
				t.Errorf("expected gs:// URL, got %q", url)
			}
			t.Logf("integration: video at %s", url)
			return
		case adapter.PollFailed:
			t.Fatalf("Poll failed: %+v", pr.Error)
		}
		time.Sleep(delay)
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}
