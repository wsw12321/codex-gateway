package config

import (
	"encoding/json"
	"fmt"
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
	encoded, err := json.Marshal(UsagePricing{
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
