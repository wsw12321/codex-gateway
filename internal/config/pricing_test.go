package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

const validUsagePricingJSON = `{
	"catalog_as_of":"2026-08-01",
	"fx_as_of":"2026-07-31",
	"usd_cny_rate":"7.20",
	"models":{
		"gpt-exact":{
			"input_usd_per_million":"1.25",
			"cached_input_usd_per_million":"0.125",
			"output_usd_per_million":"10.000000000001"
		}
	}
}`

func TestParseUsagePricing(t *testing.T) {
	pricing, err := ParseUsagePricing(validUsagePricingJSON)
	if err != nil {
		t.Fatal(err)
	}
	if pricing.CatalogAsOf != "2026-08-01" || pricing.FXAsOf != "2026-07-31" {
		t.Fatalf("unexpected pricing dates: %+v", pricing)
	}
	if pricing.USDCNYRate != "7.20" {
		t.Fatalf("USD/CNY rate = %q", pricing.USDCNYRate)
	}
	if got := pricing.Models["gpt-exact"].OutputUSDPerMillion; got != "10.000000000001" {
		t.Fatalf("output price = %q", got)
	}
}

func TestOfficialPricingV2TemplateMatrix(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if pricing.SchemaVersion != PricingSchemaV2 {
		t.Fatalf("schema version = %d", pricing.SchemaVersion)
	}
	wantModels := []string{
		"codex-auto-review", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5",
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
	}
	gotModels := make([]string, 0, len(pricing.Models))
	for model := range pricing.Models {
		gotModels = append(gotModels, model)
	}
	sort.Strings(gotModels)
	if strings.Join(gotModels, ",") != strings.Join(wantModels, ",") {
		t.Fatalf("models = %v", gotModels)
	}
	type expected struct{ input, cached, write, output string }
	short := map[string]map[string]expected{
		"gpt-5.6-sol":   {"default": {"5", "0.5", "6.25", "30"}, "flex": {"2.5", "0.25", "3.125", "15"}, "priority": {"10", "1", "12.5", "60"}},
		"gpt-5.6-terra": {"default": {"2", "0.2", "2.5", "12"}, "flex": {"1", "0.1", "1.25", "6"}, "priority": {"4", "0.4", "5", "24"}},
		"gpt-5.6-luna":  {"default": {"0.2", "0.02", "0.25", "1.2"}, "flex": {"0.1", "0.01", "0.125", "0.6"}, "priority": {"0.4", "0.04", "0.5", "2.4"}},
		"gpt-5.5":       {"default": {"5", "0.5", "0", "30"}, "flex": {"2.5", "0.25", "0", "15"}, "priority": {"12.5", "1.25", "0", "75"}},
		"gpt-5.4":       {"default": {"2.5", "0.25", "0", "15"}, "flex": {"1.25", "0.13", "0", "7.5"}, "priority": {"5", "0.5", "0", "30"}},
		"gpt-5.4-mini":  {"default": {"0.75", "0.075", "0", "4.5"}, "flex": {"0.375", "0.0375", "0", "2.25"}, "priority": {"1.5", "0.15", "0", "9"}},
	}
	for model, tiers := range short {
		snapshotRaw, _, ok, err := pricing.ModelSnapshot(model)
		if err != nil || !ok {
			t.Fatalf("snapshot %s: ok=%t err=%v", model, ok, err)
		}
		snapshot, err := ParsePricingSnapshot(snapshotRaw)
		if err != nil {
			t.Fatal(err)
		}
		for tier, want := range tiers {
			got, err := snapshot.Select(tier, 272000)
			if err != nil {
				t.Fatalf("select %s/%s: %v", model, tier, err)
			}
			if [4]string{got.InputUSDPerMillion, got.CachedInputUSDPerMillion, got.CacheWriteUSDPerMillion, got.OutputUSDPerMillion} !=
				([4]string{want.input, want.cached, want.write, want.output}) {
				t.Fatalf("short %s/%s = %+v", model, tier, got)
			}
		}
	}
	long := map[string]map[string]expected{
		"gpt-5.6-sol":   {"default": {"10", "1", "12.5", "45"}, "flex": {"5", "0.5", "6.25", "22.5"}, "priority": {"20", "2", "25", "90"}},
		"gpt-5.6-terra": {"default": {"4", "0.4", "5", "18"}, "flex": {"2", "0.2", "2.5", "9"}, "priority": {"8", "0.8", "10", "36"}},
		"gpt-5.6-luna":  {"default": {"0.4", "0.04", "0.5", "1.8"}, "flex": {"0.2", "0.02", "0.25", "0.9"}, "priority": {"0.8", "0.08", "1", "3.6"}},
		"gpt-5.5":       {"default": {"10", "1", "0", "45"}, "flex": {"5", "0.5", "0", "22.5"}},
		"gpt-5.4":       {"default": {"5", "0.5", "0", "22.5"}, "flex": {"2.5", "0.25", "0", "11.25"}},
	}
	for model, tiers := range long {
		raw, _, _, _ := pricing.ModelSnapshot(model)
		snapshot, _ := ParsePricingSnapshot(raw)
		for tier, want := range tiers {
			got, err := snapshot.Select(tier, 272001)
			if err != nil {
				t.Fatalf("select long %s/%s: %v", model, tier, err)
			}
			wantTier, _ := NormalizeServiceTier(tier)
			if got.ContextClass != ContextClassLong || got.FallbackReason != "" || got.PricingServiceTier != wantTier {
				t.Fatalf("long metadata %s/%s = %+v", model, tier, got)
			}
			if [4]string{got.InputUSDPerMillion, got.CachedInputUSDPerMillion, got.CacheWriteUSDPerMillion, got.OutputUSDPerMillion} !=
				([4]string{want.input, want.cached, want.write, want.output}) {
				t.Fatalf("long %s/%s = %+v", model, tier, got)
			}
		}
	}
	longFallbacks := map[string]expected{
		"gpt-5.5":      {"12.5", "1.25", "0", "75"},
		"gpt-5.4":      {"5", "0.5", "0", "30"},
		"gpt-5.4-mini": {"1.5", "0.15", "0", "9"},
	}
	for model, want := range longFallbacks {
		raw, _, _, _ := pricing.ModelSnapshot(model)
		snapshot, _ := ParsePricingSnapshot(raw)
		got, err := snapshot.Select("priority", 272001)
		if err != nil || got.FallbackReason != FallbackMissingPriceCombination {
			t.Fatalf("long fallback %s = %+v, %v", model, got, err)
		}
		if [4]string{got.InputUSDPerMillion, got.CachedInputUSDPerMillion, got.CacheWriteUSDPerMillion, got.OutputUSDPerMillion} !=
			([4]string{want.input, want.cached, want.write, want.output}) {
			t.Fatalf("long fallback prices %s = %+v", model, got)
		}
	}
	if got := pricing.Models["gpt-5.4-mini"].MaxInputTokens; got != 272000 {
		t.Fatalf("gpt-5.4-mini max input = %d", got)
	}
}

