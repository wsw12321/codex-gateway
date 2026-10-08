package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func anthropicPricingFixture(t *testing.T) UsagePricing {
	t.Helper()
	five, hour := "3.750", "6.00"
	return UsagePricing{SchemaVersion: PricingSchemaV2, CatalogAsOf: "2026-10-08", FXAsOf: "2026-10-08", USDCNYRate: "7.2",
		FallbackPolicy: PricingFallbackPolicy{UnknownServiceTier: FallbackMaxPublished, MissingPriceCombination: FallbackMaxPublished, MissingCacheWriteTokens: FallbackAllUncachedAsWrite},
		Models: map[string]ModelPricing{"claude-test": {CacheWriteMode: CacheWriteSeparateByTTL,
			MaxInputTokens: 200000, LongContextThresholdTokens: 200000,
			ServiceTiers: map[string]ServiceTierPricing{PricingTierStandard: {Short: &TokenPricing{
				InputUSDPerMillion: "3", CachedInputUSDPerMillion: "0.3", OutputUSDPerMillion: "15",
				CacheWrite5mUSDPerMillion: &five, CacheWrite1hUSDPerMillion: &hour,
			}}}}}}
}

func TestAnthropicPricingSnapshotTTLAndOverride(t *testing.T) {
	pricing := anthropicPricingFixture(t)
	encoded, err := json.Marshal(pricing)
	if err != nil {
		t.Fatal(err)
	}
	pricing, err = ParseUsagePricing(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, ok, err := pricing.ModelSnapshot("claude-test")
	if err != nil || !ok {
		t.Fatalf("snapshot: %v %v", ok, err)
	}
	snapshot, err := ParsePricingSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := snapshot.Select("standard", 1000)
	if err != nil || decision.CacheWrite5mUSDPerMillion != "3.750" || decision.CacheWrite1hUSDPerMillion != "6.00" || decision.CacheWriteUSDPerMillion != "6" || decision.FallbackReason != "" {
		t.Fatalf("TTL decision: %+v %v", decision, err)
	}
	initialID, err := pricing.ModelPriceStructureID("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := NormalizeModelPrice(PricingSchemaV2, "claude-test", pricing.Models["claude-test"])
	if err != nil {
		t.Fatal(err)
	}
	*updated.ServiceTiers[PricingTierStandard].Short.CacheWrite5mUSDPerMillion = "4"
	changed := pricing
	changed.Models = map[string]ModelPricing{"claude-test": updated}
	changedID, err := changed.ModelPriceStructureID("claude-test")
	if err != nil || changedID != initialID {
		t.Fatalf("price change invalidates structure: %v", err)
	}
	if _, err := pricing.ValidateModelPriceOverride("claude-test", updated); err != nil {
		t.Fatal(err)
	}
	// Normalization must clone pointers: editing prices cannot mutate a bound snapshot.
	if *pricing.Models["claude-test"].ServiceTiers[PricingTierStandard].Short.CacheWrite5mUSDPerMillion != "3.750" {
		t.Fatal("normalization shared TTL price pointer")
	}
}

func TestAnthropicPricingRejectsIncompleteOrMixedTTLPriceFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*TokenPricing)
	}{
		{"missing five minute", func(p *TokenPricing) { p.CacheWrite5mUSDPerMillion = nil }},
		{"missing one hour", func(p *TokenPricing) { p.CacheWrite1hUSDPerMillion = nil }},
		{"legacy aggregate price", func(p *TokenPricing) { v := "6"; p.CacheWriteUSDPerMillion = &v }},
		{"negative price", func(p *TokenPricing) { v := "-1"; p.CacheWrite1hUSDPerMillion = &v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pricing := anthropicPricingFixture(t)
			tc.mutate(pricing.Models["claude-test"].ServiceTiers[PricingTierStandard].Short)
			raw, _ := json.Marshal(pricing)
			if _, err := ParseUsagePricing(string(raw)); err == nil {
				t.Fatal("accepted invalid TTL pricing")
			}
		})
	}
	pricing := anthropicPricingFixture(t)
	rule := pricing.Models["claude-test"]
	rule.CacheWriteMode = CacheWriteIncludedInInput
	if _, err := NormalizeModelPrice(PricingSchemaV2, "claude-test", rule); err == nil {
		t.Fatal("accepted TTL prices in included mode")
	}
}

func TestAnthropicUnknownTierMaximizesEachTTL(t *testing.T) {
	pricing := anthropicPricingFixture(t)
	five, hour := "9", "4"
	pricing.Models["claude-test"].ServiceTiers[PricingTierFast] = ServiceTierPricing{Short: &TokenPricing{
		InputUSDPerMillion: "2", CachedInputUSDPerMillion: "0.2", OutputUSDPerMillion: "14",
		CacheWrite5mUSDPerMillion: &five, CacheWrite1hUSDPerMillion: &hour,
	}}
	raw, _, _, _ := pricing.ModelSnapshot("claude-test")
	snapshot, err := ParsePricingSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := snapshot.Select("future-tier", 1000)
	if err != nil || decision.CacheWrite5mUSDPerMillion != "9" || decision.CacheWrite1hUSDPerMillion != "6" || decision.CacheWriteUSDPerMillion != "9" || !strings.Contains(decision.FallbackReason, FallbackUnknownServiceTier) {
		t.Fatalf("max TTL decision: %+v %v", decision, err)
	}
}
