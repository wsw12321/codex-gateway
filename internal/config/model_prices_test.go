package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func editablePricingFixture(mode string) UsagePricing {
	rule := ModelPricing{CacheWriteMode: mode, MaxInputTokens: 1000000, LongContextThresholdTokens: 200000, ServiceTiers: map[string]ServiceTierPricing{}}
	for _, tier := range []string{PricingTierStandard, PricingTierFlex, PricingTierFast} {
		makePrice := func() *TokenPricing {
			price := &TokenPricing{InputUSDPerMillion: "1.000", CachedInputUSDPerMillion: "0.125000", OutputUSDPerMillion: "12.00"}
			if mode == CacheWriteSeparate {
				cache := "1.250000"
				price.CacheWriteUSDPerMillion = &cache
			}
			return price
		}
		rule.ServiceTiers[tier] = ServiceTierPricing{Short: makePrice(), Long: makePrice()}
	}
	return UsagePricing{SchemaVersion: PricingSchemaV2, FallbackPolicy: PricingFallbackPolicy{UnknownServiceTier: FallbackMaxPublished, MissingPriceCombination: FallbackMaxPublished, MissingCacheWriteTokens: FallbackAllUncachedAsWrite}, Models: map[string]ModelPricing{"test-model": rule}}
}

func cloneEditablePrice(rule ModelPricing) ModelPricing {
	raw, _ := json.Marshal(rule)
	var result ModelPricing
	_ = json.Unmarshal(raw, &result)
	return result
}

func TestModelPriceOverrideAllTiersContextsAndCacheModes(t *testing.T) {
	for _, mode := range []string{CacheWriteSeparate, CacheWriteIncludedInInput} {
		t.Run(mode, func(t *testing.T) {
			pricing := editablePricingFixture(mode)
			before := cloneEditablePrice(pricing.Models["test-model"])
			price, err := pricing.ValidateModelPriceOverride("test-model", pricing.Models["test-model"])
			if err != nil {
				t.Fatal(err)
			}
			for tier, contexts := range price.ServiceTiers {
				for _, value := range []*TokenPricing{contexts.Short, contexts.Long} {
					if value.InputUSDPerMillion != "1" || value.CachedInputUSDPerMillion != "0.125" || value.OutputUSDPerMillion != "12" {
						t.Fatalf("normalization failed for %s: %+v", tier, value)
					}
					if mode == CacheWriteSeparate && (value.CacheWriteUSDPerMillion == nil || *value.CacheWriteUSDPerMillion != "1.25") {
						t.Fatalf("cache write normalization: %+v", value)
					}
					value.InputUSDPerMillion = "0"
					value.OutputUSDPerMillion = "999999999999999999.999999999999"
				}
			}
			if _, err := pricing.ValidateModelPriceOverride("test-model", price); err != nil {
				t.Fatalf("zero and maximum precision: %v", err)
			}
			if !reflect.DeepEqual(before, pricing.Models["test-model"]) {
				t.Fatal("override normalization mutated deployment rules")
			}
		})
	}
}

func TestModelPriceOverrideRejectsReadonlyAndInvalidPrices(t *testing.T) {
	pricing := editablePricingFixture(CacheWriteSeparate)
	changes := map[string]func(*ModelPricing){
		"threshold":   func(p *ModelPricing) { p.LongContextThresholdTokens++ },
		"maximum":     func(p *ModelPricing) { p.MaxInputTokens++ },
		"mode":        func(p *ModelPricing) { p.CacheWriteMode = CacheWriteIncludedInInput },
		"remove-tier": func(p *ModelPricing) { delete(p.ServiceTiers, PricingTierFlex) },
		"remove-context": func(p *ModelPricing) {
			v := p.ServiceTiers[PricingTierFast]
			v.Long = nil
			p.ServiceTiers[PricingTierFast] = v
		},
		"remove-cache-write": func(p *ModelPricing) { p.ServiceTiers[PricingTierStandard].Short.CacheWriteUSDPerMillion = nil },
		"mixed-schema":       func(p *ModelPricing) { p.InputUSDPerMillion = "1" },
	}
	for _, invalid := range []string{"-1", "1e2", "NaN", "1.0000000000001", "1000000000000000000", "", " 1", "01"} {
		changes["amount-"+invalid] = func(p *ModelPricing) { p.ServiceTiers[PricingTierFlex].Long.OutputUSDPerMillion = invalid }
	}
	for name, mutate := range changes {
		t.Run(name, func(t *testing.T) {
			rule := cloneEditablePrice(pricing.Models["test-model"])
			mutate(&rule)
			if _, err := pricing.ValidateModelPriceOverride("test-model", rule); err == nil {
				t.Fatal("invalid override accepted")
			}
		})
	}
	if _, err := pricing.ValidateModelPriceOverride(InternalGovernanceModel, pricing.Models["test-model"]); err == nil {
		t.Fatal("internal model editable")
	}
	if _, err := pricing.ValidateModelPriceOverride("unknown", pricing.Models["test-model"]); err == nil {
		t.Fatal("unknown model editable")
	}
}

func TestModelPriceStructureTracksSchemaAndReadonlyShape(t *testing.T) {
	pricing := editablePricingFixture(CacheWriteSeparate)
	base, err := pricing.ModelPriceStructureID("test-model")
	if err != nil {
		t.Fatal(err)
	}
	pricing.Models["test-model"].ServiceTiers[PricingTierStandard].Short.InputUSDPerMillion = "42"
	pricing.CatalogAsOf, pricing.USDCNYRate = "2030-01-01", "20"
	if same, _ := pricing.ModelPriceStructureID("test-model"); same != base {
		t.Fatal("mutable price/date altered structure ID")
	}
	pricing.FallbackPolicy.MissingPriceCombination = "different"
	if changed, _ := pricing.ModelPriceStructureID("test-model"); changed == base {
		t.Fatal("fallback policy change did not alter structure ID")
	}
	legacy := UsagePricing{Models: map[string]ModelPricing{"test-model": {InputUSDPerMillion: "0", CachedInputUSDPerMillion: "1.00", OutputUSDPerMillion: "3"}}}
	price, err := legacy.ValidateModelPriceOverride("test-model", legacy.Models["test-model"])
	if err != nil || price.CachedInputUSDPerMillion != "1" {
		t.Fatalf("legacy normalization: %+v %v", price, err)
	}
	omitted, _ := legacy.ModelPriceStructureID("test-model")
	legacy.SchemaVersion = PricingSchemaV1
	explicit, _ := legacy.ModelPriceStructureID("test-model")
	if omitted != explicit {
		t.Fatal("implicit legacy schema produced different structure")
	}
	if explicit == base {
		t.Fatal("v1 and v2 share structure ID")
	}
}
