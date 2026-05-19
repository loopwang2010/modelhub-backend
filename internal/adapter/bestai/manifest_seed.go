package bestai

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// UpstreamShape selects which OpenAI-compatible sub2api endpoint is used.
type UpstreamShape string

const (
	// ShapeOpenAIChat is used by bestai/sub2api flow2api groups.
	ShapeOpenAIChat UpstreamShape = "openai_chat"
	// ShapeOpenAIImages is used by bestai/sub2api OpenAI groups.
	ShapeOpenAIImages UpstreamShape = "openai_images"
)

type modelSpec struct {
	Key            adapter.ModelKey
	Name           string
	Modality       adapter.Modality
	TaskKind       adapter.TaskKind
	UpstreamModel  string
	MinImages      int
	MaxImages      int
	Tags           []string
	Order          int
	DefaultVisible bool
	LongRunning    bool

	// UpstreamShape controls submit path and response normalization.
	UpstreamShape UpstreamShape

	// AuthKeySelector selects the bestai/sub2api group API key.
	AuthKeySelector string

	// Sizes accepted by openai_images; first entry is the default.
	Sizes []string

	// MaxN caps OpenAI Images batch size.
	MaxN int
}

func (s modelSpec) effectiveShape() UpstreamShape {
	if s.UpstreamShape == "" {
		return ShapeOpenAIChat
	}
	return s.UpstreamShape
}

func (s modelSpec) effectiveAuthSelector() string {
	if s.AuthKeySelector == "" {
		return authKeySelectorFlow2API
	}
	return s.AuthKeySelector
}

var bestAIModels = buildBestAIModels()

var bestAIOpenAIModels = []modelSpec{
	{
		Key:             "gpt-image-2",
		Name:            "GPT Image 2 via bestai/OpenAI",
		Modality:        adapter.ModalityImage,
		TaskKind:        adapter.TaskKindSync,
		UpstreamModel:   "gpt-image-2",
		Tags:            []string{"image", "bestai", "sub2api", "openai", "gpt-image"},
		Order:           400,
		UpstreamShape:   ShapeOpenAIImages,
		AuthKeySelector: authKeySelectorOpenAI,
		Sizes:           []string{"1024x1024", "1024x1792", "1792x1024"},
		MaxN:            4,
	},
	{
		Key:             "gpt-image-1.5",
		Name:            "GPT Image 1.5 via bestai/OpenAI",
		Modality:        adapter.ModalityImage,
		TaskKind:        adapter.TaskKindSync,
		UpstreamModel:   "gpt-image-1.5",
		Tags:            []string{"image", "bestai", "sub2api", "openai", "gpt-image"},
		Order:           401,
		UpstreamShape:   ShapeOpenAIImages,
		AuthKeySelector: authKeySelectorOpenAI,
		Sizes:           []string{"1024x1024", "1024x1792", "1792x1024"},
		MaxN:            4,
	},
	{
		Key:             "gpt-image-1",
		Name:            "GPT Image 1 via bestai/OpenAI",
		Modality:        adapter.ModalityImage,
		TaskKind:        adapter.TaskKindSync,
		UpstreamModel:   "gpt-image-1",
		Tags:            []string{"image", "bestai", "sub2api", "openai", "gpt-image"},
		Order:           402,
		UpstreamShape:   ShapeOpenAIImages,
		AuthKeySelector: authKeySelectorOpenAI,
		Sizes:           []string{"1024x1024", "1024x1792", "1792x1024"},
		MaxN:            4,
	},
}

func buildBestAIModels() []modelSpec {
	out := make([]modelSpec, 0, len(generatedFlow2APIModels)+len(bestAIOpenAIModels))
	out = append(out, generatedFlow2APIModels...)
	out = append(out, bestAIOpenAIModels...)
	return out
}

var modelSpecs = buildModelSpecMap(bestAIModels)

func buildModelSpecMap(in []modelSpec) map[adapter.ModelKey]modelSpec {
	out := make(map[adapter.ModelKey]modelSpec, len(in))
	for _, spec := range in {
		if _, exists := out[spec.Key]; exists {
			panic("bestai: duplicate model spec: " + string(spec.Key))
		}
		out[spec.Key] = spec
	}
	return out
}

func SeedManifests() []catalog.ModelManifest {
	out := make([]catalog.ModelManifest, 0, len(bestAIModels))
	for _, spec := range bestAIModels {
		out = append(out, catalog.ModelManifest{
			Key:           spec.Key,
			Name:          spec.Name,
			Modality:      spec.Modality,
			TaskKind:      spec.TaskKind,
			Provider:      ProviderKeyBestAI,
			UpstreamModel: spec.UpstreamModel,
			InputSchema:   inputSchema(spec),
			PriceFormula:  priceFormula(spec),
			Tags:          spec.Tags,
			Enabled:       catalogEnabled(spec),
			Order:         spec.Order,
			Examples:      examplesFor(spec),
		})
	}
	return out
}

const (
	envExposeFlow2APIAdvanced = "MODELHUB_FLOW2API_EXPOSE_ADVANCED"
	envExposeFlow2APIUpsample = "MODELHUB_FLOW2API_EXPOSE_UPSAMPLE"
)

func catalogEnabled(spec modelSpec) bool {
	if spec.effectiveAuthSelector() != authKeySelectorFlow2API {
		return true
	}
	if spec.LongRunning {
		return envBool(envExposeFlow2APIUpsample, false)
	}
	if !spec.DefaultVisible {
		return envBool(envExposeFlow2APIAdvanced, true)
	}
	return true
}

func envBool(name string, def bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

func flow2APISpecKeys() map[adapter.ModelKey]struct{} {
	out := make(map[adapter.ModelKey]struct{}, len(generatedFlow2APIModels))
	for _, spec := range generatedFlow2APIModels {
		out[spec.Key] = struct{}{}
	}
	return out
}

func inputSchema(spec modelSpec) json.RawMessage {
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
			"description": "Reference image URLs or data URLs forwarded to bestai/sub2api.",
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
				"description": "Optional output dimensions, WxH.",
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
		panic("bestai: failed to marshal input schema: " + err.Error())
	}
	return raw
}

func priceFormula(spec modelSpec) string {
	if spec.effectiveShape() == ShapeOpenAIImages {
		switch spec.Key {
		case "gpt-image-2":
			return "Estimated $0.10 per image via bestai/sub2api OpenAI."
		case "gpt-image-1.5":
			return "Estimated $0.06 per image via bestai/sub2api OpenAI."
		case "gpt-image-1":
			return "Estimated $0.04 per image via bestai/sub2api OpenAI."
		}
	}
	if spec.Modality == adapter.ModalityImage {
		switch {
		case spec.Key == "gemini-3.1-flash-image-landscape-4k":
			return "Estimated $0.15 per image via bestai/sub2api flow2api."
		case spec.Key == "gemini-3.1-flash-image-landscape-2k":
			return "Estimated $0.08 per image via bestai/sub2api flow2api."
		default:
			return "Estimated $0.04 per image via bestai/sub2api flow2api."
		}
	}
	return "Estimated fixed cost per video generation via bestai/sub2api flow2api; exact upstream billing is reconciled by the wallet layer when available."
}

func examplesFor(spec modelSpec) []catalog.Example {
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
