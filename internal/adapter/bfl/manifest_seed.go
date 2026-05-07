// Manifest seed entries for BFL Flux models.
//
// Per ADR-007, every entry is hand-validated against BFL docs and links
// back to the upstream source URL via PriceFormula / code comments.
//
// Per ADR-018 (no-passthrough), Manifest.UpstreamModel is the BFL endpoint
// path component; it is `json:"-"` and never reaches /v1/models response.
//
// AssetCostFraction note (S6 wallet design):
//   The locked ModelManifest struct does not yet carry AssetCostFraction
//   (it lands when the wallet PR opens). Until then, AssetCostFractions()
//   exposes the per-model fraction in a sibling table the wallet can adopt
//   without changing the manifest schema. BFL is mostly compute (~0.95
//   compute, 0.05 asset) per the S6 design recommendation.

package bfl

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// SeedManifests returns the full set of BFL model manifests that ship with
// modelhub at S7. The catalog ingestion code (S4) calls this at process
// boot to populate / refresh the model_catalog rows.
//
// The list is intentionally small; new BFL models go through the manifest
// promotion process documented in plans/BLUEPRINT.md §4 (Sprint 3).
func SeedManifests() []catalog.ModelManifest {
	return []catalog.ModelManifest{
		manifestFluxPro11(),
		manifestFluxDev(),
		manifestFluxSchnellV15(),
		manifestFlux2Pro(),
	}
}

// AssetCostFractions returns the (compute, asset) split per model as a
// fraction of total cost the asset-hosting step represents.
//
// The wallet's PartialRefund path (per S6) reads this when an
// `OutputAvailable → AssetLost` transition occurs: the user gets back the
// asset fraction; the compute fraction stays charged because BFL already
// billed us. BFL is mostly compute (~0.95 / 0.05) because BFL serves images
// from temporary URLs we copy quickly.
//
// Returned fraction is in [0.0, 1.0]. Default for any unknown model is 0.5
// (caller responsibility to add an explicit entry for new models).
func AssetCostFractions() map[adapter.ModelKey]float32 {
	return map[adapter.ModelKey]float32{
		"flux-pro-1.1":      0.05,
		"flux-dev":          0.05,
		"flux-schnell-v1.5": 0.05,
		"flux-2-pro":        0.05,
	}
}

// fluxImageInputSchema is the JSON Schema (Draft 2020-12) shared by all
// Flux models. Per-model differences (e.g., flux-2-pro's wider aspect
// ratios) are layered via individual manifests if they ever diverge.
var fluxImageInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["prompt"],
  "additionalProperties": false,
  "properties": {
    "prompt": {
      "type": "string",
      "minLength": 1,
      "maxLength": 2000,
      "description": "Text prompt for image generation."
    },
    "aspect_ratio": {
      "type": "string",
      "enum": ["1:1", "16:9", "9:16", "4:3", "3:4", "21:9", "9:21"],
      "default": "1:1",
      "description": "Output image aspect ratio."
    },
    "seed": {
      "type": "integer",
      "minimum": 0,
      "maximum": 4294967295,
      "description": "Optional deterministic seed."
    },
    "num_images": {
      "type": "integer",
      "minimum": 1,
      "maximum": 8,
      "default": 1,
      "description": "Number of images to generate (1-8)."
    },
    "safety_tolerance": {
      "type": "integer",
      "minimum": 0,
      "maximum": 6,
      "default": 2,
      "description": "BFL safety tolerance (0=strict, 6=permissive)."
    }
  }
}`)

func manifestFluxPro11() catalog.ModelManifest {
	return catalog.ModelManifest{
		Key:           "flux-pro-1.1",
		Name:          "Flux 1.1 Pro",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindAsync,
		Provider:      providerKey,
		UpstreamModel: "flux-pro-1.1",
		InputSchema:   fluxImageInputSchema,
		// Pricing: docs.bfl.ai/pricing — $0.04 per image (per S7 §1).
		PriceFormula: "$0.04 per image (num_images * $0.04).",
		Tags:         []string{"image", "flux", "high-quality"},
		Examples: []catalog.Example{
			{
				Title:       "Hyper-realistic portrait",
				Description: "Single 1:1 portrait at default quality.",
				Params: adapter.Params{
					"prompt":       "Studio portrait of a woman in golden-hour light, ultra-realistic, 50mm lens",
					"aspect_ratio": "1:1",
				},
			},
			{
				Title:       "Wide landscape",
				Description: "21:9 cinematic landscape.",
				Params: adapter.Params{
					"prompt":       "Misty mountain valley at dawn, painterly, cinematic widescreen",
					"aspect_ratio": "21:9",
				},
			},
		},
		Enabled: true,
		Order:   100,
	}
}

func manifestFluxDev() catalog.ModelManifest {
	return catalog.ModelManifest{
		Key:           "flux-dev",
		Name:          "Flux Dev",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindAsync,
		Provider:      providerKey,
		UpstreamModel: "flux-dev",
		InputSchema:   fluxImageInputSchema,
		PriceFormula:  "$0.025 per image (num_images * $0.025).",
		// "api-only" tag enforces ADR-006: flux-dev's Non-Commercial License v2.0
		// forbids self-hosting weights for commercial use; the only legal path
		// is via api.bfl.ai, which IS commercial-use OK per FLUX API Service Terms.
		// Surface deployers MUST NOT route /v1/generations?model=flux-dev to a
		// self-hosted weight server, ever.
		Tags: []string{"image", "flux", "api-only"},
		Examples: []catalog.Example{
			{
				Title: "Standard test render",
				Params: adapter.Params{
					"prompt":       "A small red barn at the edge of a wheat field, soft afternoon light",
					"aspect_ratio": "4:3",
				},
			},
		},
		Enabled: true,
		Order:   110,
	}
}

func manifestFluxSchnellV15() catalog.ModelManifest {
	return catalog.ModelManifest{
		Key:           "flux-schnell-v1.5",
		Name:          "Flux Schnell v1.5",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindAsync,
		Provider:      providerKey,
		UpstreamModel: "flux-schnell",
		InputSchema:   fluxImageInputSchema,
		// Schnell is the cheapest Flux variant; pricing per docs.bfl.ai/pricing.
		PriceFormula: "$0.003 per image (num_images * $0.003).",
		Tags:         []string{"image", "flux", "fast", "cheap", "apache-2.0"},
		Examples: []catalog.Example{
			{
				Title:       "Quick concept sketch",
				Description: "Schnell trades fidelity for speed.",
				Params: adapter.Params{
					"prompt":       "Concept sketch of a futuristic transit hub, isometric",
					"aspect_ratio": "16:9",
				},
			},
		},
		Enabled: true,
		Order:   120,
	}
}

func manifestFlux2Pro() catalog.ModelManifest {
	return catalog.ModelManifest{
		Key:           "flux-2-pro",
		Name:          "Flux 2 Pro",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindAsync,
		Provider:      providerKey,
		UpstreamModel: "flux-2-pro",
		InputSchema:   fluxImageInputSchema,
		PriceFormula:  "$0.08 per image (num_images * $0.08).",
		Tags:          []string{"image", "flux", "high-quality", "newest"},
		Examples: []catalog.Example{
			{
				Title:       "Editorial cover",
				Description: "Flux 2 Pro with wider aspect ratio.",
				Params: adapter.Params{
					"prompt":       "Magazine cover photo of an architect inspecting blueprints, golden hour, shallow depth of field",
					"aspect_ratio": "9:16",
				},
			},
		},
		Enabled: true,
		Order:   90, // Highest priority — newest flagship.
	}
}
