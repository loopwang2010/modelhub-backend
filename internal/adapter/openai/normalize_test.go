package openai

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// readGolden returns the bytes of testdata/<name>. Fails the test if
// the file is missing — golden files are part of the package contract.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

func TestNormalizeImagesResponse_Base64Single(t *testing.T) {
	raw := readGolden(t, "response_b64_single.json")
	got, err := normalizeImagesResponse("gpt-image-1", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Modality != adapter.ModalityEdit {
		t.Errorf("modality = %s, want edit", got.Modality)
	}
	if len(got.Outputs) != 1 {
		t.Fatalf("outputs len = %d, want 1", len(got.Outputs))
	}
	out := got.Outputs[0]
	if out.Kind != adapter.OutputKindBase64 {
		t.Errorf("output kind = %s, want base64", out.Kind)
	}
	if out.URL != "" {
		t.Errorf("output URL = %q, want empty (S9.5 fills it)", out.URL)
	}
	if out.MimeType != "image/png" {
		t.Errorf("output mime = %q, want image/png", out.MimeType)
	}
	if out.SizeBytes <= 0 {
		t.Errorf("output size = %d, want > 0", out.SizeBytes)
	}
	// Inline assets stashed in metadata for S9.5 pickup.
	assets, ok := got.Metadata[metadataBase64Bytes].([]openaiInlineAsset)
	if !ok {
		t.Fatalf("metadata[%q] not []openaiInlineAsset; got %T",
			metadataBase64Bytes, got.Metadata[metadataBase64Bytes])
	}
	if len(assets) != 1 {
		t.Fatalf("inline assets len = %d, want 1", len(assets))
	}
	if len(assets[0].Bytes) == 0 {
		t.Errorf("decoded bytes are empty")
	}
	// Usage propagated.
	usage, ok := got.Metadata[metadataUsage].(map[string]any)
	if !ok {
		t.Fatalf("metadata[%q] missing or wrong type", metadataUsage)
	}
	if usage["total_tokens"].(int) != 6912 {
		t.Errorf("total_tokens = %v, want 6912", usage["total_tokens"])
	}
}

func TestNormalizeImagesResponse_Base64Multi(t *testing.T) {
	raw := readGolden(t, "response_b64_multi.json")
	got, err := normalizeImagesResponse("gpt-image-1", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Outputs) != 2 {
		t.Errorf("outputs len = %d, want 2", len(got.Outputs))
	}
	for i, o := range got.Outputs {
		if o.Kind != adapter.OutputKindBase64 {
			t.Errorf("output[%d].Kind = %s, want base64", i, o.Kind)
		}
	}
}

// AP-9: URL-form responses must normalize without panic. Output.URL is
// left empty (S9.5 fills) and the upstream URL is captured in metadata
// so S9.5 can fetch it.
func TestNormalizeImagesResponse_URLSingle(t *testing.T) {
	raw := readGolden(t, "response_url_single.json")
	got, err := normalizeImagesResponse("gpt-image-1.5", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Outputs) != 1 {
		t.Fatalf("outputs len = %d, want 1", len(got.Outputs))
	}
	out := got.Outputs[0]
	if out.Kind != adapter.OutputKindImageURL {
		t.Errorf("output kind = %s, want image_url", out.Kind)
	}
	if out.URL != "" {
		t.Errorf("output URL = %q, want empty (AP-19 — S9.5 rewrites)", out.URL)
	}
	urls, ok := got.Metadata["openai.upstream_urls"].([]string)
	if !ok || len(urls) != 1 {
		t.Fatalf("upstream_urls metadata missing or wrong type/len: %v", got.Metadata["openai.upstream_urls"])
	}
}

// AP-9 hard guard: refuse the ambiguous shape (both b64 AND url, or
// neither). Better to error than silently pick one.
func TestNormalizeImagesResponse_AmbiguousRejected(t *testing.T) {
	raw := readGolden(t, "response_ambiguous.json")
	_, err := normalizeImagesResponse("gpt-image-1", raw)
	if err == nil {
		t.Fatal("expected error for ambiguous data entry, got nil")
	}
	if !errors.Is(err, errAmbiguousImageDatum) {
		t.Errorf("err = %v, want errAmbiguousImageDatum", err)
	}
}

func TestNormalizeImagesResponse_EmptyData(t *testing.T) {
	raw := readGolden(t, "response_empty_data.json")
	_, err := normalizeImagesResponse("gpt-image-1", raw)
	if !errors.Is(err, errEmptyResponseData) {
		t.Errorf("err = %v, want errEmptyResponseData", err)
	}
}

func TestNormalizeImagesResponse_EmptyBody(t *testing.T) {
	_, err := normalizeImagesResponse("gpt-image-1", []byte{})
	if err == nil {
		t.Error("expected error for empty body")
	}
}

func TestNormalizeImagesResponse_Garbage(t *testing.T) {
	_, err := normalizeImagesResponse("gpt-image-1", []byte("not json"))
	if err == nil {
		t.Error("expected error for non-JSON body")
	}
}

func TestNormalizeImagesResponse_BodyContainsErrorEnvelope(t *testing.T) {
	raw := readGolden(t, "error_401.json")
	_, err := normalizeImagesResponse("gpt-image-1", raw)
	if err == nil {
		t.Fatal("expected error when body is an error envelope")
	}
}

func TestDecodeBase64Tolerant(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"std padded", "aGVsbG8=", "hello"},
		{"std unpadded", "aGVsbG8", "hello"},
		{"url-safe padded", "aGVsbG8=", "hello"},
		{"with whitespace", "  aGVsbG8=  ", "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeBase64Tolerant(tc.input)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeBase64Tolerant_Failures(t *testing.T) {
	cases := []string{"", "   ", "!!@@##", "not-base-64-at-all-####"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := decodeBase64Tolerant(c)
			if err == nil {
				t.Errorf("expected error for %q", c)
			}
		})
	}
}

func TestStringOrFallback(t *testing.T) {
	if stringOrFallback("a", "b") != "a" {
		t.Error("primary should win when non-empty")
	}
	if stringOrFallback("", "b") != "b" {
		t.Error("fallback should win when primary empty")
	}
}

func TestCaptureUpstreamURLs(t *testing.T) {
	data := []rawImageDatum{
		{B64JSON: "abc"},
		{URL: "https://example.com/1.png"},
		{URL: "https://example.com/2.png"},
		{B64JSON: "def"},
	}
	got := captureUpstreamURLs(data)
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}
