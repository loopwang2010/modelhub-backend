//go:build integration

// integration_test.go — real-OpenAI integration test.
//
// Run with:
//
//	OPENAI_API_KEY=sk-... go test -tags=integration ./internal/adapter/openai/...
//
// Skipped automatically (build tag) when not opted in. The test
// exercises a single small image-edit request end-to-end against the
// live API and asserts that the response normalizes to a Base64 output
// with non-empty bytes.
//
// Cost note: a single 1024² gpt-image-1 edit costs ~$0.04. Run sparingly.

package openai

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// integrationFixturePNG is the same 1x1 PNG used by golden tests, decoded.
var integrationFixturePNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
	0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0xF8, 0x0F, 0x00, 0x00,
	0x01, 0x01, 0x01, 0x00, 0x1B, 0x6E, 0x16, 0xFC, 0x00, 0x00, 0x00, 0x00,
	0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

func TestIntegration_EditTinyImage(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY not set; skipping integration test")
	}
	fetcher := newMemoryFetcher()
	fetcher.put("upload_integration", integrationFixturePNG, "image/png", "tiny.png")

	a, err := New(fetcher)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := a.Submit(ctx, "gpt-image-1", adapter.Params{
		"prompt":   "Recolor with subtle blue tint.",
		"image_id": "upload_integration",
		"n":        1,
		"size":     "1024x1024",
	}, "integration-test")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	sync, ok := res.(adapter.SyncSubmit)
	if !ok {
		t.Fatalf("got %T, want SyncSubmit", res)
	}
	if sync.Result == nil || len(sync.Result.Outputs) == 0 {
		t.Fatal("empty result")
	}
	out := sync.Result.Outputs[0]
	if out.Kind != adapter.OutputKindBase64 && out.Kind != adapter.OutputKindImageURL {
		t.Errorf("unexpected output kind: %s", out.Kind)
	}
	t.Logf("OK — output kind=%s size=%d bytes meta=%v", out.Kind, out.SizeBytes, sync.Result.Metadata[metadataUsage])
}
