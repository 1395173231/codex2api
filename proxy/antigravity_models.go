package proxy

import (
	"strings"

	"github.com/codex2api/auth"
)

type antigravityReasoningVariant struct {
	level          string
	wireModel      string
	thinkingBudget int
}

type antigravityPublicModelDefinition struct {
	id                    string
	wireModel             string
	reasoningLevels       []string
	defaultReasoningLevel string
	variants              []antigravityReasoningVariant
}

// antigravityPublicModelCatalog is the stable logical surface advertised to
// downstream clients. Gemini backing IDs and the former per-effort model names
// are transport details and compatibility aliases, never catalog entries.
var antigravityPublicModelCatalog = []antigravityPublicModelDefinition{
	{
		id:                    "gemini-3.5-flash",
		reasoningLevels:       []string{"low", "medium", "high"},
		defaultReasoningLevel: "medium",
		variants: []antigravityReasoningVariant{
			{level: "low", wireModel: "gemini-3.5-flash-extra-low", thinkingBudget: 1000},
			{level: "medium", wireModel: "gemini-3.5-flash-low", thinkingBudget: 4000},
			{level: "high", wireModel: "gemini-3-flash-agent", thinkingBudget: 10000},
		},
	},
	{
		id:                    "gemini-3.6-flash",
		reasoningLevels:       []string{"low", "medium", "high"},
		defaultReasoningLevel: "medium",
		variants: []antigravityReasoningVariant{
			{level: "low", wireModel: "gemini-3.6-flash-low", thinkingBudget: 4096},
			{level: "medium", wireModel: "gemini-3.6-flash-medium", thinkingBudget: 8192},
			{level: "high", wireModel: "gemini-3.6-flash-high", thinkingBudget: 24576},
		},
	},
	{
		id:                    "gemini-3.7-flash",
		reasoningLevels:       []string{"low", "medium", "high"},
		defaultReasoningLevel: "medium",
		variants: []antigravityReasoningVariant{
			{level: "low", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 4096},
			{level: "medium", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 8192},
			{level: "high", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 24576},
		},
	},
	{
		id:                    "gemini-3.1-pro",
		reasoningLevels:       []string{"low", "high"},
		defaultReasoningLevel: "high",
		variants: []antigravityReasoningVariant{
			{level: "low", wireModel: "gemini-3.1-pro-low", thinkingBudget: 1001},
			{level: "high", wireModel: "gemini-pro-agent", thinkingBudget: 10001},
		},
	},
	{id: "claude-opus-4-6-thinking", wireModel: "claude-opus-4-6-thinking"},
	{id: "claude-sonnet-4-6", wireModel: "claude-sonnet-4-6"},
	{id: "gpt-oss-120b-medium", wireModel: "gpt-oss-120b-medium"},
}

// antigravityCompatibilityAliases preserves the old fixed-tier model names.
// An alias always wins over a conflicting reasoning.effort in the request.
var antigravityCompatibilityAliases = map[string]antigravityReasoningVariant{
	"gemini-3.5-flash-low":    {level: "low", wireModel: "gemini-3.5-flash-extra-low", thinkingBudget: 1000},
	"gemini-3.5-flash-medium": {level: "medium", wireModel: "gemini-3.5-flash-low", thinkingBudget: 4000},
	"gemini-3.5-flash-high":   {level: "high", wireModel: "gemini-3-flash-agent", thinkingBudget: 10000},
	"gemini-3.6-flash-low":    {level: "low", wireModel: "gemini-3.6-flash-low", thinkingBudget: 4096},
	"gemini-3.6-flash-medium": {level: "medium", wireModel: "gemini-3.6-flash-medium", thinkingBudget: 8192},
	"gemini-3.6-flash-high":   {level: "high", wireModel: "gemini-3.6-flash-high", thinkingBudget: 24576},
	"gemini-3.7-flash-low":    {level: "low", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 4096},
	"gemini-3.7-flash-medium": {level: "medium", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 8192},
	"gemini-3.7-flash-high":   {level: "high", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 24576},
	"gemini-3.1-pro-low":      {level: "low", wireModel: "gemini-3.1-pro-low", thinkingBudget: 1001},
	"gemini-3.1-pro-high":     {level: "high", wireModel: "gemini-pro-agent", thinkingBudget: 10001},
}

func antigravityPublicModel(model string) (antigravityPublicModelDefinition, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return antigravityPublicModelDefinition{}, false
	}
	for _, definition := range antigravityPublicModelCatalog {
		if definition.id == name {
			return definition, true
		}
	}
	return antigravityPublicModelDefinition{}, false
}

func antigravityCompatibilityAlias(model string) (antigravityReasoningVariant, bool) {
	variant, ok := antigravityCompatibilityAliases[strings.ToLower(strings.TrimSpace(model))]
	return variant, ok
}

func antigravityPublicModelIDs() []string {
	models := make([]string, 0, len(antigravityPublicModelCatalog))
	for _, definition := range antigravityPublicModelCatalog {
		models = append(models, definition.id)
	}
	return models
}

func antigravityAcceptedModelIDs() []string {
	models := antigravityPublicModelIDs()
	aliases := []string{
		"gemini-3.5-flash-low", "gemini-3.5-flash-medium", "gemini-3.5-flash-high",
		"gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.6-flash-high",
		"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high",
		"gemini-3.1-pro-low", "gemini-3.1-pro-high",
	}
	return append(models, aliases...)
}

