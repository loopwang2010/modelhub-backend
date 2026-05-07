// Mock sync adapter — for tests, dev-mode boot, and as a reference
// implementation of the canonical sync ProviderAdapter contract.
//
// "Sync" here means: Submit returns the result inline (SyncSubmit). No Poll
// is required. The worker walks Created → Held → Submitted → Succeeded
// in one tick.
//
// Configurable knobs (all zero-valued = sensible defaults):
//   - SubmitDelay: synthetic latency before Submit returns
//   - ForceSubmitError: ErrorClass to inject; Submit returns a typed error
//   - FixedCost: override EstimateCost result
//   - Caps: override default Capabilities
//   - Modality / OutputKind / FakeURL: shape the NormalizedResult
//
// The default URL is "https://cdn.modelhub.local/mock/{idempotency_key}.png"
// — explicitly OUR-CDN-shaped (per AP-19 anti-pattern guard); never a leaked
// upstream URL.

package adapter

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// MockSyncAdapter is a configurable in-memory ProviderAdapter that returns
// SyncSubmit results. Safe for concurrent use; counters are atomic.
type MockSyncAdapter struct {
	// SubmitDelay is added before Submit returns (default 0).
	SubmitDelay time.Duration

	// ForceSubmitError, when non-empty, makes Submit return a sentinel error
	// whose Class matches. Useful for adapter-error fault-injection tests.
	ForceSubmitError ErrorClass

	// FixedCost, when non-zero, is what EstimateCost returns. Default $0.04.
	FixedCost CostUSD

	// Caps overrides Capabilities; zero value = defaults below.
	Caps *ProviderCaps

	// Modality controls the synthesized NormalizedResult.Modality.
	// Default: ModalityImage.
	Modality Modality

	// OutputKind controls Output.Kind. Default: OutputKindImageURL.
	OutputKind OutputKind

	// FakeURL is the URL stamped into Output.URL. If empty, a deterministic
	// CDN-shaped URL is built per call from the idempotency key.
	FakeURL string

	// MimeType for the synthesized output. Default: "image/png".
	MimeType string

	// SizeBytes for the synthesized output. Default: 1_048_576 (1 MiB).
	SizeBytes int64

	// SubmitCount tracks how many Submit calls have happened (read-only;
	// atomic). Tests assert against this.
	SubmitCount atomic.Int64
}

// NewMockSyncAdapter returns a default-configured sync mock.
func NewMockSyncAdapter() *MockSyncAdapter {
	return &MockSyncAdapter{}
}

// Key returns the registry key.
func (*MockSyncAdapter) Key() ProviderKey { return "mock-sync" }

// Submit synthesizes a NormalizedResult inline and returns SyncSubmit.
// idempotencyKey MUST be non-empty (caller-side AP-12 guard).
func (m *MockSyncAdapter) Submit(ctx context.Context, model ModelKey, params Params, idempotencyKey IdempotencyKey) (SubmitResult, error) {
	if idempotencyKey == "" {
		return nil, fmt.Errorf("mock-sync: %w: empty idempotency key", ErrInvalidParams)
	}
	if m.SubmitDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.SubmitDelay):
		}
	}
	m.SubmitCount.Add(1)
	if m.ForceSubmitError != "" {
		return nil, &mockError{Class: m.ForceSubmitError, Msg: fmt.Sprintf("mock-sync: forced %s", m.ForceSubmitError)}
	}
	now := time.Now().UTC()
	return SyncSubmit{
		Result: m.synthesizeResult(model, idempotencyKey),
		At:     now,
	}, nil
}

// Poll always returns ErrUnsupported on a sync adapter — by design no Poll
// path exists. Tests that walk a sync task should never call Poll.
func (*MockSyncAdapter) Poll(ctx context.Context, model ModelKey, ref UpstreamRef) (PollResult, error) {
	return PollResult{}, ErrUnsupported
}

// Cancel returns ErrUnsupported by default; sync tasks complete in one call
// and there's nothing meaningful to cancel.
func (*MockSyncAdapter) Cancel(ctx context.Context, model ModelKey, ref UpstreamRef) error {
	return ErrUnsupported
}

// EstimateCost returns FixedCost (or default $0.04 = 40_000 micro-USD).
func (m *MockSyncAdapter) EstimateCost(model ModelKey, params Params) (CostUSD, error) {
	if m.FixedCost != 0 {
		return m.FixedCost, nil
	}
	return CostUSD(40_000), nil
}

// Capabilities returns Caps if set, otherwise defaults: no webhook, no
// cancel, no streaming, no concurrency hint.
func (m *MockSyncAdapter) Capabilities(model ModelKey) ProviderCaps {
	if m.Caps != nil {
		return *m.Caps
	}
	return ProviderCaps{}
}

// NormalizeResult treats raw as opaque (sync mock has no upstream raw bytes
// to parse) and returns the synthesized result. Useful for golden-file tests
// against future real adapters where raw is meaningful.
func (m *MockSyncAdapter) NormalizeResult(model ModelKey, raw []byte) (*NormalizedResult, error) {
	return m.synthesizeResult(model, IdempotencyKey("normalize")), nil
}

// VerifyWebhook is unsupported on the sync mock (M2 sentinel handling).
func (*MockSyncAdapter) VerifyWebhook(headers http.Header, body []byte) (*WebhookVerification, error) {
	return nil, ErrUnsupported
}

// synthesizeResult builds a deterministic NormalizedResult for tests.
func (m *MockSyncAdapter) synthesizeResult(model ModelKey, key IdempotencyKey) *NormalizedResult {
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
		// CDN-shaped URL — never an upstream-shaped URL (AP-19).
		url = fmt.Sprintf("https://cdn.modelhub.local/mock/%s/%s.png", model, key)
	}
	mime := m.MimeType
	if mime == "" {
		mime = "image/png"
	}
	size := m.SizeBytes
	if size == 0 {
		size = 1_048_576
	}
	return &NormalizedResult{
		Modality: modality,
		Outputs: []Output{
			{Kind: kind, URL: url, MimeType: mime, SizeBytes: size},
		},
		Metadata: map[string]any{
			"adapter":         "mock-sync",
			"model":           string(model),
			"idempotency_key": string(key),
		},
	}
}

// mockError is a typed error used by the mocks to surface ErrorClass.
type mockError struct {
	Class ErrorClass
	Msg   string
}

func (e *mockError) Error() string { return e.Msg }

// MockErrorClass extracts ErrorClass from an error returned by mock adapters.
// Returns "", false if err is not a mock-typed error.
func MockErrorClass(err error) (ErrorClass, bool) {
	if me, ok := err.(*mockError); ok {
		return me.Class, true
	}
	return "", false
}
