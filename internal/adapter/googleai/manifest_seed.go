// Package googleai — catalog manifest seeds for Veo3 family.
//
// SeedManifests returns the set of catalog.ModelManifest rows the catalog
// service ingests on startup. Each manifest is hand-curated against the
// upstream Vertex AI docs for Veo3, NOT cribbed from any third party
// model-dump file (per ADR-007).
//
// Pricing source: https://cloud.google.com/vertex-ai/generative-ai/pricing.
// Adapters MUST verify pricing at registration time; a stale price formula
// is a financial bug. EstimateCost does the canonical math; PriceFormula is
// the human-readable string shown on the model card.

package googleai

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// veo3InputSchema is the JSON-Schema shared by the veo-3 family. Encoded
// once at init time so SeedManifests() doesn't allocate on every call.
//
// Constraints encoded here:
//   - prompt: required string, length 1..2000.
//   - duration_seconds: integer, 1..30. Vertex AI Veo3 generates clips up to
//     30s in one operation. Adapter falls back to 5s when unset (see
//     EstimateCost).
//   - aspect_ratio: enum (16:9, 9:16, 1:1).
//   - seed: optional integer for determinism (Vertex AI accepts -1 for random).
//   - person_generation: enum gating allow|disallow|allow_adult per
//     responsible-AI defaults.
//
// Schemas are intentionally LOOSE on the params shape — adapter.Submit
// translates this to the upstream "instances" + "parameters" body. We do
// not pre-bake the upstream wire format into the input schema.
var veo3InputSchema = mustJSON(map[string]any{
	"$schema":              "https://json-schema.org/draft/2020-12/schema",
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"prompt"},
	"properties": map[string]any{
		"prompt": map[string]any{
			"type":      "string",
			"minLength": 1,
			"maxLength": 2000,
		},
		"duration_seconds": map[string]any{
			"type":    "integer",
			"minimum": 1,
			"maximum": 30,
			"default": 5,
		},
		"aspect_ratio": map[string]any{
			"type":    "string",
			"enum":    []string{"16:9", "9:16", "1:1"},
			"default": "16:9",
		},
		"seed": map[string]any{
			"type":    "integer",
			"minimum": -1,
			"default": -1,
		},
		"negative_prompt": map[string]any{
			"type":      "string",
			"maxLength": 1000,
		},
		"person_generation": map[string]any{
			"type":    "string",
			"enum":    []string{"disallow", "allow_adult", "allow_all"},
			"default": "allow_adult",
		},
	},
})

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("googleai: bad seed schema: " + err.Error())
	}
	return b
}

// SeedManifests returns the catalog rows for this adapter. The catalog
// service idempotently merges these with any DB-side overrides; admin
// changes (e.g., disabling a model) win over the seed.
func SeedManifests() []catalog.ModelManifest {
	return []catalog.ModelManifest{
		{
			Key:           "veo-3.0-generate-preview",
			Name:          "Veo 3 (Preview)",
			Modality:      adapter.ModalityVideo,
			TaskKind:      adapter.TaskKindAsync,
			Provider:      ProviderName,
			UpstreamModel: "veo-3.0-generate-preview",
			InputSchema:   veo3InputSchema,
			PriceFormula:  "$0.50 per second of generated video",
			Tags:          []string{"video", "async", "long-running", "google", "veo3"},
			Order:         100,
			Enabled:       true,
			Examples: []catalog.Example{
				{
					Title:       "Cinematic 5s clip",
					Description: "A short cinematic test clip — useful for verifying the integration",
					Params: adapter.Params{
						"prompt":           "A cinematic shot of a cat chef serving a tiny soufflé in a copper kitchen, warm lighting, shallow depth of field",
						"duration_seconds": 5,
						"aspect_ratio":     "16:9",
					},
				},
			},
		},
		{
			Key:           "veo-3.0-fast-generate-preview",
			Name:          "Veo 3 Fast (Preview)",
			Modality:      adapter.ModalityVideo,
			TaskKind:      adapter.TaskKindAsync,
			Provider:      ProviderName,
			UpstreamModel: "veo-3.0-fast-generate-preview",
			InputSchema:   veo3InputSchema,
			PriceFormula:  "$0.25 per second of generated video (lower quality, faster)",
			Tags:          []string{"video", "async", "long-running", "google", "veo3", "fast"},
			Order:         101,
			Enabled:       true,
			Examples: []catalog.Example{
				{
					Title:       "Quick 5s clip",
					Description: "Same prompt as Veo 3 but uses the fast preview tier",
					Params: adapter.Params{
						"prompt":           "Cherry blossoms drifting onto a still pond at dawn, soft pastels",
						"duration_seconds": 5,
						"aspect_ratio":     "16:9",
					},
				},
			},
		},
	}
}