func antigravityPublicModelWireID(model string) (string, bool) {
	definition, ok := antigravityPublicModel(model)
	if !ok {
		if alias, aliasOK := antigravityCompatibilityAlias(model); aliasOK {
			return alias.wireModel, true
		}
		return "", false
	}
	if definition.wireModel != "" {
		return definition.wireModel, true
	}
	variant, ok := antigravityVariantForDefinition(definition, nil)
	return variant.wireModel, ok
}

// AntigravityWireModelID returns the default backing for a logical model and
// the fixed backing for a compatibility alias.
func AntigravityWireModelID(model string) (string, bool) {
	return antigravityPublicModelWireID(model)
}

// AntigravityWireModelIDs returns every physical backing required to serve all
// advertised reasoning levels of a logical model. Admin persistence uses this
// to avoid creating an account that advertises tiers it cannot route.
func AntigravityWireModelIDs(model string) []string {
	if alias, ok := antigravityCompatibilityAlias(model); ok {
		return []string{alias.wireModel}
	}
	definition, ok := antigravityPublicModel(model)
	if !ok {
		return nil
	}
	if definition.wireModel != "" {
		return []string{definition.wireModel}
	}
	seen := make(map[string]struct{}, len(definition.variants))
	wires := make([]string, 0, len(definition.variants))
	for _, variant := range definition.variants {
		if _, exists := seen[variant.wireModel]; exists {
			continue
		}
		seen[variant.wireModel] = struct{}{}
		wires = append(wires, variant.wireModel)
	}
	return wires
}

// AntigravityPublishedModelIDs projects a synchronized raw/wire catalog into
// logical downstream models. A logical model is published only when all of its
// distinct backing IDs are present, so every advertised reasoning level works.
func AntigravityPublishedModelIDs(rawModels []string) []string {
	available := make(map[string]struct{}, len(rawModels))
	for _, model := range rawModels {
		name := strings.ToLower(strings.TrimSpace(model))
		if name != "" {
			available[name] = struct{}{}
		}
	}
	models := make([]string, 0, len(antigravityPublicModelCatalog))
	for _, definition := range antigravityPublicModelCatalog {
		wires := AntigravityWireModelIDs(definition.id)
		if len(wires) == 0 {
			continue
		}
		complete := true
		for _, wire := range wires {
			if _, ok := available[wire]; !ok {
				complete = false
				break
			}
		}
		if complete {
			models = append(models, definition.id)
		}
	}
	return models
}

func antigravityPublicModelsForAccount(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	return AntigravityPublishedModelIDs(account.AntigravityModels())
}

func antigravityAccountSupportsPublicModel(account *auth.Account, model string) bool {
	_, ok := antigravityResolvePublicModelForAccount(account, model)
	return ok
}

// antigravityResolvePublicModelForAccount accepts both logical models and old
// aliases, while requiring the physical backing(s) needed by that contract.
func antigravityResolvePublicModelForAccount(account *auth.Account, model string) (string, bool) {
	if account == nil {
		return "", false
	}
	wires := AntigravityWireModelIDs(model)
	if len(wires) == 0 {
		return "", false
	}
	available := make(map[string]struct{}, len(account.AntigravityModels()))
	for _, item := range account.AntigravityModels() {
		available[strings.ToLower(strings.TrimSpace(item))] = struct{}{}
	}
	for _, wire := range wires {
		if _, ok := available[wire]; !ok {
			return "", false
		}
	}
	defaultWire, ok := antigravityPublicModelWireID(model)
	return defaultWire, ok
}

// antigravityResponsesTextModel is a transport compatibility guard, not an
// advertising source. Raw backing IDs remain callable internally.
func antigravityResponsesTextModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	return !strings.HasPrefix(name, "imagen") &&
		!strings.Contains(name, "-image") &&
		!strings.Contains(name, "image-")
}

func antigravityCodexReasoningLevels(model string) []string {
	definition, ok := antigravityPublicModel(model)
	if !ok || len(definition.reasoningLevels) == 0 {
		return nil
	}
	return append([]string(nil), definition.reasoningLevels...)
}

func antigravityVariantForDefinition(definition antigravityPublicModelDefinition, reasoning map[string]any) (antigravityReasoningVariant, bool) {
	if len(definition.variants) == 0 {
		return antigravityReasoningVariant{}, false
	}
	level := definition.defaultReasoningLevel
	if len(reasoning) > 0 {
		level = antigravityGeminiReasoningTier(reasoning)
	}
	for _, variant := range definition.variants {
		if variant.level == level {
			return variant, true
		}
	}
	// Pro exposes only low/high; medium and unknown values use its documented
	// default rather than inventing a third tier.
	for _, variant := range definition.variants {
		if variant.level == definition.defaultReasoningLevel {
			return variant, true
		}
	}
	return antigravityReasoningVariant{}, false
}

func antigravityResolvedVariant(model string, reasoning map[string]any) (antigravityReasoningVariant, bool) {
	if alias, ok := antigravityCompatibilityAlias(model); ok {
		return alias, true
	}
	definition, ok := antigravityPublicModel(model)
	if !ok {
		return antigravityReasoningVariant{}, false
	}
	return antigravityVariantForDefinition(definition, reasoning)
}
