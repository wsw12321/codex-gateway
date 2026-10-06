package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/wsw/codex-gateway/internal/billing"
)

// ModelPriceStructureID identifies everything an administrator cannot edit.
// Price and catalog date changes deliberately do not invalidate an override.
func (p UsagePricing) ModelPriceStructureID(model string) (string, error) {
	rule, ok := p.Models[model]
	if !ok {
		return "", fmt.Errorf("pricing model %q is not configured", model)
	}
	schema := p.SchemaVersion
	if schema == 0 {
		schema = PricingSchemaV1
	}
	structure := modelPriceStructure(rule)
	encoded, err := json.Marshal(struct {
		Schema   int                   `json:"schema"`
		Fallback PricingFallbackPolicy `json:"fallback"`
		Rule     ModelPricing          `json:"rule"`
	}{schema, p.FallbackPolicy, structure})
	if err != nil {
		return "", fmt.Errorf("encode model price structure: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func modelPriceStructure(rule ModelPricing) ModelPricing {
	result := ModelPricing{CacheWriteMode: rule.CacheWriteMode,
		MaxInputTokens: rule.MaxInputTokens, LongContextThresholdTokens: rule.LongContextThresholdTokens}
	if rule.ServiceTiers != nil {
		result.ServiceTiers = make(map[string]ServiceTierPricing, len(rule.ServiceTiers))
		for tier, contexts := range rule.ServiceTiers {
			copyContext := func(price *TokenPricing) *TokenPricing {
				if price == nil {
					return nil
				}
				value := &TokenPricing{}
				if price.CacheWriteUSDPerMillion != nil {
					empty := ""
					value.CacheWriteUSDPerMillion = &empty
				}
				return value
			}
			result.ServiceTiers[tier] = ServiceTierPricing{Short: copyContext(contexts.Short), Long: copyContext(contexts.Long)}
		}
	}
	return result
}

// NormalizeModelPrice validates a complete standalone rule and returns an
// independent copy whose decimal strings have canonical exact representations.
func NormalizeModelPrice(schema int, model string, rule ModelPricing) (ModelPricing, error) {
	var err error
	if schema == 0 || schema == PricingSchemaV1 {
		err = validateV1ModelPricing(model, rule)
	} else if schema == PricingSchemaV2 {
		err = validateV2ModelPricing(model, rule)
	} else {
		err = fmt.Errorf("unsupported pricing schema")
	}
	if err != nil {
		return ModelPricing{}, err
	}
	canonical := func(value string) string { result, _ := billing.ParsePrice(value); return result }
	if schema != PricingSchemaV2 {
		rule.InputUSDPerMillion = canonical(rule.InputUSDPerMillion)
		rule.CachedInputUSDPerMillion = canonical(rule.CachedInputUSDPerMillion)
		rule.OutputUSDPerMillion = canonical(rule.OutputUSDPerMillion)
		return rule, nil
	}
	result := rule
	result.ServiceTiers = make(map[string]ServiceTierPricing, len(rule.ServiceTiers))
	for tier, contexts := range rule.ServiceTiers {
		copyContext := func(price *TokenPricing) *TokenPricing {
			if price == nil {
				return nil
			}
			value := *price
			value.InputUSDPerMillion = canonical(value.InputUSDPerMillion)
			value.CachedInputUSDPerMillion = canonical(value.CachedInputUSDPerMillion)
			value.OutputUSDPerMillion = canonical(value.OutputUSDPerMillion)
			if value.CacheWriteUSDPerMillion != nil {
				cache := canonical(*value.CacheWriteUSDPerMillion)
				value.CacheWriteUSDPerMillion = &cache
			}
			return &value
		}
		result.ServiceTiers[tier] = ServiceTierPricing{Short: copyContext(contexts.Short), Long: copyContext(contexts.Long)}
	}
	return result, nil
}

func (p UsagePricing) ValidateModelPriceOverride(model string, rule ModelPricing) (ModelPricing, error) {
	if model == InternalGovernanceModel {
		return ModelPricing{}, fmt.Errorf("internal pricing is fixed")
	}
	expected, err := p.ModelPriceStructureID(model)
	if err != nil {
		return ModelPricing{}, err
	}
	normalized, err := NormalizeModelPrice(p.SchemaVersion, model, rule)
	if err != nil {
		return ModelPricing{}, err
	}
	candidate := p
	candidate.Models = map[string]ModelPricing{model: normalized}
	actual, err := candidate.ModelPriceStructureID(model)
	if err != nil {
		return ModelPricing{}, err
	}
	if expected != actual {
		return ModelPricing{}, fmt.Errorf("model pricing structure is read-only")
	}
	return normalized, nil
}
