// Package googleai — response normalization for Vertex AI predictLongRunning.
//
// Two surfaces here:
//   1. NormalizeResult — turn an upstream operation-completion JSON into our
//      canonical NormalizedResult. Tests in adapter_test.go feed captured
//      golden bytes from testdata/ here.
//   2. parsePollResponse — interpret the operation-status doc returned by
//      GET /v1/{operation_name} into a PollResult.
//
// gs:// URL handling: the adapter emits the gs:// URI as-is in
// Output.URL. The S9.5 asset worker is responsible for downloading from GCS
// and rewriting to a CDN URL before the user ever sees it (per ticket
// T-001 in S7-S8 research). DO NOT pre-sign or rewrite here — that would
// couple the adapter to GCS SDKs and violate AP-1.
//
// Tests assert this package keeps the gs:// URL intact, so a future agent
// who tries to pre-sign here will see test failures.

package googleai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// vertexOperation is the canonical shape of a long-running operation.
// We only field-tag the bits we read; everything else stays as
// json.RawMessage to keep the contract loose.
//
// Reference: docs.cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations.publishers.models/predictLongRunning
type vertexOperation struct {
	Name     string          `json:"name"`
	Done     bool            `json:"done"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Response *vertexResponse `json:"response,omitempty"`
	Error    *vertexStatus   `json:"error,omitempty"`
}

type vertexResponse struct {
	Predictions []vertexPrediction `json:"predictions"`
}

type vertexPrediction struct {
	VideoURI string `json:"videoUri"`
	MimeType string `json:"mimeType,omitempty"`
	// Some Vertex schemas use bytesBase64Encoded for inline payloads. We
	// surface it through Metadata; users get the gs:// URL primarily.
	BytesBase64Encoded string `json:"bytesBase64Encoded,omitempty"`
}

// vertexStatus mirrors google.rpc.Status — the canonical error envelope.
type vertexStatus struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Status  string            `json:"status,omitempty"` // canonical text form, e.g., "INVALID_ARGUMENT"
	Details []json.RawMessage `json:"details,omitempty"`
}

// normalizeOperationResponse turns a successful operation body into our
// NormalizedResult. Returns an error if predictions are missing or the
// videoUri is empty (which would be a contract violation by the upstream).
func normalizeOperationResponse(model adapter.ModelKey, op *vertexOperation) (*adapter.NormalizedResult, error) {
	if op == nil || op.Response == nil {
		return nil, errors.New("googleai: operation response missing")
	}
	if len(op.Response.Predictions) == 0 {
		return nil, errors.New("googleai: response.predictions is empty")
	}
	outputs := make([]adapter.Output, 0, len(op.Response.Predictions))
	for i, pred := range op.Response.Predictions {
		if pred.VideoURI == "" {
			return nil, fmt.Errorf("googleai: prediction[%d] missing videoUri", i)
		}
		// TODO(S9.5): the gs:// URI is emitted as-is. The asset worker will
		// dispatch the right downloader (Google Cloud Storage SDK) per the
		// new ResultURLScheme cap field tracked in ticket T-001.
		mime := pred.MimeType
		if mime == "" {
			mime = "video/mp4"
		}
		outputs = append(outputs, adapter.Output{
			Kind:     adapter.OutputKindVideoURL,
			URL:      pred.VideoURI,
			MimeType: mime,
		})
	}
	meta := map[string]any{
		"adapter":      "google-ai",
		"model":        string(model),
		"upstream_ref": op.Name,
	}
	return &adapter.NormalizedResult{
		Modality: adapter.ModalityVideo,
		Outputs:  outputs,
		Metadata: meta,
	}, nil
}

// parseOperation unmarshals a raw operation body. Tolerates trailing
// whitespace, returns a wrapped error otherwise.
func parseOperation(raw []byte) (*vertexOperation, error) {
	if len(raw) == 0 {
		return nil, errors.New("googleai: empty operation body")
	}
	var op vertexOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		return nil, fmt.Errorf("googleai: invalid operation JSON: %w", err)
	}
	return &op, nil
}

// pollFromOperation builds a PollResult from a parsed operation.
func pollFromOperation(model adapter.ModelKey, op *vertexOperation, raw []byte) (adapter.PollResult, error) {
	if !op.Done {
		// Vertex AI doesn't expose a numeric progress fraction in the
		// metadata for Veo3; once done=false we report Running.
		// (The first poll right after Submit also returns Running rather
		// than Pending — Veo3 starts immediately and doesn't have a
		// distinct queued state we can detect.)
		return adapter.PollResult{Status: adapter.PollRunning}, nil
	}
	if op.Error != nil && op.Error.Code != 0 {
		class, message := classifyOperationError(op.Error)
		return adapter.PollResult{
			Status: adapter.PollFailed,
			Error: &adapter.PollError{
				Class:   class,
				Message: message,
				Raw:     capRaw(raw),
			},
		}, nil
	}
	res, err := normalizeOperationResponse(model, op)
	if err != nil {
		return adapter.PollResult{}, err
	}
	return adapter.PollResult{
		Status: adapter.PollSucceeded,
		Result: res,
	}, nil
}

// classifyOperationError maps a Vertex AI google.rpc.Status into our
// canonical ErrorClass.
//
// Code is the canonical numeric code per
// https://cloud.google.com/apis/design/errors#error_model
// 1=CANCELLED, 3=INVALID_ARGUMENT, 7=PERMISSION_DENIED,
// 8=RESOURCE_EXHAUSTED, 13=INTERNAL, 14=UNAVAILABLE, 16=UNAUTHENTICATED.
func classifyOperationError(s *vertexStatus) (adapter.ErrorClass, string) {
	if s == nil {
		return adapter.ErrClassUnknown, "googleai: unknown error"
	}
	msg := s.Message
	if msg == "" {
		msg = "googleai: upstream returned error without message"
	}
	// First, rough content-policy detection by message text — Google often
	// reports content rejections as code 3 INVALID_ARGUMENT with a message
	// containing "policy", "safety", "responsible", or "blocked".
	if isContentPolicyMessage(msg) {
		return adapter.ErrClassContentPolicy, msg
	}
	switch s.Code {
	case 16: // UNAUTHENTICATED
		return adapter.ErrClassAuth, msg
	case 7: // PERMISSION_DENIED — typically billing or org-policy
		return adapter.ErrClassPayment, msg
	case 8: // RESOURCE_EXHAUSTED
		return adapter.ErrClassRateLimit, msg
	case 3: // INVALID_ARGUMENT
		// Already screened for content policy above; otherwise treat as
		// upstream so we refund.
		return adapter.ErrClassUpstream, msg
	case 13, 14: // INTERNAL, UNAVAILABLE
		return adapter.ErrClassUpstream, msg
	case 4: // DEADLINE_EXCEEDED
		return adapter.ErrClassTimeout, msg
	case 5: // NOT_FOUND
		return adapter.ErrClassNotFound, msg
	}
	// Fall back to the canonical text form when present.
	switch strings.ToUpper(s.Status) {
	case "UNAUTHENTICATED":
		return adapter.ErrClassAuth, msg
	case "PERMISSION_DENIED":
		return adapter.ErrClassPayment, msg
	case "RESOURCE_EXHAUSTED":
		return adapter.ErrClassRateLimit, msg
	case "INTERNAL", "UNAVAILABLE":
		return adapter.ErrClassUpstream, msg
	case "DEADLINE_EXCEEDED":
		return adapter.ErrClassTimeout, msg
	case "NOT_FOUND":
		return adapter.ErrClassNotFound, msg
	}
	return adapter.ErrClassUpstream, msg
}

func isContentPolicyMessage(msg string) bool {
	lc := strings.ToLower(msg)
	for _, kw := range []string{
		"policy", "safety", "responsible", "blocked", "content was filtered",
		"prohibited", "violates",
	} {
		if strings.Contains(lc, kw) {
			return true
		}
	}
	return false
}

// classifyHTTPError maps an HTTP status + best-effort parsed response body
// into an ErrorClass. Used for the Submit/Poll/Cancel HTTP transport layer
// (i.e., when Vertex AI rejected the request itself, not when the
// long-running op completed with an error).
func classifyHTTPError(statusCode int, body []byte) (adapter.ErrorClass, string) {
	// Try to parse a google.rpc.Status envelope; HTTP errors usually carry
	// {"error":{"code":..., "message":..., "status":...}}.
	var probe struct {
		Error vertexStatus `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if err := json.Unmarshal(body, &probe); err == nil && probe.Error.Code != 0 {
		return classifyOperationError(&probe.Error)
	}
	switch {
	case statusCode == 401:
		return adapter.ErrClassAuth, fmt.Sprintf("googleai: upstream returned 401: %s", trimBody(msg))
	case statusCode == 403:
		// 403 from Vertex AI is most often billing/quota or IAM — both map
		// to refund-eligible payment class.
		return adapter.ErrClassPayment, fmt.Sprintf("googleai: upstream returned 403: %s", trimBody(msg))
	case statusCode == 404:
		return adapter.ErrClassNotFound, fmt.Sprintf("googleai: upstream returned 404: %s", trimBody(msg))
	case statusCode == 429:
		return adapter.ErrClassRateLimit, fmt.Sprintf("googleai: upstream returned 429: %s", trimBody(msg))
	case statusCode == 400:
		return adapter.ErrClassUpstream, fmt.Sprintf("googleai: bad request: %s", trimBody(msg))
	case statusCode >= 500:
		return adapter.ErrClassUpstream, fmt.Sprintf("googleai: upstream %d: %s", statusCode, trimBody(msg))
	}
	return adapter.ErrClassUnknown, fmt.Sprintf("googleai: unexpected status %d: %s", statusCode, trimBody(msg))
}

// trimBody truncates the body to a small suffix for human-readable errors.
// Full body still goes into PollError.Raw via capRaw at the operation layer.
func trimBody(body string) string {
	const maxLen = 256
	if len(body) <= maxLen {
		return body
	}
	return body[:maxLen] + "..."
}

// capRaw enforces adapter.MaxRawErrorBytes (8 KiB) so a chatty upstream
// can't blow our memory or log volume.
func capRaw(raw []byte) []byte {
	if len(raw) <= adapter.MaxRawErrorBytes {
		// Make a defensive copy so callers can't mutate our buffer.
		out := make([]byte, len(raw))
		copy(out, raw)
		return out
	}
	out := make([]byte, adapter.MaxRawErrorBytes)
	copy(out, raw[:adapter.MaxRawErrorBytes])
	return out
}
