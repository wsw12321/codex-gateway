package config

import (
	"sort"
	"strings"
)

// ModelFamilyTokenPrefix marks a billing model-scope entry that names a whole
// model family instead of one exact model. Pricing rejects model names with
// this prefix, so a token never collides with a real model ID.
const ModelFamilyTokenPrefix = "family:"

const (
	ModelFamilyGPT    = "gpt"
	ModelFamilyGemini = "gemini"
	ModelFamilyClaude = "claude"
)

// ModelFamily describes one family that a billing scope can name as a whole.
// A scope holding Token covers every model whose ModelFamilyOf is ID,
// including models priced after the scope was saved.
type ModelFamily struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Group string `json:"group"`
	Label string `json:"label"`
}

// ModelFamilies returns the families in the dashboard's brand order. The
// dashboard repeats the tokens and labels; a test keeps the two in step.
func ModelFamilies() []ModelFamily {
	return []ModelFamily{
		{ID: ModelFamilyGPT, Token: ModelFamilyTokenPrefix + ModelFamilyGPT, Group: "GPT", Label: "GPT 全系"},
		{ID: ModelFamilyGemini, Token: ModelFamilyTokenPrefix + ModelFamilyGemini, Group: "Gemini", Label: "Gemini 全系"},
		{ID: ModelFamilyClaude, Token: ModelFamilyTokenPrefix + ModelFamilyClaude, Group: "Claude", Label: "Claude 全系"},
	}
}

// ModelFamilyOf classifies a model the same way the Gateway routes it:
// claude-* goes to the Anthropic provider, the reviewed Antigravity (gemini-*)
// models to Antigravity, and everything else to the Codex provider. The
// internal governance model belongs to no family, so no scope covers it
// implicitly.
func ModelFamilyOf(model string) string {
	switch {
	case model == "" || model == InternalGovernanceModel || strings.HasPrefix(model, ModelFamilyTokenPrefix):
		return ""
	case strings.HasPrefix(model, "claude-"):
		return ModelFamilyClaude
	case IsAntigravityModel(model) || strings.HasPrefix(model, "gemini-"):
		return ModelFamilyGemini
	default:
		return ModelFamilyGPT
	}
}

// ModelFamilyGroup returns the dashboard brand group for a model.
func ModelFamilyGroup(model string) string {
	family := ModelFamilyOf(model)
	for _, candidate := range ModelFamilies() {
		if candidate.ID == family {
			return candidate.Group
		}
	}
	return ""
}

// ModelFamilyToken returns the scope token for a model's family, or "" when
// the model belongs to no family.
func ModelFamilyToken(model string) string {
	family := ModelFamilyOf(model)
	if family == "" {
		return ""
	}
	return ModelFamilyTokenPrefix + family
}

// IsModelFamilyToken reports whether value is one of the known family tokens.
// An unknown family:* value is neither a token nor a valid model.
func IsModelFamilyToken(value string) bool {
	for _, family := range ModelFamilies() {
		if value == family.Token {
			return true
		}
	}
	return false
}

// ModelScopeKeys returns the scope entries that admit model: the exact model
// ID and, when it has one, its family token. A value that is itself a family
// token is not a model and is admitted by nothing.
func ModelScopeKeys(model string) []string {
	if model == "" || strings.HasPrefix(model, ModelFamilyTokenPrefix) {
		return []string{}
	}
	keys := []string{model}
	if token := ModelFamilyToken(model); token != "" {
		keys = append(keys, token)
	}
	return keys
}

// ModelScopeAllows reports whether a saved scope covers model, either by
// naming it exactly or by naming its family.
func ModelScopeAllows(scope []string, model string) bool {
	if model == "" || strings.HasPrefix(model, ModelFamilyTokenPrefix) {
		return false
	}
	token := ModelFamilyToken(model)
	for _, entry := range scope {
		if entry == model || (token != "" && entry == token) {
			return true
		}
	}
	return false
}

// ModelFamilyScope returns the scope that covers every model in catalog and
// keeps covering its families as models are added: one token per family
// present, plus any model that belongs to no family. The result is sorted like
// every stored scope.
func ModelFamilyScope(catalog []string) []string {
	present := make(map[string]bool)
	var exact []string
	for _, model := range catalog {
		if family := ModelFamilyOf(model); family != "" {
			present[family] = true
		} else if model != "" && model != InternalGovernanceModel {
			exact = append(exact, model)
		}
	}
	scope := make([]string, 0, len(present)+len(exact))
	for _, family := range ModelFamilies() {
		if present[family.ID] {
			scope = append(scope, family.Token)
		}
	}
	scope = append(scope, exact...)
	sort.Strings(scope)
	return scope
}
