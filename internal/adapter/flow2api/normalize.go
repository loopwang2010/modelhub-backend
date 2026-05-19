package flow2api

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
)

var (
	markdownImageRE = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)\)`)
	htmlVideoRE     = regexp.MustCompile(`(?i)<video[^>]+src=['"]([^'"]+)['"]`)
	bareURLRE       = regexp.MustCompile(`https?://[^\s)<>"']+`)
)

type chatCompletionResponse struct {
	URL     string `json:"url"`
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func normalizeChatCompletion(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	var payload chatCompletionResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if payload.Error != nil && strings.TrimSpace(payload.Error.Message) != "" {
		return nil, fmt.Errorf("upstream error: %s", payload.Error.Message)
	}

	content := extractMessageContent(payload)
	mediaURL := strings.TrimSpace(payload.URL)
	if mediaURL == "" {
		mediaURL = extractMediaURL(content)
	}
	if mediaURL == "" {
		return nil, fmt.Errorf("response did not contain a generated media URL")
	}

	spec, ok := modelSpecs[model]
	if !ok {
		return nil, fmt.Errorf("unsupported model %q", model)
	}
	kind, mimeType := outputKindAndMIME(spec, mediaURL)
	return &adapter.NormalizedResult{
		Modality: spec.Modality,
		Outputs: []adapter.Output{
			{
				Kind:     kind,
				URL:      mediaURL,
				MimeType: mimeType,
			},
		},
		Metadata: map[string]any{
			"source_model": spec.UpstreamModel,
		},
	}, nil
}

func extractMessageContent(payload chatCompletionResponse) string {
	if len(payload.Choices) == 0 {
		return ""
	}
	raw := payload.Choices[0].Message.Content
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				out = append(out, strings.TrimSpace(part.Text))
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func extractMediaURL(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if m := markdownImageRE.FindStringSubmatch(content); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	if m := htmlVideoRE.FindStringSubmatch(content); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	if m := bareURLRE.FindString(content); m != "" {
		return strings.TrimRight(m, ".,;")
	}
	return ""
}

func outputKindAndMIME(spec flowModelSpec, mediaURL string) (adapter.OutputKind, string) {
	if spec.Modality == adapter.ModalityVideo {
		return adapter.OutputKindVideoURL, mimeForURL(mediaURL, "video/mp4")
	}
	return adapter.OutputKindImageURL, mimeForURL(mediaURL, "image/png")
}

// ── OpenAI /v1/images/generations response normalization ──────────────────────

type imageGenerationsResponse struct {
	Created int64 `json:"created"`
	Data    []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func normalizeImageGenerations(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	var payload imageGenerationsResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if payload.Error != nil && strings.TrimSpace(payload.Error.Message) != "" {
		return nil, fmt.Errorf("upstream error: %s", payload.Error.Message)
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("response did not contain any generated images")
	}

	spec, ok := modelSpecs[model]
	if !ok {
		return nil, fmt.Errorf("unsupported model %q", model)
	}

	first := payload.Data[0]
	metadata := map[string]any{
		"source_model": spec.UpstreamModel,
	}
	if strings.TrimSpace(first.RevisedPrompt) != "" {
		metadata["revised_prompt"] = first.RevisedPrompt
	}
	if len(payload.Data) > 1 {
		metadata["additional_images"] = len(payload.Data) - 1
	}

	out := adapter.Output{
		MimeType: "image/png",
	}
	if strings.TrimSpace(first.URL) != "" {
		out.Kind = adapter.OutputKindImageURL
		out.URL = strings.TrimSpace(first.URL)
		out.MimeType = mimeForURL(out.URL, "image/png")
	} else if strings.TrimSpace(first.B64JSON) != "" {
		out.Kind = adapter.OutputKindBase64
		metadata["base64"] = first.B64JSON
	} else {
		return nil, fmt.Errorf("first image had neither url nor b64_json")
	}

	return &adapter.NormalizedResult{
		Modality: spec.Modality,
		Outputs:  []adapter.Output{out},
		Metadata: metadata,
	}, nil
}

func mimeForURL(rawURL, fallback string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fallback
	}
	ext := strings.ToLower(path.Ext(parsed.Path))
	if ext == "" {
		return fallback
	}
	if m := mime.TypeByExtension(ext); m != "" {
		if semi := strings.Index(m, ";"); semi >= 0 {
			return m[:semi]
		}
		return m
	}
	return fallback
}
