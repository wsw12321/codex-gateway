package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestModelFamilyOfFollowsProviderRouting(t *testing.T) {
	for model, want := range map[string]string{
		"gpt-6-sol":               ModelFamilyGPT,
		"gpt-5.4-mini":            ModelFamilyGPT,
		"o9-preview":              ModelFamilyGPT, // routed to the Codex provider
		"claude-opus-5-5":         ModelFamilyClaude,
		"gemini-4-argon-high":     ModelFamilyGemini,
		"gemini-9-future":         ModelFamilyGemini,
		InternalGovernanceModel:   "",
		"":                        "",
		"family:gpt":              "",
		"family:claude-opus-5-5x": "",
	} {
		if got := ModelFamilyOf(model); got != want {
			t.Errorf("ModelFamilyOf(%q) = %q, want %q", model, got, want)
		}
	}
	for _, model := range AntigravityModels() {
		if got := ModelFamilyOf(model); got != ModelFamilyGemini {
			t.Errorf("reviewed Antigravity model %q is in family %q", model, got)
		}
	}
}

func TestModelScopeAllowsExactModelsAndFamilies(t *testing.T) {
	scope := []string{"claude-opus-5-5", "family:gpt"}
	for model, want := range map[string]bool{
		"gpt-6-sol":               true,
		"gpt-7-priced-later":      true,
		"claude-opus-5-5":         true,
		"claude-sonnet-5-5":       false,
		"gemini-4-argon-high":     false,
		InternalGovernanceModel:   false,
		"":                        false,
		"family:gpt":              false,
		"family:claude-opus-5-5x": false,
	} {
		if got := ModelScopeAllows(scope, model); got != want {
			t.Errorf("ModelScopeAllows(%q) = %v, want %v", model, got, want)
		}
	}
	if got, want := ModelScopeKeys("claude-haiku-5-5"), []string{"claude-haiku-5-5", "family:claude"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelScopeKeys = %#v, want %#v", got, want)
	}
	if got, want := ModelScopeKeys(InternalGovernanceModel), []string{InternalGovernanceModel}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelScopeKeys(internal) = %#v, want %#v", got, want)
	}
	if got := ModelScopeKeys("family:gpt"); len(got) != 0 {
		t.Fatalf("ModelScopeKeys(token) = %#v, want none", got)
	}
}

func TestModelFamilyTokensAreClosed(t *testing.T) {
	for _, family := range ModelFamilies() {
		if !IsModelFamilyToken(family.Token) || family.Token != ModelFamilyTokenPrefix+family.ID || family.Label != family.Group+" 全系" {
			t.Fatalf("inconsistent family %#v", family)
		}
	}
	for _, value := range []string{"family:", "family:xai", "family:GPT", "gpt", "gpt-6-sol"} {
		if IsModelFamilyToken(value) {
			t.Fatalf("%q accepted as a family token", value)
		}
	}
}

func TestModelFamilyScopeCoversCatalogFamilies(t *testing.T) {
	got := ModelFamilyScope([]string{"gpt-6-sol", "claude-opus-5-5", "gpt-5.4", InternalGovernanceModel})
	if want := []string{"family:claude", "family:gpt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelFamilyScope = %#v, want %#v", got, want)
	}
	if got := ModelFamilyScope(nil); len(got) != 0 {
		t.Fatalf("empty catalog scope = %#v", got)
	}
}

func TestPricingRejectsFamilyTokenModelNames(t *testing.T) {
	raw := strings.Replace(validPricingV2ForFamilyTest, `"gpt-test"`, `"family:gpt"`, 1)
	if _, err := ParseUsagePricing(raw); err == nil || !strings.Contains(err.Error(), "model family") {
		t.Fatalf("ParseUsagePricing accepted a family token model name: %v", err)
	}
	if _, err := ParseUsagePricing(validPricingV2ForFamilyTest); err != nil {
		t.Fatalf("baseline pricing rejected: %v", err)
	}
}

const validPricingV2ForFamilyTest = `{"schema_version":2,"catalog_as_of":"2026-08-20","fx_as_of":"2026-08-20","usd_cny_rate":"7.2","fallback_policy":{"unknown_service_tier":"max_published","missing_price_combination":"max_published","missing_cache_write_tokens":"all_uncached_as_write"},"models":{"gpt-test":{"cache_write_mode":"included_in_input","max_input_tokens":272000,"long_context_threshold_tokens":272000,"service_tiers":{"standard":{"short":{"input_usd_per_million":"1","cached_input_usd_per_million":"0.1","output_usd_per_million":"2"}}}}}}`
