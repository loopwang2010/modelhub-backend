// manifest_seed.go — ModelManifest seeds for the OpenAI image-edit
// catalogue. Loaded by the catalog service (S4) at boot when the
// adapter is registered.
//
// AssetCostFraction note: per S6 wallet design, the per-model
// (compute / asset) cost split lets Settle refund only the asset
// portion when an asset-host failure marks a task AssetLost. For
// gpt-image-1 we set 0.6 compute / 0.4 asset (lower asset fraction
// than typical async models because the source image was ALSO an
// input cost — refunding the asset portion alone would over-credit
// the user). This split is documented inline; see manifests below.
//
// Tags include "image-edit", "sync", and "multipart-via-upload" so
// the frontend filter UI can group these models intuitively.

package openai

import (
	"encoding/json"
	"errors"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// AssetCostFraction is the (compute, asset) split for OpenAI image-edit
// outputs. gpt-image-1 returns base64 — the "asset cost" is our S9.5
// CDN upload + storage, much smaller than the upstream compute cost.
//
// Wallet uses these to compute partial refunds:
//   - Settle reconciles total cost via response.usage.total_tokens
//   - On AssetLost, only refund the asset fraction (asset failed → user
//     didn't get the image, but compute did happen)
//   - On content_policy: refund compute fraction (no compute consumed)
type AssetCostFraction struct {
	Compute float64
	Asset   float64
}

// gptImageAssetCost is the canonical split for OpenAI image models.
// 0.6 compute means: of the total billed cost, 60% covers OpenAI's
// upstream compute, 40% covers our CDN/storage costs.
//
// Rationale for the relatively low asset fraction (0.4): unlike async
// providers where the upstream URL TTL is short and we own the entire
// asset lifecycle, OpenAI returns base64 inline. Our asset cost is
// just one S3 PUT + lifetime storage, not a download + retry budget.
var gptImageAssetCost = AssetCostFraction{Compute: 0.6, Asset: 0.4}

// SeedManifests returns the curated list of ModelManifest entries this
// adapter exposes to /v1/models. Catalog service (S4) calls this at boot.
//
// Each manifest is independently Validate()'d at construction; we
// fail-fast here because a malformed seed is a programmer error.
func SeedManifests() ([]catalog.ModelManifest, error) {
	manifests := []catalog.ModelManifest{
		gptImage1Manifest(),
		gptImage1MiniManifest(),
		gptImage15Manifest(),
	}
	for i := range manifests {
		if err := manifests[i].Validate(); err != nil {
			return nil, errors.New("manifest validation failed for " + string(manifests[i].Key) + ": " + err.Error())
		}
	}
	return manifests, nil
}

// gptImage1Manifest is the canonical gpt-image-1 entry: 1024² baseline,
// $0.04/output, base64 inline response.
func gptImage1Manifest() catalog.ModelManifest {
	return catalog.ModelManifest{
		Key:           "gpt-image-1",
		Name:          "GPT Image 1",
		Modality:      adapter.ModalityEdit,
		TaskKind:      adapter.TaskKindSync,
		Provider:      ProviderKeyOpenAI,
		UpstreamModel: "gpt-image-1",
		InputSchema:   gptImageInputSchema("gpt-image-1"),
		PriceFormula:  "Token-based: ~$0.04 per 1024² standard quality output. Reconciled at Settle from response.usage.total_tokens. Estimate over-provisions by 1.5×.",
		Tags:          []string{"image-edit", "sync", "multipart-via-upload", "openai"},
		Enabled:       true,
		Order:         100,
		Examples: []catalog.Example{
			{
				Title:       "Cyberpunk reskin",
				Description: "Re-style an existing image as cyberpunk neon.",
				Params: adapter.Params{
					"prompt":   "Transform this scene with cyberpunk neon lighting and rain.",
					"image_id": "upload_<paste-after-/v1/uploads>",
					"n":        1,
					"size":     "1024x1024",
				},
			},
		},
	}
}

// gptImage1MiniManifest is the cheaper mini variant.
func gptImage1MiniManifest() catalog.ModelManifest {
	return catalog.ModelManifest{
		Key:           "gpt-image-1-mini",
		Name:          "GPT Image 1 Mini",
		Modality:      adapter.ModalityEdit,
		TaskKind:      adapter.TaskKindSync,
		Provider:      ProviderKeyOpenAI,
		UpstreamModel: "gpt-image-1-mini",
		InputSchema:   gptImageInputSchema("gpt-image-1-mini"),
		PriceFormula:  "Token-based: ~$0.015 per 1024² standard quality output. Faster + cheaper than gpt-image-1, slightly lower fidelity.",
		Tags:          []string{"image-edit", "sync", "multipart-via-upload", "openai", "fast", "cheap"},
		Enabled:       true,
		Order:         110,
	}
}

// gptImage15Manifest is the premium 1.5 variant. May support URL-form
// response per OpenAI roadmap (per AP-9, normalize handles both).
func gptImage15Manifest() catalog.ModelManifest {
	schema := gptImageInputSchema("gpt-image-1.5")
	// 1.5 supports response_format:url additionally; the schema's enum
	// for that field already includes both options below.
	return catalog.ModelManifest{
		Key:           "gpt-image-1.5",
		Name:          "GPT Image 1.5",
		Modality:      adapter.ModalityEdit,
		TaskKind:      adapter.TaskKindSync,
		Provider:      ProviderKeyOpenAI,
		UpstreamModel: "gpt-image-1.5",
		InputSchema:   schema,
		PriceFormula:  "Token-based: ~$0.08 per 1024² standard quality output. Premium fidelity tier with mask support.",
		Tags:          []string{"image-edit", "sync", "multipart-via-upload", "openai", "premium"},
		Enabled:       true,
		Order:         90,
	}
}

// gptImageInputSchema returns the JSON Schema for an image-edit
// invocation. Image is referenced by upload_id (per ADR-009), prompt
// is required. Optional sizing / quality / format. The schema is the
// authoritative contract enforced at /v1/generations BEFORE Submit.
func gptImageInputSchema(_ adapter.ModelKey) json.RawMessage {
	schema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"required": []string{
			"prompt",
			"image_id",
		},
		"additionalProperties": false,
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   32_000,
				"description": "Edit instruction. Required.",
			},
			"image_id": map[string]any{
				"type":        "string",
				"pattern":     "^upload_[a-f0-9]{32}$",
				"description": "Reference to a previously POSTed /v1/uploads result. Required.",
			},
			"n": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     10,
				"default":     1,
				"description": "Number of output images.",
			},
			"size": map[string]any{
				"type": "string",
				"enum": []string{
					"auto",
					"256x256",
					"512x512",
					"1024x1024",
					"1024x1536",
					"1536x1024",
					"1536x1536",
					"2048x2048",
				},
				"default":     "1024x1024",
				"description": "Output dimensions.",
			},
			"quality": map[string]any{
				"type": "string",
				"enum": []string{
					"auto",
					"low",
					"medium",
					"high",
					"standard",
					"hd",
				},
				"default":     "auto",
				"description": "Output quality tier.",
			},
			"response_format": map[string]any{
				"type": "string",
				"enum": []string{
					"b64_json",
					"url",
				},
				"description": "Upstream response shape. Defaults to b64_json (gpt-image-1). gpt-image-1.5 also supports url.",
			},
			"user": map[string]any{
				"type":        "string",
				"maxLength":   256,
				"description": "End-user identifier — forwarded per OpenAI's abuse-monitoring guidance.",
			},
		},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		// json.Marshal of a map[string]any constructed in code cannot
		// realistically fail; if it does, panic — programmer error.
		panic("openai: failed to marshal input schema: " + err.Error())
	}
	return raw
}
