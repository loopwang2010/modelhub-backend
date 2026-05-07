package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// ─────────────────────────────────────────────────────────────────────────
// GenerationRequest
// ─────────────────────────────────────────────────────────────────────────

func TestGenerationRequest_ValidateOK(t *testing.T) {
	r := GenerationRequest{
		Model:  "flux-pro-1.1",
		Params: json.RawMessage(`{"prompt":"hi","width":1024}`),
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationRequest_ValidateRejectsEmptyModel(t *testing.T) {
	r := GenerationRequest{Params: json.RawMessage(`{"a":1}`)}
	if err := r.Validate(); err == nil {
		t.Fatal("expected error for empty model")
	}
}

func TestGenerationRequest_ValidateRejectsEmptyParams(t *testing.T) {
	r := GenerationRequest{Model: "m"}
	if err := r.Validate(); err == nil {
		t.Fatal("expected error for empty params")
	}
}

func TestGenerationRequest_ValidateRejectsNonObjectParams(t *testing.T) {
	cases := []string{`"string"`, `123`, `["array"]`, `null`, `not-json`}
	for _, c := range cases {
		r := GenerationRequest{Model: "m", Params: json.RawMessage(c)}
		if err := r.Validate(); err == nil {
			t.Errorf("expected error for params=%s", c)
		}
	}
}

func TestGenerationRequest_ValidateAcceptsHTTPSWebhook(t *testing.T) {
	r := GenerationRequest{
		Model:   "m",
		Params:  json.RawMessage(`{}`),
		Webhook: "https://hooks.example.com/cb",
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationRequest_ValidateRejectsRelativeWebhook(t *testing.T) {
	r := GenerationRequest{
		Model:   "m",
		Params:  json.RawMessage(`{}`),
		Webhook: "/relative-path",
	}
	if err := r.Validate(); err == nil {
		t.Fatal("expected error for relative webhook")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// EnvelopeFromTask
// ─────────────────────────────────────────────────────────────────────────

func newImageManifest() *catalog.ModelManifest {
	return &catalog.ModelManifest{
		Key:         "flux-pro-1.1",
		Name:        "Flux Pro 1.1",
		Modality:    adapter.ModalityImage,
		TaskKind:    adapter.TaskKindAsync,
		Provider:    "bfl",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Enabled:     true,
	}
}

func TestEnvelopeFromTask_SucceededImage(t *testing.T) {
	m := newImageManifest()
	now := time.Now().UTC()
	completed := now.Add(time.Second)
	ts := &TaskSnapshot{
		ID:          "gen_abc",
		Model:       m.Key,
		Status:      StatusSucceeded,
		CreatedAt:   now,
		CompletedAt: &completed,
		Result: &adapter.NormalizedResult{
			Modality: adapter.ModalityImage,
			Outputs: []adapter.Output{
				{
					Kind:      adapter.OutputKindImageURL,
					URL:       "https://cdn.modelhub.local/results/abc.png",
					MimeType:  "image/png",
					SizeBytes: 1234,
				},
			},
			Metadata: map[string]any{"seed": 42.0},
		},
		Credits: Credits{Held: 100, Settled: 90, Refunded: 10},
	}
	resp, err := EnvelopeFromTask(ts, m)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusSucceeded {
		t.Errorf("status = %q", resp.Status)
	}
	if resp.Modality != adapter.ModalityImage {
		t.Errorf("modality = %q", resp.Modality)
	}
	if resp.Output == nil {
		t.Fatal("expected Output")
	}
	if resp.Output.Type != adapter.OutputKindImageURL {
		t.Errorf("output type = %q", resp.Output.Type)
	}
	if resp.Output.URL != "https://cdn.modelhub.local/results/abc.png" {
		t.Errorf("URL = %q", resp.Output.URL)
	}
	if resp.Credits.Settled != 90 {
		t.Errorf("settled = %d", resp.Credits.Settled)
	}
}

func TestEnvelopeFromTask_RejectsNonCDNUrl_AP19(t *testing.T) {
	m := newImageManifest()
	now := time.Now().UTC()
	ts := &TaskSnapshot{
		ID:        "gen_x",
		Model:     m.Key,
		Status:    StatusSucceeded,
		CreatedAt: now,
		Result: &adapter.NormalizedResult{
			Modality: adapter.ModalityImage,
			Outputs: []adapter.Output{{
				Kind: adapter.OutputKindImageURL,
				URL:  "https://upstream.example.com/result.png", // upstream-shaped
			}},
		},
	}
	_, err := EnvelopeFromTask(ts, m)
	if err == nil {
		t.Fatal("expected AP-19 violation rejection")
	}
	if !strings.Contains(err.Error(), "AP-19") {
		t.Errorf("error %q does not mention AP-19", err.Error())
	}
}

func TestEnvelopeFromTask_FailedCarriesError(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:         "gen_x",
		Model:      m.Key,
		Status:     StatusFailed,
		CreatedAt:  time.Now().UTC(),
		ErrorClass: adapter.ErrClassRateLimit,
		ErrorMsg:   "upstream rate limit",
	}
	resp, err := EnvelopeFromTask(ts, m)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatal("expected Error")
	}
	if resp.Error.Code != "rate_limit" {
		t.Errorf("code = %q", resp.Error.Code)
	}
	if resp.Output != nil {
		t.Error("Output should be nil on failure")
	}
}

func TestEnvelopeFromTask_FailedDefaultErrorCode(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:        "gen_x",
		Model:     m.Key,
		Status:    StatusFailed,
		CreatedAt: time.Now().UTC(),
		// No ErrorClass set — should default to "unknown"
		ErrorMsg: "something broke",
	}
	resp, _ := EnvelopeFromTask(ts, m)
	if resp.Error.Code != "unknown" {
		t.Errorf("default code = %q", resp.Error.Code)
	}
}

func TestEnvelopeFromTask_RejectsNilArgs(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{Model: m.Key}
	if _, err := EnvelopeFromTask(nil, m); err == nil {
		t.Error("expected error for nil snapshot")
	}
	if _, err := EnvelopeFromTask(ts, nil); err == nil {
		t.Error("expected error for nil manifest")
	}
}

func TestEnvelopeFromTask_RejectsModelMismatch(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		Model:  "different-model",
		Status: StatusQueued,
	}
	if _, err := EnvelopeFromTask(ts, m); err == nil {
		t.Error("expected error for model mismatch")
	}
}

func TestEnvelopeFromTask_QueuedHasNoOutputOrError(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:        "gen_x",
		Model:     m.Key,
		Status:    StatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	resp, err := EnvelopeFromTask(ts, m)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output != nil || resp.Error != nil {
		t.Error("queued envelope must have neither Output nor Error")
	}
}

func TestEnvelopeFromTask_SucceededRejectsMissingResult(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:     "gen_x",
		Model:  m.Key,
		Status: StatusSucceeded,
		// Result missing
	}
	if _, err := EnvelopeFromTask(ts, m); err == nil {
		t.Error("expected error for succeeded-without-result")
	}
}

func TestEnvelopeFromTask_SucceededRejectsZeroOutputs(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:     "gen_x",
		Model:  m.Key,
		Status: StatusSucceeded,
		Result: &adapter.NormalizedResult{Outputs: []adapter.Output{}},
	}
	if _, err := EnvelopeFromTask(ts, m); err == nil {
		t.Error("expected error for zero outputs")
	}
}

