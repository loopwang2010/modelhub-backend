package bestai

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

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/adapter"
)

const (
	envBaseURL        = "BESTAI_BASE_URL"
	envFlow2APIKey    = "BESTAI_FLOW2API_API_KEY"
	envOpenAIAPIKey   = "BESTAI_OPENAI_API_KEY"
	envTimeoutSeconds = "BESTAI_TIMEOUT_SECONDS"

	legacyEnvBaseURL        = "FLOW2API_BASE_URL"
	legacyEnvFlow2APIKey    = "FLOW2API_API_KEY"
	legacyEnvOpenAIImageKey = "FLOW2API_GPT_IMAGE_API_KEY"
	legacyEnvTimeoutSeconds = "FLOW2API_TIMEOUT_SECONDS"

	defaultHTTPTimeout = 15 * time.Minute
	maxResponseBytes   = 32 * 1024 * 1024
	maxSubmitAttempts  = 3
	asyncPollInterval  = 2 * time.Second

	authKeySelectorFlow2API = "flow2api"
	authKeySelectorOpenAI   = "openai"
)

type client struct {
	httpClient   *http.Client
	baseURL      string
	flow2apiKey  string
	openAIAPIKey string
}

func newClientFromEnv() (*client, error) {
	baseURL := envFirst(envBaseURL, legacyEnvBaseURL)
	flow2apiKey := envFirst(envFlow2APIKey, legacyEnvFlow2APIKey)
	openAIKey := envFirst(envOpenAIAPIKey, legacyEnvOpenAIImageKey)
	timeout := defaultHTTPTimeout
	if raw := envFirst(envTimeoutSeconds, legacyEnvTimeoutSeconds); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			return nil, fmt.Errorf("bestai: invalid %s %q", envTimeoutSeconds, raw)
		}
		timeout = time.Duration(seconds) * time.Second
	}
	return newClientWithKeys(baseURL, flow2apiKey, openAIKey, timeout)
}

func newClient(baseURL, apiKey string, timeout time.Duration) (*client, error) {
	return newClientWithKeys(baseURL, apiKey, "", timeout)
}

