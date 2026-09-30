package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
)

// CPAV8PricingCatalogAsOf identifies the explicitly reviewed upgrade. This
// migration is an offline operation, never applied during normal startup.
const CPAV8PricingCatalogAsOf = "2026-09-30"

//go:embed data/cpa-v8-model-prices.json
var cpaV8ModelPrices []byte

// CPAV8PricingModels returns a fresh copy of the nine reviewed price rules.
// MaxInputTokens is a billing boundary, not a claim of channel capabilities.
func CPAV8PricingModels() map[string]ModelPricing {
	var models map[string]ModelPricing
	if err := json.Unmarshal(cpaV8ModelPrices, &models); err != nil {
		panic("invalid embedded CPA v8 pricing: " + err.Error())
	}
	return models
}

// MigrateCPAV8Pricing merges the reviewed prices into the entire configured
// catalog. Unrelated models, FX and fallback policy survive unchanged. Only
// retired Gemini aliases are removed; API key allowlists are never consulted.
func MigrateCPAV8Pricing(raw []byte) ([]byte, error) {
	pricing, err := ParseUsagePricing(string(raw))
	if err != nil {
		return nil, err
	}
	if pricing.SchemaVersion != PricingSchemaV2 {
		return nil, fmt.Errorf("CPA v8 pricing migration requires schema_version 2; convert legacy pricing explicitly first")
	}
	for _, model := range RetiredAntigravityModels() {
		delete(pricing.Models, model)
	}
	for model, rule := range CPAV8PricingModels() {
		pricing.Models[model] = rule
	}
	pricing.CatalogAsOf = CPAV8PricingCatalogAsOf
	return marshalValidatedPricing(pricing)
}

// RestoreCPAV8Pricing reverts only migration changes that still match the
// applied snapshot. Subsequent operator changes and unrelated current models
// and FX settings remain intact. before and applied are protected cutover
// snapshots, while current is the configuration in use at rollback time.
func RestoreCPAV8Pricing(current, before, applied []byte) ([]byte, error) {
	currentPricing, err := ParseUsagePricing(string(current))
	if err != nil {
		return nil, err
	}
	beforePricing, err := ParseUsagePricing(string(before))
	if err != nil {
		return nil, fmt.Errorf("parse before snapshot: %w", err)
	}
	appliedPricing, err := ParseUsagePricing(string(applied))
	if err != nil {
		return nil, fmt.Errorf("parse applied snapshot: %w", err)
	}
	if currentPricing.SchemaVersion != PricingSchemaV2 || beforePricing.SchemaVersion != PricingSchemaV2 || appliedPricing.SchemaVersion != PricingSchemaV2 {
		return nil, fmt.Errorf("CPA v8 pricing rollback requires schema_version 2 snapshots")
	}
	models := RetiredAntigravityModels()
	for model := range CPAV8PricingModels() {
		models = append(models, model)
	}
	for _, model := range models {
		currentRule, currentOK := currentPricing.Models[model]
		appliedRule, appliedOK := appliedPricing.Models[model]
		if currentOK != appliedOK || !reflect.DeepEqual(currentRule, appliedRule) {
			continue
		}
		if beforeRule, ok := beforePricing.Models[model]; ok {
			currentPricing.Models[model] = beforeRule
		} else {
			delete(currentPricing.Models, model)
		}
	}
	if currentPricing.CatalogAsOf == appliedPricing.CatalogAsOf {
		currentPricing.CatalogAsOf = beforePricing.CatalogAsOf
	}
	return marshalValidatedPricing(currentPricing)
}

func marshalValidatedPricing(pricing UsagePricing) ([]byte, error) {
	raw, err := json.MarshalIndent(pricing, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode pricing: %w", err)
	}
	if _, err := ParseUsagePricing(string(raw)); err != nil {
		return nil, fmt.Errorf("validate migrated pricing: %w", err)
	}
	return append(raw, '\n'), nil
}