func TestParseUsagePricingRejectsInvalidValues(t *testing.T) {
	validModel := `"x":{"input_usd_per_million":"1","cached_input_usd_per_million":"0","output_usd_per_million":"1"}`
	tests := map[string]string{
		"empty":               "",
		"invalid JSON":        `{`,
		"trailing JSON":       validUsagePricingJSON + `{}`,
		"unknown field":       `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","extra":true,"models":{` + validModel + `}}`,
		"invalid catalog":     `{"catalog_as_of":"today","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{` + validModel + `}}`,
		"invalid FX date":     `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-02-30","usd_cny_rate":"7","models":{` + validModel + `}}`,
		"negative FX":         `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"-7","models":{` + validModel + `}}`,
		"zero FX":             `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"0","models":{` + validModel + `}}`,
		"FX over scale":       `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"0.0000000000001","models":{` + validModel + `}}`,
		"FX integer overflow": `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"1000000000000000000","models":{` + validModel + `}}`,
		"exponent":            `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7e0","models":{` + validModel + `}}`,
		"leading decimal":     `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":".7","models":{` + validModel + `}}`,
		"leading zero":        `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"07","models":{` + validModel + `}}`,
		"missing models":      `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7"}`,
		"empty models":        `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{}}`,
		"blank model":         `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{" ":{"input_usd_per_million":"1","cached_input_usd_per_million":"0","output_usd_per_million":"1"}}}`,
		"model with slash":    `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{"openai/x":{"input_usd_per_million":"1","cached_input_usd_per_million":"0","output_usd_per_million":"1"}}}`,
		"oversized model":     `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{"` + strings.Repeat("m", 129) + `":{"input_usd_per_million":"1","cached_input_usd_per_million":"0","output_usd_per_million":"1"}}}`,
		"missing model price": `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{"x":{"input_usd_per_million":"1","output_usd_per_million":"1"}}}`,
		"negative price":      `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{"x":{"input_usd_per_million":"-1","cached_input_usd_per_million":"0","output_usd_per_million":"1"}}}`,
		"price over scale":    `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{"x":{"input_usd_per_million":"0.0000000000001","cached_input_usd_per_million":"0","output_usd_per_million":"1"}}}`,
		"price overflow":      `{"catalog_as_of":"2026-01-01","fx_as_of":"2026-01-01","usd_cny_rate":"7","models":{"x":{"input_usd_per_million":"1000000000000000000","cached_input_usd_per_million":"0","output_usd_per_million":"1"}}}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseUsagePricing(raw); err == nil {
				t.Fatalf("accepted invalid pricing: %s", raw)
			}
		})
	}
}

func TestParseUsagePricingModelCountBounds(t *testing.T) {
	if _, err := ParseUsagePricing(usagePricingJSONWithModels(t, maxUsagePricingModels)); err != nil {
		t.Fatalf("rejected exactly %d models: %v", maxUsagePricingModels, err)
	}
	if _, err := ParseUsagePricing(usagePricingJSONWithModels(t, maxUsagePricingModels+1)); err == nil {
		t.Fatalf("accepted more than %d models", maxUsagePricingModels)
	}
}

func usagePricingJSONWithModels(t *testing.T, count int) string {
	t.Helper()
	models := make(map[string]ModelPricing, count)
	for index := 0; index < count; index++ {
		models[fmt.Sprintf("model-%04d", index)] = ModelPricing{
			InputUSDPerMillion:       "1",
			CachedInputUSDPerMillion: "0.1",
			OutputUSDPerMillion:      "10",
		}
	}
	encoded, err := json.Marshal(struct {
		CatalogAsOf string                  `json:"catalog_as_of"`
		FXAsOf      string                  `json:"fx_as_of"`
		USDCNYRate  string                  `json:"usd_cny_rate"`
		Models      map[string]ModelPricing `json:"models"`
	}{
		CatalogAsOf: "2026-01-01",
		FXAsOf:      "2026-01-02",
		USDCNYRate:  "7.2",
		Models:      models,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestParseUsagePricingAllowsNumericSnapshotBoundaries(t *testing.T) {
	pricing, err := ParseUsagePricing(`{
		"catalog_as_of":"2026-01-01",
		"fx_as_of":"2026-01-02",
		"usd_cny_rate":"999999999999999999.999999999999",
		"models":{"free-model":{
			"input_usd_per_million":"0",
			"cached_input_usd_per_million":"0.000000000001",
			"output_usd_per_million":"999999999999999999.999999999999"
		}}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := pricing.Models["free-model"].CachedInputUSDPerMillion; got != "0.000000000001" {
		t.Fatalf("cached input price = %q", got)
	}
}

func TestParseUsagePricingV2SelectsTierContextAndFallbacks(t *testing.T) {
	pricing, err := ParseUsagePricing(`{
		"schema_version":2,
		"catalog_as_of":"2026-08-20",
		"fx_as_of":"2026-08-20",
		"usd_cny_rate":"7.2",
		"fallback_policy":{
			"unknown_service_tier":"max_published",
			"missing_price_combination":"max_published",
			"missing_cache_write_tokens":"all_uncached_as_write"
		},
		"models":{"gpt-test":{
			"cache_write_mode":"separate",
			"max_input_tokens":1050000,
			"long_context_threshold_tokens":272000,
			"service_tiers":{
				"standard":{"short":{"input_usd_per_million":"5","cached_input_usd_per_million":"0.5","cache_write_usd_per_million":"6.25","output_usd_per_million":"30"},"long":{"input_usd_per_million":"10","cached_input_usd_per_million":"1","cache_write_usd_per_million":"12.5","output_usd_per_million":"45"}},
				"flex":{"short":{"input_usd_per_million":"2.5","cached_input_usd_per_million":"0.25","cache_write_usd_per_million":"3.125","output_usd_per_million":"15"}},
				"fast":{"short":{"input_usd_per_million":"12.5","cached_input_usd_per_million":"1.25","cache_write_usd_per_million":"15.625","output_usd_per_million":"75"}}
			}
		}}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, ok, err := pricing.ModelSnapshot("gpt-test")
	if err != nil || !ok {
		t.Fatalf("snapshot: ok=%t err=%v", ok, err)
	}
	snapshot, err := ParsePricingSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	short, err := snapshot.Select("default", 272000)
	if err != nil || short.ContextClass != ContextClassShort || short.InputUSDPerMillion != "5" {
		t.Fatalf("short decision = %+v, %v", short, err)
	}
	long, err := snapshot.Select("default", 272001)
	if err != nil || long.ContextClass != ContextClassLong || long.OutputUSDPerMillion != "45" {
		t.Fatalf("long decision = %+v, %v", long, err)
	}
	missingFastLong, err := snapshot.Select("priority", 272001)
	if err != nil || missingFastLong.PricingServiceTier != PricingTierMaxPublished ||
		missingFastLong.InputUSDPerMillion != "12.5" || missingFastLong.OutputUSDPerMillion != "75" ||
		missingFastLong.FallbackReason != FallbackMissingPriceCombination {
		t.Fatalf("missing fast long decision = %+v, %v", missingFastLong, err)
	}
	unknown, err := snapshot.Select("ultrafast", 100)
	if err != nil || unknown.PricingServiceTier != PricingTierMaxPublished ||
		unknown.OutputUSDPerMillion != "75" || unknown.FallbackReason != FallbackUnknownServiceTier {
		t.Fatalf("unknown tier decision = %+v, %v", unknown, err)
	}
	overMaximumMissingTier, err := snapshot.Select("", 1_050_001)
	wantFallbacks := AppendFallbackReason(FallbackMissingPriceCombination, FallbackMissingServiceTier)
	if err != nil || overMaximumMissingTier.PricingServiceTier != PricingTierMaxPublished ||
		overMaximumMissingTier.FallbackReason != wantFallbacks {
		t.Fatalf("over-maximum missing-tier decision = %+v, %v", overMaximumMissingTier, err)
	}
}

func TestParseUsagePricingV2RejectsMixedSchemas(t *testing.T) {
	base := `{"schema_version":2,"catalog_as_of":"2026-08-20","fx_as_of":"2026-08-20","usd_cny_rate":"7.2","fallback_policy":{"unknown_service_tier":"max_published","missing_price_combination":"max_published","missing_cache_write_tokens":"all_uncached_as_write"},"models":{"x":{"cache_write_mode":"included_in_input","max_input_tokens":272000,"long_context_threshold_tokens":272000,"service_tiers":{"standard":{"short":{"input_usd_per_million":"1","cached_input_usd_per_million":"0.1","output_usd_per_million":"2"}}},%s}}}`
	if _, err := ParseUsagePricing(fmt.Sprintf(base, `"input_usd_per_million":"1"`)); err == nil {
		t.Fatal("v2 accepted a v1 price field")
	}
	if _, err := ParseUsagePricing(strings.Replace(fmt.Sprintf(base, `"unused":"x"`), `,"unused":"x"`, "", 1)); err != nil {
		t.Fatalf("valid v2 rejected: %v", err)
	}
}

func TestParseUsagePricingRejectsPresentCrossVersionFields(t *testing.T) {
	v2Base := `{"schema_version":2,"catalog_as_of":"2026-08-20","fx_as_of":"2026-08-20","usd_cny_rate":"7.2","fallback_policy":{"unknown_service_tier":"max_published","missing_price_combination":"max_published","missing_cache_write_tokens":"all_uncached_as_write"},"models":{"x":{"cache_write_mode":"included_in_input","max_input_tokens":272000,"long_context_threshold_tokens":272000,"service_tiers":{"standard":{"short":{"input_usd_per_million":"1","cached_input_usd_per_million":"0.1","output_usd_per_million":"2"}}},%s}}}`
	for _, field := range []string{`"input_usd_per_million":""`, `"cached_input_usd_per_million":null`, `"output_usd_per_million":""`} {
		if _, err := ParseUsagePricing(fmt.Sprintf(v2Base, field)); err == nil {
			t.Fatalf("v2 accepted present v1 field: %s", field)
		}
	}

	v1Base := `{"catalog_as_of":"2026-08-20","fx_as_of":"2026-08-20","usd_cny_rate":"7.2","models":{"x":{"input_usd_per_million":"1","cached_input_usd_per_million":"0.1","output_usd_per_million":"2",%s}}}`
	for _, field := range []string{`"cache_write_mode":""`, `"max_input_tokens":0`, `"long_context_threshold_tokens":null`, `"service_tiers":{}`} {
		if _, err := ParseUsagePricing(fmt.Sprintf(v1Base, field)); err == nil {
			t.Fatalf("v1 accepted present v2 field: %s", field)
		}
	}
	for _, fallback := range []string{`null`, `{}`} {
		raw := strings.Replace(validUsagePricingJSON, `"models"`, `"fallback_policy":`+fallback+`,"models"`, 1)
		if _, err := ParseUsagePricing(raw); err == nil {
			t.Fatalf("v1 accepted present fallback_policy: %s", fallback)
		}
	}
}
