package bestai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// ProviderKeyBestAI is the registry key for the bestai.codes/sub2api upstream.
const ProviderKeyBestAI adapter.ProviderKey = "bestai"

type Adapter struct {
	client *client
	nowFn  func() time.Time
}

func New(baseURL, apiKey string) (*Adapter, error) {
	c, err := newClient(baseURL, apiKey, defaultHTTPTimeout)
	if err != nil {
		return nil, err
	}
	return &Adapter{client: c, nowFn: time.Now}, nil
}

func NewWithKeys(baseURL, flow2apiKey, openAIKey string) (*Adapter, error) {
	c, err := newClientWithKeys(baseURL, flow2apiKey, openAIKey, defaultHTTPTimeout)
	if err != nil {
		return nil, err
	}
	return &Adapter{client: c, nowFn: time.Now}, nil
}

func NewFromEnv() (*Adapter, error) {
	c, err := newClientFromEnv()
	if err != nil {
		return nil, err
	}
	return &Adapter{client: c, nowFn: time.Now}, nil
}

func (*Adapter) Key() adapter.ProviderKey { return ProviderKeyBestAI }

func (*Adapter) Validate(model adapter.ModelKey, params adapter.Params) error {
	_, _, _, err := validateParams(model, params)
	return err
}

func (a *Adapter) Submit(ctx context.Context, model adapter.ModelKey, params adapter.Params, idem adapter.IdempotencyKey) (adapter.SubmitResult, error) {
	if idem == "" {
		return nil, fmt.Errorf("bestai: %w: empty idempotency key", adapter.ErrInvalidParams)
	}
	spec, prompt, imageURLs, err := validateParams(model, params)
	if err != nil {
		return nil, err
	}

	var (
		path string
		body any
	)
	switch spec.effectiveShape() {
	case ShapeOpenAIImages:
		size, n, perr := openAIImageParams(spec, params)
		if perr != nil {
			return nil, perr
		}
		path = "/v1/images/generations"
		body = buildImageGenerationsBody(spec.UpstreamModel, prompt, size, n)
	default:
		path = "/v1/chat/completions"
		body = buildChatCompletionBody(spec.UpstreamModel, prompt, imageURLs)
	}

	raw, status, class, err := a.client.postJSONWithKey(ctx, path, spec.effectiveAuthSelector(), idem, body)
	if err != nil {
		return nil, &Error{
			Class:   class,
			Status:  status,
			Message: err.Error(),
			Raw:     capRaw(raw),
		}
	}
	result, err := a.NormalizeResult(model, raw)
	if err != nil {
		return nil, &Error{Class: adapter.ErrClassUpstream, Status: status, Message: "normalize: " + err.Error(), Raw: capRaw(raw)}
	}
	return adapter.SyncSubmit{
		Result: result,
		At:     a.nowFn().UTC(),
	}, nil
}

func (*Adapter) Poll(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) (adapter.PollResult, error) {
	return adapter.PollResult{}, adapter.ErrUnsupported
}

func (*Adapter) Cancel(ctx context.Context, model adapter.ModelKey, ref adapter.UpstreamRef) error {
	return adapter.ErrUnsupported
}

func (*Adapter) EstimateCost(model adapter.ModelKey, params adapter.Params) (adapter.CostUSD, error) {
	spec, ok := modelSpecs[model]
	if !ok {
		return 0, fmt.Errorf("bestai: %w: unsupported model %q", adapter.ErrInvalidParams, model)
	}
	var cost adapter.CostUSD
	switch spec.Modality {
	case adapter.ModalityImage:
		cost = imageModelCost(model)
	case adapter.ModalityVideo:
		cost = videoModelCost(model)
	default:
		cost = 100_000
	}
	if cost > adapter.MaxCostUSD {
		return adapter.MaxCostUSD, fmt.Errorf("bestai: %w: model=%q", adapter.ErrCostCeilingExceeded, model)
	}
	return cost, nil
}

func (*Adapter) Capabilities(model adapter.ModelKey) adapter.ProviderCaps {
	return adapter.ProviderCaps{
		SupportsWebhook:   false,
		SupportsCancel:    false,
		SupportsStreaming: false,
		MaxConcurrent:     4,
	}
}

