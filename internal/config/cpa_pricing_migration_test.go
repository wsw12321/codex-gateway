package config

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestCPAV8PricingMigrationPreservesUnrelatedRulesAndFX(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	before, err := ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	before.CatalogAsOf, before.FXAsOf, before.USDCNYRate = "2026-09-01", "2026-09-02", "6.987654"
	before.Models["private-codex-model"] = before.Models["gpt-5.4"]
	before.Models["gemini-3.1-pro-high"] = before.Models["gemini-pro-agent"]
	delete(before.Models, "gemini-pro-agent")
	delete(before.Models, "gpt-6.1-sol")
	beforeRaw, _ := json.Marshal(before)
	upgradedRaw, err := MigrateCPAV8Pricing(beforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := ParseUsagePricing(string(upgradedRaw))
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.FXAsOf != before.FXAsOf || upgraded.USDCNYRate != before.USDCNYRate || upgraded.FallbackPolicy != before.FallbackPolicy {
		t.Fatal("migration changed exchange rate or fallback settings")
	}
	for model, rule := range before.Models {
		if model == "gemini-3.1-pro-high" {
			continue
		}
		if !reflect.DeepEqual(rule, upgraded.Models[model]) {
			t.Fatalf("unrelated pricing changed: %s", model)
		}
	}
	if _, ok := upgraded.Models["gemini-3.1-pro-high"]; ok {
		t.Fatal("retired model still in catalog")
	}
	for model, rule := range CPAV8PricingModels() {
		if !reflect.DeepEqual(rule, upgraded.Models[model]) {
			t.Fatalf("missing reviewed model %s", model)
		}
	}
	// A later owner edit to a target model and current FX must survive rollback.
	changed := upgraded.Models["gpt-6.1-sol"]
	changed.ServiceTiers[PricingTierStandard].Short.InputUSDPerMillion = "9.75"
	upgraded.Models["gpt-6.1-sol"] = changed
	upgraded.USDCNYRate = "7.01"
	currentRaw, _ := json.Marshal(upgraded)
	restoredRaw, err := RestoreCPAV8Pricing(currentRaw, beforeRaw, upgradedRaw)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ParseUsagePricing(string(restoredRaw))
	if err != nil {
		t.Fatal(err)
	}
	if restored.USDCNYRate != "7.01" || !reflect.DeepEqual(restored.Models["gpt-6.1-sol"], changed) {
		t.Fatal("rollback overwrote a post-cutover change")
	}
	if _, ok := restored.Models["gemini-pro-agent"]; ok {
		t.Fatal("rollback retained unchanged new model")
	}
	if !reflect.DeepEqual(restored.Models["gemini-3.1-pro-high"], before.Models["gemini-3.1-pro-high"]) ||
		!reflect.DeepEqual(restored.Models["private-codex-model"], before.Models["private-codex-model"]) {
		t.Fatal("rollback lost original model rule")
	}
	if _, err := MigrateCPAV8Pricing([]byte(validUsagePricingJSON)); err == nil {
		t.Fatal("legacy pricing was implicitly repriced")
	}
}

func TestCPAV8ReviewedPricingBoundaries(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for model, rule := range CPAV8PricingModels() {
		if !reflect.DeepEqual(rule, pricing.Models[model]) {
			t.Fatalf("deployment differs from reviewed migration for %s", model)
		}
	}
	tests := []struct {
		model, tier string
		input       int64
		want        [4]string
	}{
		{"gemini-pro-agent", "standard", 200_000, [4]string{"2", "0.20", "0", "12"}},
		{"gemini-pro-agent", "standard", 200_001, [4]string{"4", "0.40", "0", "18"}},
		{"gemini-3.1-pro-low", "standard", 200_000, [4]string{"2", "0.20", "0", "12"}},
		{"gemini-3.1-pro-low", "standard", 200_001, [4]string{"4", "0.40", "0", "18"}},
		{"gemini-3-flash", "standard", 200_001, [4]string{"0.50", "0.05", "0", "3"}},
		{"gemini-3.1-flash-lite", "standard", 200_001, [4]string{"0.25", "0.025", "0", "1.50"}},
		{"gemini-3.5-flash-lite", "standard", 200_001, [4]string{"0.30", "0.03", "0", "2.50"}},
		{"gpt-6.1-sol", "standard", 272_000, [4]string{"2", "0.10", "2.50", "10"}},
		{"gpt-6.1-sol", "standard", 272_001, [4]string{"4", "0.20", "5", "15"}},
		{"gpt-6.1-sol", "flex", 272_000, [4]string{"1", "0.05", "1.25", "5"}},
		{"gpt-6.1-sol", "flex", 272_001, [4]string{"2", "0.10", "2.5", "7.5"}},
		{"gpt-6.1-sol", "priority", 272_000, [4]string{"4", "0.20", "5", "20"}},
		{"gpt-6.1-sol", "priority", 272_001, [4]string{"8", "0.40", "10", "30"}},
	}
	for _, tc := range tests {
		snapshotRaw, _, _, _ := pricing.ModelSnapshot(tc.model)
		snapshot, err := ParsePricingSnapshot(snapshotRaw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := snapshot.Select(tc.tier, tc.input)
		if err != nil || got.FallbackReason != "" || [4]string{got.InputUSDPerMillion, got.CachedInputUSDPerMillion, got.CacheWriteUSDPerMillion, got.OutputUSDPerMillion} != tc.want {
			t.Fatalf("%s/%s/%d: %+v, %v; want %v", tc.model, tc.tier, tc.input, got, err, tc.want)
		}
	}
	for _, model := range AntigravityModels() {
		for _, tier := range []string{"flex", "fast", "priority"} {
			if err := pricing.ValidateRequestedServiceTier(model, tier); err == nil {
				t.Fatalf("Gemini %s accepted %s", model, tier)
			}
		}
	}
}
