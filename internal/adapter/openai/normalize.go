// normalize.go — translate raw OpenAI image-edit response bytes into
// our canonical NormalizedResult shape.
//
// Per ADR-018 (no-passthrough), OpenAI's response shape MUST NOT leak
// past this package's boundary. This file is the chokepoint: every byte
// that came from the upstream goes through normalizeImagesResponse().
//
// AP-9 enforcement: the normalizer handles BOTH base64 (`b64_json`,
// gpt-image-1's only mode today) AND URL (`url`, what gpt-image-1.5
// MAY support per OpenAI docs). We reject ambiguous/empty entries
// rather than coerce — silent degradation is worse than an explicit
// upstream-shape error.
//
// The decoded base64 bytes are NOT returned in NormalizedResult.Outputs
// directly — Output.URL is empty for OutputKindBase64. The bytes are
// stashed in NormalizedResult.Metadata under metadataBase64Bytes so the
// S9.5 asset worker can pick them up, upload to CDN, and rewrite the URL.
// This keeps the public contract clean (callers don't accidentally
// surface base64) while letting the asset worker do its job.

package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// metadataBase64Bytes is the well-known metadata key the S9.5 asset
// worker reads from NormalizedResult.Metadata to retrieve the decoded
// base64 payload(s) for upload to CDN. Value is []openaiInlineAsset.
const metadataBase64Bytes = "openai.inline_assets"

// metadataUsage is the well-known metadata key carrying OpenAI's
// reported token usage (`usage.total_tokens` etc.). The S6 wallet
// uses this at Settle time to reconcile the over-estimated Hold
// against actual cost.
const metadataUsage = "openai.usage"

// metadataModel records what upstream model actually executed. Useful
// for debugging when the request asked for "gpt-image-1" but OpenAI
// silently routed to a fallback (rare but observed historically).
const metadataModel = "openai.model"

// metadataCreated is the upstream's `created` Unix timestamp.
const metadataCreated = "openai.created_at"

// openaiInlineAsset is the per-output payload the asset worker needs.
// Exported lowercase for in-package use; never serialized over the wire.
type openaiInlineAsset struct {
	Bytes    []byte // decoded image bytes — uploaded to CDN by S9.5
	MimeType string // always "image/png" for gpt-image-1 today
}

// rawImagesResponse mirrors OpenAI's `/v1/images/edits` response shape.
// Fields are tolerant: we only assert what we use, we don't reject on
// unknown fields (forward-compat with new OpenAI fields).
type rawImagesResponse struct {
	Created int64               `json:"created"`
	Data    []rawImageDatum     `json:"data"`
	Model   string              `json:"model,omitempty"`
	Usage   *rawImageUsage      `json:"usage,omitempty"`
	Error   *rawImageErrorEntry `json:"error,omitempty"`
}

// rawImageDatum is a single image result from OpenAI. Either b64_json
// OR url is set — never both, never neither (per AP-9, we enforce).
type rawImageDatum struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// rawImageUsage mirrors the token-based usage block.
type rawImageUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// rawImageErrorEntry is OpenAI's structured error body. When present,
// the response status was non-2xx (or the response was a stub error
// shape). The adapter's HTTP layer decides which mapping to use; this
// type is here so the normalizer can recognize embedded error shapes.
type rawImageErrorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
}

// errEmptyResponseData is a sentinel for "response had no `data` array
// or it was empty". Reported up so callers can map to ErrClassUpstream.
var errEmptyResponseData = errors.New("openai: response data array missing or empty")

// errAmbiguousImageDatum is returned when a `data[i]` entry has BOTH
// b64_json and url set, OR neither. AP-9 explicitly: refuse to guess.
var errAmbiguousImageDatum = errors.New("openai: data entry has neither b64_json nor url, or has both")

