package flow2api

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// UpstreamShape selects which upstream protocol the adapter speaks for a given model.
// Empty string = "openai_chat" (the default — Gemini/Imagen/Veo via /v1/chat/completions).
type UpstreamShape string

const (
	ShapeOpenAIChat   UpstreamShape = "openai_chat"   // /v1/chat/completions; response normalized via chatCompletion path
	ShapeOpenAIImages UpstreamShape = "openai_images" // /v1/images/generations; returns b64_json
)

type flowModelSpec struct {
	Key           adapter.ModelKey
	Name          string
	Modality      adapter.Modality
	TaskKind      adapter.TaskKind
	UpstreamModel string
	MinImages     int
	MaxImages     int
	Tags          []string
	Order         int

	// UpstreamShape controls submit path + response normalization. Empty = openai_chat.
	UpstreamShape UpstreamShape

	// AuthKeySelector picks which API key to send. Empty = "default" (FLOW2API_API_KEY).
	// "gpt_image" routes to FLOW2API_GPT_IMAGE_API_KEY.
	AuthKeySelector string

	// Sizes accepted by openai_images; first entry is the default.
	Sizes []string

	// MaxN cap for openai_images batch.
	MaxN int
}

// effectiveShape returns the resolved shape (defaults to openai_chat).
func (s flowModelSpec) effectiveShape() UpstreamShape {
	if s.UpstreamShape == "" {
		return ShapeOpenAIChat
	}
	return s.UpstreamShape
}

var flowModels = []flowModelSpec{
	{
		Key:           "gemini-3.1-flash-image-landscape",
		Name:          "Gemini 3.1 Flash Image Landscape via flow2api",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "gemini-3.1-flash-image-landscape",
		MaxImages:     8,
		Tags:          []string{"image", "flow2api", "gemini", "landscape"},
		Order:         300,
	},
	{
		Key:           "gemini-3.1-flash-image-landscape-2k",
		Name:          "Gemini 3.1 Flash Image Landscape 2K via flow2api",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "gemini-3.1-flash-image-landscape-2k",
		MaxImages:     8,
		Tags:          []string{"image", "flow2api", "gemini", "2k", "landscape"},
		Order:         301,
	},
	{
		Key:           "gemini-3.1-flash-image-landscape-4k",
		Name:          "Gemini 3.1 Flash Image Landscape 4K via flow2api",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "gemini-3.1-flash-image-landscape-4k",
		MaxImages:     8,
		Tags:          []string{"image", "flow2api", "gemini", "4k", "landscape"},
		Order:         302,
	},
	{
		Key:           "imagen-4.0-generate-preview-landscape",
		Name:          "Imagen 4 Preview Landscape via flow2api",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "imagen-4.0-generate-preview-landscape",
		Tags:          []string{"image", "flow2api", "imagen", "landscape"},
		Order:         303,
	},
	{
		Key:           "veo_3_1_t2v_lite_landscape",
		Name:          "Veo 3.1 T2V Lite Landscape via flow2api",
		Modality:      adapter.ModalityVideo,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "veo_3_1_t2v_lite_landscape",
		Tags:          []string{"video", "flow2api", "veo", "lite", "t2v"},
		Order:         320,
	},
	{
		Key:           "veo_3_1_t2v_fast_landscape",
		Name:          "Veo 3.1 T2V Fast Landscape via flow2api",
		Modality:      adapter.ModalityVideo,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "veo_3_1_t2v_fast_landscape",
		Tags:          []string{"video", "flow2api", "veo", "fast", "t2v"},
		Order:         321,
	},
	{
		Key:           "veo_3_1_i2v_s_fast_fl",
		Name:          "Veo 3.1 I2V Fast First/Last via flow2api",
		Modality:      adapter.ModalityVideo,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "veo_3_1_i2v_s_fast_fl",
		MinImages:     1,
		MaxImages:     2,
		Tags:          []string{"video", "flow2api", "veo", "fast", "i2v"},
		Order:         322,
	},
	{
		Key:           "veo_3_1_r2v_fast",
		Name:          "Veo 3.1 R2V Fast via flow2api",
		Modality:      adapter.ModalityVideo,
		TaskKind:      adapter.TaskKindSync,
		UpstreamModel: "veo_3_1_r2v_fast",
		MaxImages:     3,
		Tags:          []string{"video", "flow2api", "veo", "fast", "r2v"},
		Order:         323,
	},

	// ── OpenAI image generation via flow2api /v1/images/generations ─────────────
	// Uses a separate API key (FLOW2API_GPT_IMAGE_API_KEY) bound to a Codex-tier
	// account at the same upstream. Response is base64 PNG (not URL).
	{
		Key:             "gpt-image-2",
		Name:            "GPT Image 2 via flow2api",
		Modality:        adapter.ModalityImage,
		TaskKind:        adapter.TaskKindSync,
		UpstreamModel:   "gpt-image-2",
		Tags:            []string{"image", "flow2api", "openai", "gpt-image"},
		Order:           400,
		UpstreamShape:   ShapeOpenAIImages,
		AuthKeySelector: "gpt_image",
		Sizes:           []string{"1024x1024", "1024x1792", "1792x1024"},
		MaxN:            4,
	},
	{
		Key:             "gpt-image-1.5",
		Name:            "GPT Image 1.5 via flow2api",
		Modality:        adapter.ModalityImage,
		TaskKind:        adapter.TaskKindSync,
		UpstreamModel:   "gpt-image-1.5",
		Tags:            []string{"image", "flow2api", "openai", "gpt-image"},
		Order:           401,
		UpstreamShape:   ShapeOpenAIImages,
		AuthKeySelector: "gpt_image",
		Sizes:           []string{"1024x1024", "1024x1792", "1792x1024"},
		MaxN:            4,
	},
	{
		Key:             "gpt-image-1",
		Name:            "GPT Image 1 via flow2api",
		Modality:        adapter.ModalityImage,
		TaskKind:        adapter.TaskKindSync,
		UpstreamModel:   "gpt-image-1",
		Tags:            []string{"image", "flow2api", "openai", "gpt-image"},
		Order:           402,
		UpstreamShape:   ShapeOpenAIImages,
		AuthKeySelector: "gpt_image",
		Sizes:           []string{"1024x1024", "1024x1792", "1792x1024"},
		MaxN:            4,
	},
}