func (*Adapter) NormalizeResult(model adapter.ModelKey, raw []byte) (*adapter.NormalizedResult, error) {
	spec, ok := modelSpecs[model]
	if !ok {
		return nil, fmt.Errorf("unsupported model %q", model)
	}
	if spec.effectiveShape() == ShapeOpenAIImages {
		return normalizeImageGenerations(model, raw)
	}
	return normalizeChatCompletion(model, raw)
}

func (*Adapter) VerifyWebhook(headers http.Header, body []byte) (*adapter.WebhookVerification, error) {
	return nil, adapter.ErrUnsupported
}

func validateParams(model adapter.ModelKey, params adapter.Params) (modelSpec, string, []string, error) {
	if params == nil {
		return modelSpec{}, "", nil, fmt.Errorf("bestai: %w: nil params", adapter.ErrInvalidParams)
	}
	spec, ok := modelSpecs[model]
	if !ok {
		return modelSpec{}, "", nil, fmt.Errorf("bestai: %w: unsupported model %q", adapter.ErrInvalidParams, model)
	}
	prompt, err := promptFromParams(params)
	if err != nil {
		return modelSpec{}, "", nil, err
	}
	imageURLs, err := imageURLsFromParams(params)
	if err != nil {
		return modelSpec{}, "", nil, err
	}
	if len(imageURLs) < spec.MinImages {
		return modelSpec{}, "", nil, fmt.Errorf("bestai: %w: model %q requires at least %d image_urls", adapter.ErrInvalidParams, model, spec.MinImages)
	}
	if spec.MaxImages == 0 && len(imageURLs) > 0 {
		return modelSpec{}, "", nil, fmt.Errorf("bestai: %w: model %q does not accept image_urls", adapter.ErrInvalidParams, model)
	}
	if spec.MaxImages > 0 && len(imageURLs) > spec.MaxImages {
		return modelSpec{}, "", nil, fmt.Errorf("bestai: %w: model %q accepts at most %d image_urls", adapter.ErrInvalidParams, model, spec.MaxImages)
	}
	return spec, prompt, imageURLs, nil
}

type chatCompletionBody struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

type imageGenerationsBody struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	N      int    `json:"n,omitempty"`
	Size   string `json:"size,omitempty"`
}

func buildImageGenerationsBody(upstreamModel, prompt, size string, n int) imageGenerationsBody {
	return imageGenerationsBody{
		Model:  upstreamModel,
		Prompt: prompt,
		N:      n,
		Size:   size,
	}
}

// openAIImageParams pulls size/n out of params with bounds checks against the
// model's manifest spec. Both fields are optional; n defaults to 1.
func openAIImageParams(spec modelSpec, params adapter.Params) (string, int, error) {
	size := ""
	if raw, ok := params["size"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return "", 0, fmt.Errorf("bestai: %w: size must be a string", adapter.ErrInvalidParams)
		}
		size = strings.TrimSpace(s)
	}
	if size != "" && len(spec.Sizes) > 0 {
		allowed := false
		for _, s := range spec.Sizes {
			if s == size {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", 0, fmt.Errorf("bestai: %w: size %q not allowed for model %q", adapter.ErrInvalidParams, size, spec.Key)
		}
	}

	n := 1
	if raw, ok := params["n"]; ok && raw != nil {
		switch v := raw.(type) {
		case float64:
			n = int(v)
		case int:
			n = v
		default:
			return "", 0, fmt.Errorf("bestai: %w: n must be an integer", adapter.ErrInvalidParams)
		}
	}
	if n < 1 {
		n = 1
	}
	if spec.MaxN > 0 && n > spec.MaxN {
		return "", 0, fmt.Errorf("bestai: %w: n=%d exceeds max %d for model %q", adapter.ErrInvalidParams, n, spec.MaxN, spec.Key)
	}
	return size, n, nil
}

func buildChatCompletionBody(upstreamModel, prompt string, imageURLs []string) chatCompletionBody {
	content := any(prompt)
	if len(imageURLs) > 0 {
		parts := make([]chatContentPart, 0, 1+len(imageURLs))
		parts = append(parts, chatContentPart{Type: "text", Text: prompt})
		for _, u := range imageURLs {
			parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &imageURLPart{URL: u}})
		}
		content = parts
	}
	return chatCompletionBody{
		Model:    upstreamModel,
		Messages: []chatMessage{{Role: "user", Content: content}},
		Stream:   false,
	}
}