func TestEnvelopeFromTask_RejectsImageURLMissingURL(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:     "gen_x",
		Model:  m.Key,
		Status: StatusSucceeded,
		Result: &adapter.NormalizedResult{
			Outputs: []adapter.Output{{Kind: adapter.OutputKindImageURL}},
		},
	}
	if _, err := EnvelopeFromTask(ts, m); err == nil {
		t.Error("expected error for missing URL")
	}
}

func TestEnvelopeFromTask_RejectsUnsupportedKind(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:     "gen_x",
		Model:  m.Key,
		Status: StatusSucceeded,
		Result: &adapter.NormalizedResult{
			Outputs: []adapter.Output{{Kind: "weird-new-kind"}},
		},
	}
	if _, err := EnvelopeFromTask(ts, m); err == nil {
		t.Error("expected error for unsupported kind")
	}
}

func TestEnvelopeFromTask_TextOutput(t *testing.T) {
	m := newImageManifest()
	m.Modality = adapter.ModalityLLM
	ts := &TaskSnapshot{
		ID:     "gen_x",
		Model:  m.Key,
		Status: StatusSucceeded,
		Result: &adapter.NormalizedResult{
			Outputs: []adapter.Output{{Kind: adapter.OutputKindText}},
			Metadata: map[string]any{
				"text": "hello world",
			},
		},
	}
	resp, err := EnvelopeFromTask(ts, m)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output.Text != "hello world" {
		t.Errorf("text = %q", resp.Output.Text)
	}
}

