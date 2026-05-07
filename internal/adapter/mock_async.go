// Mock async adapter — Submit returns AsyncSubmit, Poll cycles through
// Pending → Running → Succeeded over PollStepsToSucceed calls.
//
// This adapter purposefully does NOT sleep inside Poll (anti-pattern AP-3).
// The caller is expected to apply exponential backoff with jitter; the mock
// only advances state machinery on each call.
//
// Configurable knobs:
//   - PollStepsToSucceed: how many Poll calls until Succeeded (default 3)
//   - ForceSubmitError / ForcePollError: inject typed errors
//   - ProgressCurve: optional []float32 of progress fractions reported
//     during the Pending/Running phase (M1)
//   - WebhookSupported: when true, VerifyWebhook is enabled
//   - WebhookSecret: HMAC secret for VerifyWebhook
//
// Thread-safety: per-ref state is kept in a sync.Map so concurrent Polls
// for different refs don't contend. Per-ref Poll calls advance an int64
// counter atomically.

package adapter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// MockAsyncAdapter is a configurable in-memory async ProviderAdapter.
// Safe for concurrent use.
type MockAsyncAdapter struct {
	// PollStepsToSucceed is the number of Poll calls required before
	// PollSucceeded is returned. Default 3.
	PollStepsToSucceed int

	// ForceSubmitError, when non-empty, makes Submit return a typed error.
	ForceSubmitError ErrorClass

	// ForcePollError, when non-empty, makes Poll return PollFailed with this
	// class once the task reaches the "completion" step.
	ForcePollError ErrorClass

	// ProgressCurve is an optional slice of progress fractions. Index i is
	// reported on the i-th Poll call until len(ProgressCurve). Values clamped
	// to [0, 1]. nil = no progress reporting.
	ProgressCurve []float32

	// FixedCost overrides EstimateCost (default $0.06 = 60_000 micro-USD).
	FixedCost CostUSD

	// Caps overrides Capabilities; zero value = defaults below.
	Caps *ProviderCaps

	// WebhookSupported toggles VerifyWebhook support (default false).
	WebhookSupported bool

	// WebhookSecret is the HMAC-SHA256 secret used by VerifyWebhook to
	// authenticate the X-Mock-Signature header. Required when
	// WebhookSupported = true.
	WebhookSecret []byte

	// Modality / OutputKind / FakeURL / MimeType / SizeBytes shape the
	// result returned on Succeeded.
	Modality   Modality
	OutputKind OutputKind
	FakeURL    string
	MimeType   string
	SizeBytes  int64

	// SubmitCount / PollCount / CancelCount: atomic counters for tests.
	SubmitCount atomic.Int64
	PollCount   atomic.Int64
	CancelCount atomic.Int64

	// pollState tracks per-ref counters.
	pollState sync.Map // UpstreamRef → *atomic.Int64

	// canceledRefs marks refs that were Cancel()'d so Poll returns Failed.
	canceledRefs sync.Map // UpstreamRef → struct{}

	// refSeq is a monotonically increasing counter to mint unique refs.
	refSeq atomic.Int64
}

// NewMockAsyncAdapter returns a default-configured async mock.
func NewMockAsyncAdapter() *MockAsyncAdapter {
	return &MockAsyncAdapter{}
}

// Key returns the registry key.
func (*MockAsyncAdapter) Key() ProviderKey { return "mock-async" }

