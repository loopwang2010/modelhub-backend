// Package catalog defines the ModelManifest — the metadata that describes
// each model exposed via /v1/models, drives the auto-generated parameter
// form on the frontend, and gates Submit-time validation.
//
// Per ADR-007, this manifest is OUR canonical model description, derived
// per model from the actual upstream provider's docs. It is NOT
// the OGAI models_dump.json (which mirrors muapi's wrapper schema, not
// any original API). Every entry must be hand-validated against the
// upstream provider's actual documentation before going live.
package catalog

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// ModelManifest is the public-facing description of a single model variant.
//
// One row in our model_catalog DB table = one ModelManifest. The catalog
// service (S4) loads these from the DB and serves them via /v1/models.
// The frontend (S10) consumes the JSON form to render model cards + param forms.
type ModelManifest struct {
	// Key is the stable identifier exposed to API users.
	// Example: "flux-pro-1.1". Format is opaque — keys are unique strings.
	Key adapter.ModelKey `json:"key"`

	// Name is the human-readable display name shown in UIs.
	// Example: "Flux Pro 1.1".
	Name string `json:"name"`

	// Modality describes the OUTPUT shape (image / video / audio / edit / llm).
	Modality adapter.Modality `json:"modality"`

	// TaskKind describes the dispatch pattern (sync / async / streaming).
	TaskKind adapter.TaskKind `json:"task_kind"`

	// Provider is the upstream provider key. Maps to a registered ProviderAdapter.
	Provider adapter.ProviderKey `json:"provider"`

	// UpstreamModel is the model name as the UPSTREAM PROVIDER knows it.
	// Adapter.Submit uses this to build the upstream request.
	// Example: for our "flux-pro-1.1" → upstream BFL endpoint path includes "flux-pro-1.1".
	// MUST NOT leak into our public API surface (per ADR-018 no-passthrough rule).
	UpstreamModel string `json:"-"` // json:"-" → never serialized to /v1/models response

	// InputSchema is the JSON Schema (Draft 2020-12) for the params object.
	// Validates incoming requests at the API boundary BEFORE Submit is called.
	// Drives the frontend's auto-generated parameter form.
	//
	// Schema constraints SHOULD be expressed declaratively (min/max, enum, type, required).
	// Provider-specific quirks that can't be expressed declaratively (e.g., "width must
	// be divisible by 64") belong in the adapter's Submit, returning ErrInvalidParams.
	InputSchema json.RawMessage `json:"input_schema"`

	// PriceFormula is a human-readable pricing description shown to users.
	// Example: "$0.04 per image, +$0.02 if width*height > 1MP".
	// The MACHINE-readable cost is computed by the adapter's EstimateCost.
	// We keep both so users see the formula and machines compute the actual.
	PriceFormula string `json:"price_formula"`

	// Examples is a small set of pre-canned param sets the frontend uses
	// to populate "Try this" buttons in the playground.
	Examples []Example `json:"examples,omitempty"`

	// Tags is free-form labels for filtering: "image", "fast", "high-quality", etc.
	// Frontend renders these as chips on model cards.
	Tags []string `json:"tags,omitempty"`

	// Enabled toggles availability. When false, /v1/models hides this model
	// and Submit rejects requests for it. Used by admin to disable a model
	// without redeploying.
	Enabled bool `json:"-"`

	// Order controls display position in the model card grid (lower first).
	Order int `json:"order,omitempty"`

	// CreatedAt / UpdatedAt are catalog management metadata.
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

// Example is a pre-canned set of params the frontend uses for "Try this" buttons.
type Example struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Params      adapter.Params `json:"params"`
}

// ─────────────────────────────────────────────────────────────────────────
// Validation
// ─────────────────────────────────────────────────────────────────────────

// Validate checks that a manifest is internally consistent.
// Called when ingesting or admin-editing a manifest. Does NOT validate
// InputSchema against any specific request — that's done at request time.
func (m *ModelManifest) Validate() error {
	if m.Key == "" {
		return errors.New("manifest: Key is required")
	}
	if m.Name == "" {
		return errors.New("manifest: Name is required")
	}
	if m.Provider == "" {
		return errors.New("manifest: Provider is required")
	}
	if m.UpstreamModel == "" {
		return errors.New("manifest: UpstreamModel is required (cannot rely on Key alone)")
	}
	switch m.Modality {
	case adapter.ModalityImage, adapter.ModalityVideo, adapter.ModalityAudio,
		adapter.ModalityEdit, adapter.ModalityLLM:
		// ok
	default:
		return errors.New("manifest: Modality must be one of {image,video,audio,edit,llm}")
	}
	switch m.TaskKind {
	case adapter.TaskKindSync, adapter.TaskKindAsync, adapter.TaskKindStreaming:
		// ok
	default:
		return errors.New("manifest: TaskKind must be one of {sync,async,streaming}")
	}
	if len(m.InputSchema) == 0 {
		return errors.New("manifest: InputSchema is required")
	}
	// Confirm InputSchema is valid JSON (full JSON-Schema compliance is checked separately)
	var probe any
	if err := json.Unmarshal(m.InputSchema, &probe); err != nil {
		return errors.New("manifest: InputSchema must be valid JSON")
	}
	return nil
}

// PublicJSON returns the JSON representation suitable for /v1/models exposure.
// Strips fields that should not leak (UpstreamModel per ADR-018, internal timestamps).
// Reuses the standard json.Marshal because the struct already uses `json:"-"` tags
// for hidden fields — but kept as a method for clarity and to allow future
// per-tier filtering (e.g., admin sees more than user).
func (m *ModelManifest) PublicJSON() ([]byte, error) {
	return json.Marshal(m)
}