var modelSpecs = buildModelSpecMap(flowModels)

func buildModelSpecMap(in []flowModelSpec) map[adapter.ModelKey]flowModelSpec {
	out := make(map[adapter.ModelKey]flowModelSpec, len(in))
	for _, spec := range in {
		out[spec.Key] = spec
	}
	return out
}

func SeedManifests() []catalog.ModelManifest {
	out := make([]catalog.ModelManifest, 0, len(flowModels))
	for _, spec := range flowModels {
		out = append(out, catalog.ModelManifest{
			Key:           spec.Key,
			Name:          spec.Name,
			Modality:      spec.Modality,
			TaskKind:      spec.TaskKind,
			Provider:      ProviderKeyFlow2API,
			UpstreamModel: spec.UpstreamModel,
			InputSchema:   inputSchema(spec),
			PriceFormula:  priceFormula(spec),
			Tags:          spec.Tags,
			Enabled:       true,
			Order:         spec.Order,
			Examples:      examplesFor(spec),
		})
	}
	return out
}

func inputSchema(spec flowModelSpec) json.RawMessage {
	required := []string{"prompt"}
	properties := map[string]any{
		"prompt": map[string]any{
			"type":        "string",
			"minLength":   1,
			"maxLength":   32000,
			"description": "Text prompt for the generation.",
		},
	}
	if spec.MaxImages > 0 {
		properties["image_urls"] = map[string]any{
			"type":        "array",
			"minItems":    spec.MinImages,
			"maxItems":    spec.MaxImages,
			"description": "Reference image URLs or data URLs forwarded to flow2api.",
			"items": map[string]any{
				"type":      "string",
				"minLength": 1,
			},
		}
		if spec.MinImages > 0 {
			required = append(required, "image_urls")
		}
	}
	if spec.effectiveShape() == ShapeOpenAIImages {
		if len(spec.Sizes) > 0 {
			properties["size"] = map[string]any{
				"type":        "string",
				"enum":        spec.Sizes,
				"description": "Output dimensions, WxH. Defaults to " + spec.Sizes[0] + ".",
			}
		}
		if spec.MaxN > 0 {
			properties["n"] = map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     spec.MaxN,
				"description": "Number of images to generate. Defaults to 1.",
			}
		}
	}
	schema := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"required":             required,
		"properties":           properties,
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		panic("flow2api: failed to marshal input schema: " + err.Error())
	}
	return raw
}

func priceFormula(spec flowModelSpec) string {
	if spec.effectiveShape() == ShapeOpenAIImages {
		switch spec.Key {
		case "gpt-image-2":
			return "Estimated $0.10 per image via flow2api (gpt-image-2)."
		case "gpt-image-1.5":
			return "Estimated $0.06 per image via flow2api (gpt-image-1.5)."
		case "gpt-image-1":
			return "Estimated $0.04 per image via flow2api (gpt-image-1)."
		}
	}
	if spec.Modality == adapter.ModalityImage {
		switch {
		case spec.Key == "gemini-3.1-flash-image-landscape-4k":
			return "Estimated $0.15 per image via flow2api."
		case spec.Key == "gemini-3.1-flash-image-landscape-2k":
			return "Estimated $0.08 per image via flow2api."
		default:
			return "Estimated $0.04 per image via flow2api."
		}
	}
	return "Estimated fixed cost per video generation via flow2api; exact upstream billing is reconciled by the wallet layer when available."
}

func examplesFor(spec flowModelSpec) []catalog.Example {
	if spec.Modality == adapter.ModalityVideo {
		params := adapter.Params{
			"prompt": "A cinematic landscape shot of morning fog rolling through a pine valley, slow camera push.",
		}
		if spec.MinImages > 0 {
			params["image_urls"] = []string{"https://cdn.modelhub.local/uploads/upload_example"}
		}
		return []catalog.Example{{Title: "Cinematic landscape", Params: params}}
	}
	return []catalog.Example{
		{
			Title: "Editorial image",
			Params: adapter.Params{
				"prompt": "Editorial product photo of a matte black smart speaker on a marble table, soft studio lighting.",
			},
		},
	}
}
