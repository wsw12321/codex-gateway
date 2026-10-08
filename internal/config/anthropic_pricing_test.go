package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/billing"
)

func TestClaudePricingV2StandardBoundariesAndTTL(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model                      string
		maximum                    int64
		prices                     [5]string // Input, output, cache read, 5m write, 1h write.
		mixedTTLCost, fallbackCost string
	}{
		{"claude-fable-5-1", 1_000_000, [5]string{"10", "50", "0.25", "12.50", "20"}, "0.015775000000", "0.018025000000"},
		{"claude-opus-5-5", 1_000_000, [5]string{"4", "20", "0.20", "5", "8"}, "0.006320000000", "0.007220000000"},
		{"claude-sonnet-5-5", 1_000_000, [5]string{"2", "10", "0.10", "2.50", "4"}, "0.003160000000", "0.003610000000"},
		{"claude-haiku-4-5-20251001", 200_000, [5]string{"1", "5", "0.10", "1.25", "2"}, "0.001585000000", "0.001810000000"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			raw, rule, ok, err := pricing.ModelSnapshot(tc.model)
			if err != nil || !ok {
				t.Fatalf("snapshot: ok=%t err=%v", ok, err)
			}
			standard, ok := rule.ServiceTiers[PricingTierStandard]
			if rule.CacheWriteMode != CacheWriteSeparateByTTL || rule.MaxInputTokens != tc.maximum ||
				rule.LongContextThresholdTokens != tc.maximum || len(rule.ServiceTiers) != 1 ||
				!ok || standard.Short == nil || standard.Long != nil {
				t.Fatalf("Claude pricing rule = %+v", rule)
			}
			if standard.Short.CacheWriteUSDPerMillion != nil || standard.Short.CacheWrite5mUSDPerMillion == nil || standard.Short.CacheWrite1hUSDPerMillion == nil {
				t.Fatalf("Claude requires separate TTL write prices: %+v", standard.Short)
			}
			for _, tier := range []string{"", "default", "standard"} {
				if err := pricing.ValidateRequestedServiceTier(tc.model, tier); err != nil {
					t.Errorf("rejected Standard tier %q: %v", tier, err)
				}
			}
			for _, tier := range []string{"fast", "priority", "flex", "ultrafast"} {
				if err := pricing.ValidateRequestedServiceTier(tc.model, tier); err == nil {
					t.Errorf("accepted unconfigured tier %q", tier)
				}
			}
			snapshot, err := ParsePricingSnapshot(raw)
			if err != nil {
				t.Fatal(err)
			}
			inputs := []int64{0, 200_000, tc.maximum - 1, tc.maximum}
			if tc.maximum > 200_000 {
				inputs = append(inputs, 200_001)
			}
			for _, input := range inputs {
				decision, err := snapshot.Select("standard", input)
				if err != nil {
					t.Fatal(err)
				}
				if decision.ContextClass != ContextClassShort || decision.PricingServiceTier != PricingTierStandard || decision.FallbackReason != "" {
					t.Fatalf("Claude pricing at %d = %+v", input, decision)
				}
				got := [5]string{decision.InputUSDPerMillion, decision.OutputUSDPerMillion, decision.CachedInputUSDPerMillion,
					decision.CacheWrite5mUSDPerMillion, decision.CacheWrite1hUSDPerMillion}
				for index, price := range got {
					canonical, err := billing.ParsePrice(price)
					want, wantErr := billing.ParsePrice(tc.prices[index])
					if err != nil || wantErr != nil || canonical != want {
						t.Fatalf("Claude price %d at %d = %q, want %q", index, input, price, tc.prices[index])
					}
				}
				if decision.CacheWriteUSDPerMillion != tc.prices[4] {
					t.Fatalf("missing-TTL write price = %q, want %q", decision.CacheWriteUSDPerMillion, tc.prices[4])
				}
			}
			aboveMaximum, err := snapshot.Select("standard", tc.maximum+1)
			if err != nil || aboveMaximum.ContextClass != ContextClassLong || aboveMaximum.PricingServiceTier != PricingTierMaxPublished || aboveMaximum.FallbackReason != FallbackMissingPriceCombination {
				t.Fatalf("above-maximum decision = %+v, %v", aboveMaximum, err)
			}
			decision, err := snapshot.Select("standard", 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, ttl := range []struct {
				present    bool
				five, hour int64
				want       string
			}{
				{true, 300, 200, tc.mixedTTLCost},
				{false, 0, 0, tc.fallbackCost},
			} {
				// 1,000 total input = 400 ordinary + 100 reads + 500 writes.
				cost, err := billing.CalculateCostV2TTLWithMultiplier(1000, 100, 500, ttl.five, ttl.hour, 80, ttl.present,
					decision.InputUSDPerMillion, decision.CachedInputUSDPerMillion, decision.CacheWrite5mUSDPerMillion,
					decision.CacheWrite1hUSDPerMillion, decision.OutputUSDPerMillion, "1")
				if err != nil || cost != ttl.want {
					t.Fatalf("cost with TTL present=%t: %q, %v; want %q", ttl.present, cost, err, ttl.want)
				}
			}
		})
	}
}

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
	pricing = changed
	newRaw, _, ok, err := pricing.ModelSnapshot("claude-test")
	if err != nil || !ok {
		t.Fatalf("updated snapshot: %v %v", ok, err)
	}
	newSnapshot, err := ParsePricingSnapshot(newRaw)
	if err != nil {
		t.Fatal(err)
	}
	newDecision, err := newSnapshot.Select("standard", 1000)
	if err != nil || newDecision.CacheWrite5mUSDPerMillion != "4" {
		t.Fatalf("new snapshot did not capture the changed price: %+v %v", newDecision, err)
	}
	// Re-reading a persisted reservation must retain its original price even
	// after the active catalog is replaced.
	savedSnapshot, err := ParsePricingSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	savedDecision, err := savedSnapshot.Select("standard", 1000)
	if err != nil || savedDecision != decision {
		t.Fatalf("persisted snapshot changed after catalog update: %+v %v", savedDecision, err)
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