// normalizeImagesResponse decodes raw response bytes into our canonical
// shape. The model parameter is informational only — used to populate
// metadata. The decoded bytes (for base64 outputs) live in Metadata,
// not in Output.URL.
//
// Returns ErrClassContentPolicy-tagged error when the embedded error
// indicates a content moderation rejection? No — the HTTP layer makes
// the class decision based on status code. This function only signals
// "the body was unparseable / empty / ambiguous". Status-code mapping
// happens in adapter.go.
func normalizeImagesResponse(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	if len(raw) == 0 {
		return nil, errors.New("openai: empty response body")
	}
	var probe rawImagesResponse
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("openai: response not JSON: %w", err)
	}
	if probe.Error != nil && probe.Error.Code != "" {
		// Body is an error envelope, not a result envelope.
		return nil, fmt.Errorf("openai: upstream error in response body: code=%s type=%s message=%s",
			probe.Error.Code, probe.Error.Type, probe.Error.Message)
	}
	if len(probe.Data) == 0 {
		return nil, errEmptyResponseData
	}

	outputs := make([]adapter.Output, 0, len(probe.Data))
	inlineAssets := make([]openaiInlineAsset, 0, len(probe.Data))

	for i, datum := range probe.Data {
		hasB64 := datum.B64JSON != ""
		hasURL := datum.URL != ""
		// AP-9: refuse the ambiguous shape rather than guess.
		if hasB64 == hasURL {
			return nil, fmt.Errorf("%w (index %d)", errAmbiguousImageDatum, i)
		}
		switch {
		case hasB64:
			decoded, err := decodeBase64Tolerant(datum.B64JSON)
			if err != nil {
				return nil, fmt.Errorf("openai: base64 decode failed at data[%d]: %w", i, err)
			}
			if len(decoded) == 0 {
				return nil, fmt.Errorf("openai: empty base64 payload at data[%d]", i)
			}
			outputs = append(outputs, adapter.Output{
				Kind:      adapter.OutputKindBase64,
				URL:       "", // S9.5 fills this after CDN upload
				MimeType:  "image/png",
				SizeBytes: int64(len(decoded)),
			})
			inlineAssets = append(inlineAssets, openaiInlineAsset{
				Bytes:    decoded,
				MimeType: "image/png",
			})
		case hasURL:
			// AP-19: we record the upstream URL in metadata only — the
			// public Output.URL is left empty until S9.5 fetches and
			// rehosts on our CDN. Never let an upstream URL escape.
			outputs = append(outputs, adapter.Output{
				Kind:      adapter.OutputKindImageURL,
				URL:       "", // CDN URL filled by S9.5
				MimeType:  "image/png",
				SizeBytes: 0, // unknown until S9.5 fetches
			})
		}
	}

	meta := map[string]any{
		metadataModel:   stringOrFallback(probe.Model, string(model)),
		metadataCreated: probe.Created,
	}
	if probe.Usage != nil {
		meta[metadataUsage] = map[string]any{
			"input_tokens":  probe.Usage.InputTokens,
			"output_tokens": probe.Usage.OutputTokens,
			"total_tokens":  probe.Usage.TotalTokens,
		}
	}
	if len(inlineAssets) > 0 {
		meta[metadataBase64Bytes] = inlineAssets
	}
	// Capture upstream URLs (if any) under a separate metadata key so
	// S9.5 can fetch them — kept out of inline_assets to avoid mixing
	// shapes. NOTE: this slice mirrors `outputs` 1:1 by index.
	if anyURLForm := captureUpstreamURLs(probe.Data); len(anyURLForm) > 0 {
		meta["openai.upstream_urls"] = anyURLForm
	}

	return &adapter.NormalizedResult{
		Modality: adapter.ModalityEdit,
		Outputs:  outputs,
		Metadata: meta,
	}, nil
}

// captureUpstreamURLs extracts any URL-form data entries so S9.5 can
// fetch them without re-parsing the raw response. Slice is dense (only
// non-empty URLs); index does NOT map 1:1 to outputs. Empty slice when
// every output was base64.
func captureUpstreamURLs(data []rawImageDatum) []string {
	out := make([]string, 0)
	for _, datum := range data {
		if datum.URL != "" {
			out = append(out, datum.URL)
		}
	}
	return out
}

// decodeBase64Tolerant accepts both standard and URL-safe base64,
// with or without padding. OpenAI's raw form is standard padded, but
// being tolerant here saves us from a future change in their wire
// format silently breaking the adapter.
func decodeBase64Tolerant(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty input")
	}
	// Try standard padded first (the documented form).
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	// Fall back to standard unpadded.
	if decoded, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	// Fall back to URL-safe variants.
	if decoded, err := base64.URLEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	return nil, errors.New("base64 decode: input did not match any known encoding")
}

// stringOrFallback returns primary if non-empty, else fallback.
func stringOrFallback(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