func TestEnvelopeFromTask_Base64Output(t *testing.T) {
	m := newImageManifest()
	ts := &TaskSnapshot{
		ID:     "gen_x",
		Model:  m.Key,
		Status: StatusSucceeded,
		Result: &adapter.NormalizedResult{
			Outputs: []adapter.Output{{Kind: adapter.OutputKindBase64}},
			Metadata: map[string]any{
				"base64": "dGVzdA==",
			},
		},
	}
	resp, _ := EnvelopeFromTask(ts, m)
	if resp.Output.Base64 != "dGVzdA==" {
		t.Errorf("base64 = %q", resp.Output.Base64)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// MapTaskStateToStatus
// ─────────────────────────────────────────────────────────────────────────

func TestMapTaskStateToStatus_AllStates(t *testing.T) {
	cases := map[string]Status{
		"created":    StatusQueued,
		"held":       StatusQueued,
		"submitted":  StatusQueued,
		"running":    StatusRunning,
		"succeeded":  StatusSucceeded,
		"settled":    StatusSucceeded,
		"asset_lost": StatusFailed,
		"failed":     StatusFailed,
		"timed_out":  StatusFailed,
		"cancelled":  StatusCancelled,
		"unknown_x":  StatusFailed, // fail-closed default
	}
	for from, want := range cases {
		got := MapTaskStateToStatus(from)
		if got != want {
			t.Errorf("MapTaskStateToStatus(%q) = %q, want %q", from, got, want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// JSON wire shape sanity (snapshot-style)
// ─────────────────────────────────────────────────────────────────────────

func TestGenerationResponse_JSONShape(t *testing.T) {
	completed := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	resp := GenerationResponse{
		ID:          "gen_a",
		Model:       "flux-pro-1.1",
		Status:      StatusSucceeded,
		Modality:    adapter.ModalityImage,
		TaskKind:    adapter.TaskKindAsync,
		CreatedAt:   time.Date(2026, 5, 7, 11, 59, 0, 0, time.UTC),
		CompletedAt: &completed,
		Output: &Output{
			Type:      adapter.OutputKindImageURL,
			URL:       "https://cdn.modelhub.local/x.png",
			MimeType:  "image/png",
			SizeBytes: 100,
		},
		Credits: Credits{Held: 100, Settled: 100},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	wantSubstrings := []string{
		`"id":"gen_a"`,
		`"model":"flux-pro-1.1"`,
		`"status":"succeeded"`,
		`"modality":"image"`,
		`"task_kind":"async"`,
		`"output":{`,
		`"credits":{`,
		`"created_at":"2026-05-07T11:59:00Z"`,
		`"completed_at":"2026-05-07T12:00:00Z"`,
	}
	got := string(b)
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("JSON missing %q\nfull: %s", s, got)
		}
	}
}

func TestGenerationResponse_QueuedJSONOmitsOutputAndError(t *testing.T) {
	resp := GenerationResponse{
		ID:        "gen_a",
		Model:     "m",
		Status:    StatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	b, _ := json.Marshal(resp)
	got := string(b)
	if strings.Contains(got, `"output"`) || strings.Contains(got, `"error"`) {
		t.Errorf("queued JSON unexpectedly includes output or error: %s", got)
	}
}
