package config

import (
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/billing"
)

func TestDefaultUsagePricingIsIndependent(t *testing.T) {
	first, err := DefaultUsagePricing()
	if err != nil {
		t.Fatal(err)
	}
	delete(first.Models, "gpt-6-sol")
	raw := DefaultUsagePricingJSON()
	raw[0] = 'x'
	second, err := DefaultUsagePricing()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second.Models["gpt-6-sol"]; !ok {
		t.Fatal("callers share the built-in catalog")
	}
	if DefaultUsagePricingJSON()[0] != '{' {
		t.Fatal("callers share the embedded document")
	}
}

// The built-in catalog replaces the deploy-time jq validator: it must stay a
// strict v2 document with only reviewed, positively priced public models.
func TestBuiltInPricingIsDeployable(t *testing.T) {
	pricing, err := DefaultUsagePricing()
	if err != nil {
		t.Fatal(err)
	}
	if pricing.SchemaVersion != PricingSchemaV2 {
		t.Fatalf("schema_version = %d", pricing.SchemaVersion)
	}
	for _, model := range append(RetiredAntigravityModels(), RetiredCodexModels()...) {
		if _, ok := pricing.Models[model]; ok {
			t.Fatalf("built-in catalog prices retired model %q", model)
		}
	}
	for model, rule := range pricing.Models {
		if strings.HasPrefix(model, "gemini-") && !IsAntigravityModel(model) {
			t.Fatalf("built-in catalog prices unreviewed Gemini model %q", model)
		}
		if model == InternalGovernanceModel {
			continue
		}
		if err := requirePositivePrices(model, rule); err != nil {
			t.Fatalf("built-in catalog: %v", err)
		}
	}
	internal := pricing.Models[InternalGovernanceModel].ServiceTiers[PricingTierStandard].Short
	if internal == nil {
		t.Fatalf("built-in catalog lacks %s", InternalGovernanceModel)
	}
	for _, value := range []string{internal.InputUSDPerMillion, internal.CachedInputUSDPerMillion, internal.OutputUSDPerMillion} {
		if price, err := billing.ParsePrice(value); err != nil || price != "0" {
			t.Fatalf("%s must be zero priced, got %q", InternalGovernanceModel, value)
		}
	}
	cfg := validConfigForValidation(t)
	cfg.UsagePricing = pricing
	if err := cfg.Validate(); err != nil {
		t.Fatalf("built-in pricing fails deployment validation: %v", err)
	}
}

func TestCPAV8ReviewedPricingBoundaries(t *testing.T) {
	pricing, err := DefaultUsagePricing()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		model, tier string
		input       int64
		want        [4]string
	}{
		{"gemini-3.8-flash-high", "standard", 1, [4]string{"0.75", "0.075", "0", "3.75"}},
		{"gemini-4-argon-high", "standard", 1, [4]string{"2", "0.10", "0", "10"}},
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

func TestRetiredCodexPricingCannotReactivateModelAccess(t *testing.T) {
	for _, model := range RetiredCodexModels() {
		cfg := validConfigForValidation(t)
		cfg.UsagePricing.Models = map[string]ModelPricing{
			model: {InputUSDPerMillion: "5", CachedInputUSDPerMillion: "0.5", OutputUSDPerMillion: "30"},
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "retired CPA model") {
			t.Fatalf("retired Codex model %q: %v", model, err)
		}
	}
}