func promptFromParams(params adapter.Params) (string, error) {
	raw, ok := params["prompt"]
	if !ok {
		return "", fmt.Errorf("bestai: %w: prompt is required", adapter.ErrInvalidParams)
	}
	prompt, ok := raw.(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("bestai: %w: prompt must be a non-empty string", adapter.ErrInvalidParams)
	}
	return strings.TrimSpace(prompt), nil
}

func imageURLsFromParams(params adapter.Params) ([]string, error) {
	raw, ok := params["image_urls"]
	if !ok || raw == nil {
		return nil, nil
	}
	switch typed := raw.(type) {
	case []string:
		return cleanImageURLs(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("bestai: %w: image_urls must contain only strings", adapter.ErrInvalidParams)
			}
			out = append(out, s)
		}
		return cleanImageURLs(out)
	default:
		return nil, fmt.Errorf("bestai: %w: image_urls must be an array", adapter.ErrInvalidParams)
	}
}

func cleanImageURLs(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, u := range in {
		u = strings.TrimSpace(u)
		if u == "" {
			return nil, fmt.Errorf("bestai: %w: image_urls cannot contain empty strings", adapter.ErrInvalidParams)
		}
		out = append(out, u)
	}
	return out, nil
}

func imageModelCost(model adapter.ModelKey) adapter.CostUSD {
	key := strings.ToLower(string(model))
	switch {
	case key == "gpt-image-2":
		return 100_000
	case key == "gpt-image-1.5":
		return 60_000
	case key == "gpt-image-1":
		return 40_000
	case strings.Contains(key, "-4k"):
		return 150_000
	case strings.Contains(key, "-2k"):
		return 80_000
	default:
		return 40_000
	}
}

func videoModelCost(model adapter.ModelKey) adapter.CostUSD {
	key := strings.ToLower(string(model))
	base := adapter.CostUSD(800_000)
	switch {
	case strings.Contains(key, "lite"):
		base = 200_000
	case strings.Contains(key, "veo_2"):
		base = 400_000
	case strings.Contains(key, "ultra_relaxed"):
		base = 1_200_000
	case strings.Contains(key, "ultra"):
		base = 1_600_000
	}
	if strings.Contains(key, "_1080p") || strings.Contains(key, "-1080p") {
		base += 400_000
	}
	if strings.Contains(key, "_4k") || strings.Contains(key, "-4k") {
		base += 1_200_000
	}
	return base
}

type Error struct {
	Class   adapter.ErrorClass
	Status  int
	Message string
	Raw     []byte
}

func (e *Error) Error() string {
	return fmt.Sprintf("bestai: status=%d class=%s: %s", e.Status, e.Class, e.Message)
}

func (e *Error) ErrorClass() adapter.ErrorClass { return e.Class }

func capRaw(raw []byte) []byte {
	if len(raw) > adapter.MaxRawErrorBytes {
		return raw[:adapter.MaxRawErrorBytes]
	}
	return raw
}

func errorMessageFromBody(raw []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &payload)
	}
	if strings.TrimSpace(payload.Error.Message) != "" {
		return strings.TrimSpace(payload.Error.Message)
	}
	if payload.Detail != nil {
		if s, ok := payload.Detail.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 256 {
		return text[:256] + "...(truncated)"
	}
	return text
}

func classifyHTTPError(status int, raw []byte) (adapter.ErrorClass, string) {
	msg := errorMessageFromBody(raw)
	if msg == "" {
		msg = http.StatusText(status)
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "policy") || strings.Contains(lower, "safety") || strings.Contains(lower, "blocked") {
		return adapter.ErrClassContentPolicy, msg
	}
	switch status {
	case http.StatusUnauthorized:
		return adapter.ErrClassAuth, msg
	case http.StatusPaymentRequired, http.StatusForbidden:
		return adapter.ErrClassPayment, msg
	case http.StatusNotFound:
		return adapter.ErrClassNotFound, msg
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return adapter.ErrClassTimeout, msg
	case http.StatusTooManyRequests:
		return adapter.ErrClassRateLimit, msg
	default:
		if status >= 500 {
			return adapter.ErrClassUpstream, msg
		}
		return adapter.ErrClassUnknown, msg
	}
}