func newClientWithKeys(baseURL, flow2apiKey, openAIKey string, timeout time.Duration) (*client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	flow2apiKey = strings.TrimSpace(flow2apiKey)
	openAIKey = strings.TrimSpace(openAIKey)
	if baseURL == "" {
		return nil, fmt.Errorf("bestai: %w: %s is not set", adapter.ErrNotConfigured, envBaseURL)
	}
	if flow2apiKey == "" && openAIKey == "" {
		return nil, fmt.Errorf("bestai: %w: at least one of %s or %s is required", adapter.ErrNotConfigured, envFlow2APIKey, envOpenAIAPIKey)
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("bestai: invalid base URL %q", baseURL)
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	return &client{
		httpClient:   &http.Client{Timeout: timeout},
		baseURL:      baseURL,
		flow2apiKey:  flow2apiKey,
		openAIAPIKey: openAIKey,
	}, nil
}

func envFirst(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func (c *client) keyForSelector(selector string) (string, error) {
	switch selector {
	case authKeySelectorOpenAI:
		if c.openAIAPIKey == "" {
			return "", fmt.Errorf("bestai: %w: %s is not set for OpenAI image models", adapter.ErrNotConfigured, envOpenAIAPIKey)
		}
		return c.openAIAPIKey, nil
	case "", authKeySelectorFlow2API:
		if c.flow2apiKey == "" {
			return "", fmt.Errorf("bestai: %w: %s is not set for flow2api models", adapter.ErrNotConfigured, envFlow2APIKey)
		}
		return c.flow2apiKey, nil
	default:
		return "", fmt.Errorf("bestai: %w: unknown auth selector %q", adapter.ErrInvalidParams, selector)
	}
}

func (c *client) chatCompletionsURL() string {
	return c.baseURL + "/v1/chat/completions"
}

func (c *client) postJSON(ctx context.Context, path string, idem adapter.IdempotencyKey, body any) (raw []byte, status int, class adapter.ErrorClass, err error) {
	return c.postJSONWithKey(ctx, path, authKeySelectorFlow2API, idem, body)
}

func (c *client) postJSONWithKey(ctx context.Context, path, authSelector string, idem adapter.IdempotencyKey, body any) (raw []byte, status int, class adapter.ErrorClass, err error) {
	if asyncPath := asyncGatewayPathFor(path); asyncPath != "" {
		return c.postAsyncTaskAndWait(ctx, asyncPath, authSelector, idem, body)
	}
	for attempt := 1; attempt <= maxSubmitAttempts; attempt++ {
		raw, status, class, err = c.postJSONWithKeyOnce(ctx, path, authSelector, idem, body)
		if err == nil {
			return raw, status, class, nil
		}
		if attempt == maxSubmitAttempts || !shouldRetryBestAI(status, class, raw) {
			return raw, status, class, err
		}
		delay := retryDelayForBestAI(raw, attempt)
		if common.DebugEnabled {
			common.SysLog(fmt.Sprintf(
				"modelhub bestai upstream retry path=%s auth=%s status=%d class=%s attempt=%d/%d delay=%s error=%s",
				path,
				authSelector,
				status,
				class,
				attempt+1,
				maxSubmitAttempts,
				delay,
				truncateForLog(err.Error(), 512),
			))
		}
		select {
		case <-ctx.Done():
			return raw, status, adapter.ErrClassTimeout, fmt.Errorf("bestai: retry canceled: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
	return raw, status, class, err
}

type asyncTaskEnvelope struct {
	ID          string                    `json:"id"`
	Status      string                    `json:"status"`
	Model       string                    `json:"model"`
	MediaKind   string                    `json:"media_kind"`
	Attempts    int                       `json:"attempts"`
	MaxAttempts int                       `json:"max_attempts"`
	Response    *asyncTaskStoredResponse  `json:"response"`
	Error       *asyncTaskErrorDescriptor `json:"error"`
}

type asyncTaskStoredResponse struct {
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body"`
	BodyText   string          `json:"body_text"`
}

type asyncTaskErrorDescriptor struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func asyncGatewayPathFor(path string) string {
	switch strings.TrimSpace(path) {
	case "/v1/images/generations":
		return "/v1/async/images/generations"
	case "/v1/chat/completions":
		return "/v1/async/chat/completions"
	default:
		return ""
	}
}

func (c *client) postAsyncTaskAndWait(ctx context.Context, path, authSelector string, idem adapter.IdempotencyKey, body any) ([]byte, int, adapter.ErrorClass, error) {
	var task *asyncTaskEnvelope
	var raw []byte
	var status int
	var class adapter.ErrorClass
	var err error
	for attempt := 1; attempt <= maxSubmitAttempts; attempt++ {
		task, raw, status, class, err = c.postAsyncTaskOnce(ctx, path, authSelector, idem, body)
		if err == nil {
			break
		}
		if attempt == maxSubmitAttempts || !shouldRetryBestAI(status, class, raw) {
			return raw, status, class, err
		}
		delay := retryDelayForBestAI(raw, attempt)
		if common.DebugEnabled {
			common.SysLog(fmt.Sprintf(
				"modelhub bestai async submit retry path=%s auth=%s status=%d class=%s attempt=%d/%d delay=%s error=%s",
				path,
				authSelector,
				status,
				class,
				attempt+1,
				maxSubmitAttempts,
				delay,
				truncateForLog(err.Error(), 512),
			))
		}
		select {
		case <-ctx.Done():
			return raw, status, adapter.ErrClassTimeout, fmt.Errorf("bestai: async submit retry canceled: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
	for {
		if raw, status, class, err := asyncTaskResult(task); taskTerminal(task) {
			logBestAIAsyncTaskTerminal(task, authSelector)
			return raw, status, class, err
		}
		select {
		case <-ctx.Done():
			return raw, status, adapter.ErrClassTimeout, fmt.Errorf("bestai: async task polling canceled: %w", ctx.Err())
		case <-time.After(asyncPollInterval):
		}
		task, raw, status, class, err = c.getAsyncTaskOnce(ctx, task.ID, authSelector)
		if err != nil {
			if shouldRetryBestAI(status, class, raw) {
				continue
			}
			return raw, status, class, err
		}
	}
}

func logBestAIAsyncTaskTerminal(task *asyncTaskEnvelope, authSelector string) {
	if !common.DebugEnabled || task == nil {
		return
	}
	responseStatus := 0
	responseBytes := 0
	bodySummary := ""
	if task.Response != nil {
		responseStatus = task.Response.StatusCode
		body := []byte(task.Response.Body)
		if len(body) == 0 && strings.TrimSpace(task.Response.BodyText) != "" {
			body = []byte(strings.TrimSpace(task.Response.BodyText))
		}
		responseBytes = len(body)
		if len(body) > 0 {
			bodySummary = summarizeBestAIResponseForLog(body)
		}
	}
	errorMessage := ""
	if task.Error != nil {
		errorMessage = truncateForLog(strings.TrimSpace(task.Error.Message), 1000)
	}
	common.SysLog(fmt.Sprintf(
		"modelhub bestai async task terminal id=%s auth=%s status=%s model=%s media=%s attempts=%d/%d response_status=%d response_bytes=%d error=%s body=%s",
		task.ID,
		authSelector,
		task.Status,
		task.Model,
		task.MediaKind,
		task.Attempts,
		task.MaxAttempts,
		responseStatus,
		responseBytes,
		errorMessage,
		bodySummary,
	))
}

func (c *client) postAsyncTaskOnce(ctx context.Context, path, authSelector string, idem adapter.IdempotencyKey, body any) (*asyncTaskEnvelope, []byte, int, adapter.ErrorClass, error) {
	raw, status, class, err := c.postJSONWithKeyOnce(ctx, path, authSelector, idem, body)
	if err != nil {
		return nil, raw, status, class, err
	}
	task, parseErr := parseAsyncTask(raw)
	if parseErr != nil {
		return nil, raw, status, adapter.ErrClassUpstream, parseErr
	}
	if task.ID == "" {
		return nil, raw, status, adapter.ErrClassUpstream, fmt.Errorf("bestai: async task response missing id")
	}
	return task, raw, status, class, nil
}

func (c *client) getAsyncTaskOnce(ctx context.Context, taskID string, authSelector string) (*asyncTaskEnvelope, []byte, int, adapter.ErrorClass, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.baseURL, "/")+"/v1/async/tasks/"+url.PathEscape(taskID), nil)
	if err != nil {
		return nil, nil, 0, adapter.ErrClassUnknown, fmt.Errorf("bestai: build async poll request: %w", err)
	}
	key, err := c.keyForSelector(authSelector)
	if err != nil {
		return nil, nil, 0, adapter.ErrClassAuth, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, classifyTransport(err), fmt.Errorf("bestai: async poll http: %w", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, nil, resp.StatusCode, adapter.ErrClassUpstream, fmt.Errorf("bestai: read async poll response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		class, msg := classifyHTTPError(resp.StatusCode, raw)
		logBestAIUpstreamResponse("/v1/async/tasks/:id", authSelector, resp.StatusCode, raw)
		return nil, raw, resp.StatusCode, class, fmt.Errorf("bestai: async poll upstream %d: %s", resp.StatusCode, msg)
	}
	task, parseErr := parseAsyncTask(raw)
	if parseErr != nil {
		return nil, raw, resp.StatusCode, adapter.ErrClassUpstream, parseErr
	}
	return task, raw, resp.StatusCode, "", nil
}

func parseAsyncTask(raw []byte) (*asyncTaskEnvelope, error) {
	var task asyncTaskEnvelope
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, fmt.Errorf("bestai: parse async task response: %w", err)
	}
	task.Status = strings.ToLower(strings.TrimSpace(task.Status))
	return &task, nil
}

func taskTerminal(task *asyncTaskEnvelope) bool {
	if task == nil {
		return true
	}
	switch task.Status {
	case "succeeded", "failed", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

func asyncTaskResult(task *asyncTaskEnvelope) ([]byte, int, adapter.ErrorClass, error) {
	if task == nil {
		return nil, 0, adapter.ErrClassUpstream, fmt.Errorf("bestai: empty async task response")
	}
	switch task.Status {
	case "succeeded":
		if task.Response == nil {
			return nil, 0, adapter.ErrClassUpstream, fmt.Errorf("bestai: async task succeeded without response")
		}
		raw := []byte(task.Response.Body)
		if len(raw) == 0 && strings.TrimSpace(task.Response.BodyText) != "" {
			raw = []byte(strings.TrimSpace(task.Response.BodyText))
		}
		status := task.Response.StatusCode
		if status == 0 {
			status = http.StatusOK
		}
		if status < 200 || status >= 300 {
			class, msg := classifyHTTPError(status, raw)
			return raw, status, class, fmt.Errorf("bestai: async task upstream %d: %s", status, msg)
		}
		return raw, status, "", nil
	case "failed", "canceled", "cancelled":
		msg := "async task failed"
		if task.Error != nil && strings.TrimSpace(task.Error.Message) != "" {
			msg = strings.TrimSpace(task.Error.Message)
		}
		status := 0
		var raw []byte
		if task.Response != nil {
			status = task.Response.StatusCode
			raw = []byte(task.Response.Body)
			if len(raw) == 0 && strings.TrimSpace(task.Response.BodyText) != "" {
				raw = []byte(strings.TrimSpace(task.Response.BodyText))
			}
		}
		class := adapter.ErrClassUpstream
		if status > 0 {
			class, _ = classifyHTTPError(status, raw)
		}
		return raw, status, class, fmt.Errorf("bestai: %s: %s", task.Status, msg)
	default:
		return nil, 0, "", nil
	}
}

func (c *client) postJSONWithKeyOnce(ctx context.Context, path, authSelector string, idem adapter.IdempotencyKey, body any) (raw []byte, status int, class adapter.ErrorClass, err error) {
	var reqBody io.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, 0, adapter.ErrClassUnknown, fmt.Errorf("bestai: marshal request: %w", marshalErr)
		}
		reqBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+path, reqBody)
	if err != nil {
		return nil, 0, adapter.ErrClassUnknown, fmt.Errorf("bestai: build request: %w", err)
	}
	key, err := c.keyForSelector(authSelector)
	if err != nil {
		return nil, 0, adapter.ErrClassAuth, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("X-Idempotency-Key", string(idem))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, classifyTransport(err), fmt.Errorf("bestai: http: %w", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, resp.StatusCode, adapter.ErrClassUpstream, fmt.Errorf("bestai: read response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		class, msg := classifyHTTPError(resp.StatusCode, raw)
		logBestAIUpstreamResponse(path, authSelector, resp.StatusCode, raw)
		return raw, resp.StatusCode, class, fmt.Errorf("bestai: upstream %d: %s", resp.StatusCode, msg)
	}
	logBestAIUpstreamResponse(path, authSelector, resp.StatusCode, raw)
	return raw, resp.StatusCode, "", nil
}

func shouldRetryBestAI(status int, class adapter.ErrorClass, raw []byte) bool {
	if bestAIRetryable(raw) {
		return true
	}
	if status == 0 {
		return class == adapter.ErrClassTimeout || class == adapter.ErrClassUpstream
	}
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests:
		return true
	}
	return status >= 500 && status <= 599
}

func bestAIRetryable(raw []byte) bool {
	var payload struct {
		Retryable bool `json:"retryable"`
	}
	return len(raw) > 0 && json.Unmarshal(raw, &payload) == nil && payload.Retryable
}

func retryDelayForBestAI(raw []byte, attempt int) time.Duration {
	var payload struct {
		RetryAfter any `json:"retry_after"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &payload) == nil {
		if seconds := retryAfterSeconds(payload.RetryAfter); seconds > 0 {
			if seconds > 120 {
				seconds = 120
			}
			return time.Duration(seconds) * time.Second
		}
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(1<<attempt) * time.Second
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func retryAfterSeconds(raw any) int {
	switch v := raw.(type) {
	case float64:
		return int(v)
	case string:
		seconds, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return seconds
	default:
		return 0
	}
}

func logBestAIUpstreamResponse(path, authSelector string, status int, raw []byte) {
	if !common.DebugEnabled {
		return
	}
	common.SysLog(fmt.Sprintf(
		"modelhub bestai upstream response path=%s auth=%s status=%d bytes=%d body=%s",
		path,
		authSelector,
		status,
		len(raw),
		summarizeBestAIResponseForLog(raw),
	))
}

func summarizeBestAIResponseForLog(raw []byte) string {
	if len(raw) == 0 {
		return "<empty>"
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err == nil {
		redacted := redactBestAIResponseValue(payload)
		if encoded, marshalErr := json.Marshal(redacted); marshalErr == nil {
			return truncateForLog(string(encoded), 4096)
		}
	}
	return truncateForLog(strings.TrimSpace(string(raw)), 4096)
}

func redactBestAIResponseValue(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			lower := strings.ToLower(key)
			if lower == "b64_json" || strings.Contains(lower, "base64") {
				if s, ok := val.(string); ok {
					out[key] = fmt.Sprintf("<redacted base64 len=%d>", len(s))
				} else {
					out[key] = "<redacted base64>"
				}
				continue
			}
			out[key] = redactBestAIResponseValue(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, redactBestAIResponseValue(item))
		}
		return out
	case string:
		return truncateForLog(typed, 1000)
	default:
		return typed
	}
}

func truncateForLog(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "...(truncated)"
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