// Submit returns an AsyncSubmit pointing at a synthetic UpstreamRef.
// idempotencyKey MUST be non-empty.
func (m *MockAsyncAdapter) Submit(ctx context.Context, model ModelKey, params Params, idempotencyKey IdempotencyKey) (SubmitResult, error) {
	if idempotencyKey == "" {
		return nil, fmt.Errorf("mock-async: %w: empty idempotency key", ErrInvalidParams)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.SubmitCount.Add(1)
	if m.ForceSubmitError != "" {
		return nil, &mockError{Class: m.ForceSubmitError, Msg: fmt.Sprintf("mock-async: forced %s", m.ForceSubmitError)}
	}
	seq := m.refSeq.Add(1)
	ref := UpstreamRef(fmt.Sprintf("mock-async-ref-%s-%d", idempotencyKey, seq))
	return AsyncSubmit{UpstreamRef: ref, At: time.Now().UTC()}, nil
}

// Poll advances the per-ref counter and returns the next PollResult.
// AP-3: Poll does NOT sleep — caller owns backoff.
func (m *MockAsyncAdapter) Poll(ctx context.Context, model ModelKey, ref UpstreamRef) (PollResult, error) {
	if err := ctx.Err(); err != nil {
		return PollResult{}, err
	}
	m.PollCount.Add(1)

	if _, canceled := m.canceledRefs.Load(ref); canceled {
		return PollResult{
			Status: PollFailed,
			Error: &PollError{
				Class:   ErrClassUnknown,
				Message: "mock-async: task was cancelled",
			},
		}, nil
	}

	steps := m.PollStepsToSucceed
	if steps <= 0 {
		steps = 3
	}

	counter, _ := m.pollState.LoadOrStore(ref, &atomic.Int64{})
	c := counter.(*atomic.Int64)
	calls := c.Add(1) // calls = 1, 2, 3, ...

	progress := m.progressFor(calls)

	// Final-step branch — succeeded or forced-failed.
	if calls >= int64(steps) {
		if m.ForcePollError != "" {
			return PollResult{
				Status: PollFailed,
				Error: &PollError{
					Class:   m.ForcePollError,
					Message: fmt.Sprintf("mock-async: forced %s", m.ForcePollError),
				},
			}, nil
		}
		return PollResult{
			Status:   PollSucceeded,
			Progress: progress,
			Result:   m.synthesizeResult(model, ref),
		}, nil
	}

	// First call → Pending; subsequent → Running.
	if calls == 1 {
		return PollResult{Status: PollPending, Progress: progress}, nil
	}
	return PollResult{Status: PollRunning, Progress: progress}, nil
}

// Cancel records the ref as cancelled. Subsequent Poll calls for this ref
// return PollFailed with class=unknown. Returns nil to indicate the cancel
// request was accepted (not that upstream actually stopped).
func (m *MockAsyncAdapter) Cancel(ctx context.Context, model ModelKey, ref UpstreamRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.CancelCount.Add(1)
	caps := m.Capabilities(model)
	if !caps.SupportsCancel {
		return ErrUnsupported
	}
	m.canceledRefs.Store(ref, struct{}{})
	return nil
}

// EstimateCost returns FixedCost or the default $0.06.
func (m *MockAsyncAdapter) EstimateCost(model ModelKey, params Params) (CostUSD, error) {
	if m.FixedCost != 0 {
		return m.FixedCost, nil
	}
	return CostUSD(60_000), nil
}

// Capabilities returns Caps or default capabilities. The default async mock
// supports cancel; webhook support is gated by WebhookSupported.
func (m *MockAsyncAdapter) Capabilities(model ModelKey) ProviderCaps {
	if m.Caps != nil {
		return *m.Caps
	}
	return ProviderCaps{
		SupportsWebhook: m.WebhookSupported,
		SupportsCancel:  true,
	}
}

// NormalizeResult parses raw as JSON of the form {"ref":"...","model":"..."}
// and synthesizes a NormalizedResult. Empty/invalid raw is tolerated for tests.
func (m *MockAsyncAdapter) NormalizeResult(model ModelKey, raw []byte) (*NormalizedResult, error) {
	var probe struct {
		Ref UpstreamRef `json:"ref"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &probe) // tolerant: tests may pass anything
	}
	if probe.Ref == "" {
		probe.Ref = "normalize"
	}
	return m.synthesizeResult(model, probe.Ref), nil
}

// VerifyWebhook authenticates an HMAC-SHA256 signature in the
// X-Mock-Signature header (hex-encoded). Body is expected to be JSON of the
// form {"ref":"...","status":"succeeded|failed","error_class":"..."}.
//
// Returns ErrUnsupported when WebhookSupported = false.
func (m *MockAsyncAdapter) VerifyWebhook(headers http.Header, body []byte) (*WebhookVerification, error) {
	if !m.WebhookSupported {
		return nil, ErrUnsupported
	}
	sigHeader := headers.Get("X-Mock-Signature")
	if sigHeader == "" {
		return nil, fmt.Errorf("mock-async: missing X-Mock-Signature header")
	}
	gotSig, err := hex.DecodeString(sigHeader)
	if err != nil {
		return nil, fmt.Errorf("mock-async: invalid signature encoding: %w", err)
	}
	mac := hmac.New(sha256.New, m.WebhookSecret)
	mac.Write(body)
	expectSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, expectSig) {
		return nil, fmt.Errorf("mock-async: signature mismatch")
	}
	var payload struct {
		Ref        UpstreamRef `json:"ref"`
		Status     string      `json:"status"`
		ErrorClass ErrorClass  `json:"error_class"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("mock-async: invalid webhook body: %w", err)
	}
	if payload.Ref == "" {
		return nil, fmt.Errorf("mock-async: missing ref in webhook body")
	}
	verif := &WebhookVerification{UpstreamRef: payload.Ref}
	switch payload.Status {
	case "succeeded":
		verif.Result = PollResult{
			Status: PollSucceeded,
			Result: m.synthesizeResult("", payload.Ref),
		}
	case "failed":
		class := payload.ErrorClass
		if class == "" {
			class = ErrClassUnknown
		}
		verif.Result = PollResult{
			Status: PollFailed,
			Error: &PollError{
				Class:   class,
				Message: "mock-async: webhook reported failure",
			},
		}
	default:
		return nil, fmt.Errorf("mock-async: unsupported webhook status %q", payload.Status)
	}
	return verif, nil
}

// progressFor returns a clamped float32 pointer for the i-th call (1-indexed)
// or nil when no curve is configured.
func (m *MockAsyncAdapter) progressFor(call int64) *float32 {
	if len(m.ProgressCurve) == 0 {
		return nil
	}
	idx := int(call - 1)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(m.ProgressCurve) {
		idx = len(m.ProgressCurve) - 1
	}
	v := m.ProgressCurve[idx]
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return &v
}

func (m *MockAsyncAdapter) synthesizeResult(model ModelKey, ref UpstreamRef) *NormalizedResult {
	modality := m.Modality
	if modality == "" {
		modality = ModalityImage
	}
	kind := m.OutputKind
	if kind == "" {
		kind = OutputKindImageURL
	}
	url := m.FakeURL
	if url == "" {
		// AP-19: never an upstream-shaped URL.
		url = fmt.Sprintf("https://cdn.modelhub.local/mock/%s/%s.png", model, ref)
	}
	mime := m.MimeType
	if mime == "" {
		mime = "image/png"
	}
	size := m.SizeBytes
	if size == 0 {
		size = 2_097_152 // 2 MiB
	}
	return &NormalizedResult{
		Modality: modality,
		Outputs: []Output{
			{Kind: kind, URL: url, MimeType: mime, SizeBytes: size},
		},
		Metadata: map[string]any{
			"adapter":      "mock-async",
			"model":        string(model),
			"upstream_ref": string(ref),
		},
	}
}
