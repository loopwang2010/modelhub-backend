package flow2api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

const (
	envBaseURL          = "FLOW2API_BASE_URL"
	envAPIKey           = "FLOW2API_API_KEY"
	envGPTImageAPIKey   = "FLOW2API_GPT_IMAGE_API_KEY"
	envTimeoutSeconds   = "FLOW2API_TIMEOUT_SECONDS"

	defaultHTTPTimeout = 10 * time.Minute
	maxResponseBytes   = 32 * 1024 * 1024

	// AuthKeySelector values keyed off flowModelSpec.AuthKeySelector.
	authKeySelectorDefault  = ""
	authKeySelectorGPTImage = "gpt_image"
)

type client struct {
	httpClient      *http.Client
	baseURL         string
	apiKey          string // default key (FLOW2API_API_KEY)
	gptImageAPIKey  string // optional override for openai_images shape; falls back to apiKey when unset
}

func newClientFromEnv() (*client, error) {
	baseURL := os.Getenv(envBaseURL)
	apiKey := os.Getenv(envAPIKey)
	gptImageKey := os.Getenv(envGPTImageAPIKey)
	timeout := defaultHTTPTimeout
	if raw := strings.TrimSpace(os.Getenv(envTimeoutSeconds)); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			return nil, fmt.Errorf("flow2api: invalid %s %q", envTimeoutSeconds, raw)
		}
		timeout = time.Duration(seconds) * time.Second
	}
	return newClientWithKeys(baseURL, apiKey, gptImageKey, timeout)
}

func newClient(baseURL, apiKey string, timeout time.Duration) (*client, error) {
	return newClientWithKeys(baseURL, apiKey, "", timeout)
}

func newClientWithKeys(baseURL, apiKey, gptImageKey string, timeout time.Duration) (*client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	gptImageKey = strings.TrimSpace(gptImageKey)
	if baseURL == "" {
		return nil, fmt.Errorf("flow2api: %w: %s is not set", adapter.ErrNotConfigured, envBaseURL)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("flow2api: %w: %s is not set", adapter.ErrNotConfigured, envAPIKey)
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("flow2api: invalid base URL %q", baseURL)
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	return &client{
		httpClient:     &http.Client{Timeout: timeout},
		baseURL:        baseURL,
		apiKey:         apiKey,
		gptImageAPIKey: gptImageKey,
	}, nil
}

func (c *client) keyForSelector(selector string) string {
	if selector == authKeySelectorGPTImage && c.gptImageAPIKey != "" {
		return c.gptImageAPIKey
	}
	return c.apiKey
}

func (c *client) chatCompletionsURL() string {
	return c.baseURL + "/v1/chat/completions"
}

func (c *client) postJSON(ctx context.Context, path string, idem adapter.IdempotencyKey, body any) (raw []byte, status int, class adapter.ErrorClass, err error) {
	return c.postJSONWithKey(ctx, path, "", idem, body)
}

// postJSONWithKey is like postJSON but uses the API key resolved by authSelector.
// Empty selector → default key (FLOW2API_API_KEY).
func (c *client) postJSONWithKey(ctx context.Context, path, authSelector string, idem adapter.IdempotencyKey, body any) (raw []byte, status int, class adapter.ErrorClass, err error) {
	var reqBody io.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, 0, adapter.ErrClassUnknown, fmt.Errorf("flow2api: marshal request: %w", marshalErr)
		}
		reqBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+path, reqBody)
	if err != nil {
		return nil, 0, adapter.ErrClassUnknown, fmt.Errorf("flow2api: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.keyForSelector(authSelector))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("X-Idempotency-Key", string(idem))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, classifyTransport(err), fmt.Errorf("flow2api: http: %w", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, resp.StatusCode, adapter.ErrClassUpstream, fmt.Errorf("flow2api: read response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		class, msg := classifyHTTPError(resp.StatusCode, raw)
		return raw, resp.StatusCode, class, fmt.Errorf("flow2api: upstream %d: %s", resp.StatusCode, msg)
	}
	return raw, resp.StatusCode, "", nil
}

func classifyTransport(err error) adapter.ErrorClass {
	if err == nil {
		return adapter.ErrClassUnknown
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "context deadline exceeded"):
		return adapter.ErrClassTimeout
	case strings.Contains(msg, "context canceled"):
		return adapter.ErrClassUnknown
	default:
		return adapter.ErrClassUpstream
	}
}
