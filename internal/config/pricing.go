package config

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/billing"
)

const (
	maxUsagePricingModels = 1000

	PricingSchemaV1 = 1
	PricingSchemaV2 = 2

	CacheWriteSeparate        = "separate"
	CacheWriteIncludedInInput = "included_in_input"

	PricingTierStandard     = "standard"
	PricingTierFlex         = "flex"
	PricingTierFast         = "fast"
	PricingTierMaxPublished = "max_published"

	ContextClassShort = "short"
	ContextClassLong  = "long"

	FallbackUnknownServiceTier      = "unknown_service_tier"
	FallbackMissingServiceTier      = "missing_service_tier"
	FallbackMissingPriceCombination = "missing_price_combination"
	FallbackMissingCacheWriteTokens = "missing_cache_write_tokens"

	FallbackMaxPublished       = "max_published"
	FallbackAllUncachedAsWrite = "all_uncached_as_write"
)

var usagePricingModelPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// UsagePricing is an operator-supplied pricing snapshot. An omitted
// schema_version is the legacy v1 three-price format. Version 2 snapshots a
// complete model/service-tier/context matrix and its conservative fallback
// policy so admission and settlement never depend on mutable process state.
type UsagePricing struct {
	SchemaVersion  int                     `json:"schema_version,omitempty"`
	CatalogAsOf    string                  `json:"catalog_as_of"`
	FXAsOf         string                  `json:"fx_as_of"`
	USDCNYRate     string                  `json:"usd_cny_rate"`
	FallbackPolicy PricingFallbackPolicy   `json:"fallback_policy,omitempty"`
	Models         map[string]ModelPricing `json:"models"`
}

type PricingFallbackPolicy struct {
	UnknownServiceTier      string `json:"unknown_service_tier,omitempty"`
	MissingPriceCombination string `json:"missing_price_combination,omitempty"`
	MissingCacheWriteTokens string `json:"missing_cache_write_tokens,omitempty"`
}

// ModelPricing is a strict tagged union selected by UsagePricing.SchemaVersion.
// The three top-level prices are v1-only. All remaining fields are v2-only.
type ModelPricing struct {
	InputUSDPerMillion       string `json:"input_usd_per_million,omitempty"`
	CachedInputUSDPerMillion string `json:"cached_input_usd_per_million,omitempty"`
	OutputUSDPerMillion      string `json:"output_usd_per_million,omitempty"`

	CacheWriteMode             string                        `json:"cache_write_mode,omitempty"`
	MaxInputTokens             int64                         `json:"max_input_tokens,omitempty"`
	LongContextThresholdTokens int64                         `json:"long_context_threshold_tokens,omitempty"`
	ServiceTiers               map[string]ServiceTierPricing `json:"service_tiers,omitempty"`
}

type ServiceTierPricing struct {
	Short *TokenPricing `json:"short,omitempty"`
	Long  *TokenPricing `json:"long,omitempty"`
}

type TokenPricing struct {
	InputUSDPerMillion       string  `json:"input_usd_per_million"`
	CachedInputUSDPerMillion string  `json:"cached_input_usd_per_million"`
	CacheWriteUSDPerMillion  *string `json:"cache_write_usd_per_million,omitempty"`
	OutputUSDPerMillion      string  `json:"output_usd_per_million"`
}

// PricingSnapshot is persisted in billing_reservations for v2. It is kept
// deliberately small: only the selected model rule and the fallback policy
// are needed after admission.
type PricingSnapshot struct {
	SchemaVersion  int                   `json:"schema_version"`
	FallbackPolicy PricingFallbackPolicy `json:"fallback_policy"`
	Model          string                `json:"model"`
	Rule           ModelPricing          `json:"rule"`
}

type PricingDecision struct {
	PricingServiceTier       string
	ContextClass             string
	InputUSDPerMillion       string
	CachedInputUSDPerMillion string
	CacheWriteUSDPerMillion  string
	OutputUSDPerMillion      string
	FallbackReason           string
}

func ParseUsagePricing(raw string) (UsagePricing, error) {
	var pricing UsagePricing
	if strings.TrimSpace(raw) == "" {
		return pricing, fmt.Errorf("GATEWAY_USAGE_PRICING_JSON is required")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pricing); err != nil {
		return pricing, fmt.Errorf("parse GATEWAY_USAGE_PRICING_JSON: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return pricing, fmt.Errorf("parse GATEWAY_USAGE_PRICING_JSON: %w", err)
	}
	if pricing.SchemaVersion == 0 {
		pricing.SchemaVersion = PricingSchemaV1
	}
	if pricing.SchemaVersion != PricingSchemaV1 && pricing.SchemaVersion != PricingSchemaV2 {
		return pricing, fmt.Errorf("pricing schema_version must be 1 or 2")
	}
	modelFields, fallbackPolicyPresent, err := usagePricingRawFields([]byte(raw))
	if err != nil {
		return pricing, fmt.Errorf("inspect GATEWAY_USAGE_PRICING_JSON: %w", err)
	}
	if _, err := time.Parse("2006-01-02", pricing.CatalogAsOf); err != nil {
		return pricing, fmt.Errorf("pricing catalog_as_of must be YYYY-MM-DD")
	}
	if _, err := time.Parse("2006-01-02", pricing.FXAsOf); err != nil {
		return pricing, fmt.Errorf("pricing fx_as_of must be YYYY-MM-DD")
	}
	if _, err := billing.ParseRate(pricing.USDCNYRate); err != nil {
		return pricing, fmt.Errorf("pricing usd_cny_rate: %w", err)
	}
	if len(pricing.Models) == 0 {
		return pricing, fmt.Errorf("pricing models must contain at least one exact model")
	}
	if len(pricing.Models) > maxUsagePricingModels {
		return pricing, fmt.Errorf("pricing models must contain at most %d entries", maxUsagePricingModels)
	}
	if pricing.SchemaVersion == PricingSchemaV1 {
		if fallbackPolicyPresent {
			return pricing, fmt.Errorf("pricing v1 must not define fallback_policy")
		}
	} else if err := validateV2FallbackPolicy(pricing.FallbackPolicy); err != nil {
		return pricing, err
	}
	for model, price := range pricing.Models {
		if !usagePricingModelPattern.MatchString(model) {
			return pricing, fmt.Errorf("pricing model names must match [A-Za-z0-9._:-]{1,128}")
		}
		if err := validateModelPricingFieldPresence(pricing.SchemaVersion, model, modelFields[model]); err != nil {
			return pricing, err
		}
		var err error
		if pricing.SchemaVersion == PricingSchemaV1 {
			err = validateV1ModelPricing(model, price)
		} else {
			err = validateV2ModelPricing(model, price)
		}
		if err != nil {
			return pricing, err
		}
	}
	return pricing, nil
}

func validateV2FallbackPolicy(policy PricingFallbackPolicy) error {
	if policy.UnknownServiceTier != FallbackMaxPublished {
		return fmt.Errorf("pricing v2 fallback_policy.unknown_service_tier must be %q", FallbackMaxPublished)
	}
	if policy.MissingPriceCombination != FallbackMaxPublished {
		return fmt.Errorf("pricing v2 fallback_policy.missing_price_combination must be %q", FallbackMaxPublished)
	}
	if policy.MissingCacheWriteTokens != FallbackAllUncachedAsWrite {
		return fmt.Errorf("pricing v2 fallback_policy.missing_cache_write_tokens must be %q", FallbackAllUncachedAsWrite)
	}
	return nil
}

func usagePricingRawFields(raw []byte) (map[string]map[string]json.RawMessage, bool, error) {
	var value struct {
		FallbackPolicy json.RawMessage                       `json:"fallback_policy"`
		Models         map[string]map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, err
	}
	return value.Models, value.FallbackPolicy != nil, nil
}

func validateModelPricingFieldPresence(schemaVersion int, model string, fields map[string]json.RawMessage) error {
	v1Fields := []string{"input_usd_per_million", "cached_input_usd_per_million", "output_usd_per_million"}
	v2Fields := []string{"cache_write_mode", "max_input_tokens", "long_context_threshold_tokens", "service_tiers"}
	for _, field := range v1Fields {
		if _, present := fields[field]; present && schemaVersion == PricingSchemaV2 {
			return fmt.Errorf("pricing v2 model %q must not define v1 field %q", model, field)
		}
	}
	for _, field := range v2Fields {
		if _, present := fields[field]; present && schemaVersion == PricingSchemaV1 {
			return fmt.Errorf("pricing v1 model %q must not define v2 field %q", model, field)
		}
	}
	return nil
}

func validateV1ModelPricing(model string, price ModelPricing) error {
	if price.CacheWriteMode != "" || price.MaxInputTokens != 0 ||
		price.LongContextThresholdTokens != 0 || len(price.ServiceTiers) != 0 {
		return fmt.Errorf("pricing v1 model %q must not mix v2 fields", model)
	}
	for field, value := range map[string]string{
		"input_usd_per_million":        price.InputUSDPerMillion,
		"cached_input_usd_per_million": price.CachedInputUSDPerMillion,
		"output_usd_per_million":       price.OutputUSDPerMillion,
	} {
		if _, err := billing.ParsePrice(value); err != nil {
			return fmt.Errorf("pricing model %q %s: %w", model, field, err)
		}
	}
	return nil
}

func validateV2ModelPricing(model string, price ModelPricing) error {
	if price.InputUSDPerMillion != "" || price.CachedInputUSDPerMillion != "" || price.OutputUSDPerMillion != "" {
		return fmt.Errorf("pricing v2 model %q must not mix v1 price fields", model)
	}
	if price.CacheWriteMode != CacheWriteSeparate && price.CacheWriteMode != CacheWriteIncludedInInput {
		return fmt.Errorf("pricing v2 model %q has invalid cache_write_mode", model)
	}
	if price.MaxInputTokens <= 0 || price.LongContextThresholdTokens <= 0 ||
		price.LongContextThresholdTokens > price.MaxInputTokens {
		return fmt.Errorf("pricing v2 model %q has invalid input-token boundaries", model)
	}
	if len(price.ServiceTiers) == 0 {
		return fmt.Errorf("pricing v2 model %q must define service_tiers", model)
	}
	for tier, contexts := range price.ServiceTiers {
		if tier != PricingTierStandard && tier != PricingTierFlex && tier != PricingTierFast {
			return fmt.Errorf("pricing v2 model %q has invalid service tier %q", model, tier)
		}
		if contexts.Short == nil && contexts.Long == nil {
			return fmt.Errorf("pricing v2 model %q tier %q has no published context price", model, tier)
		}
		if contexts.Short != nil {
			if err := validateTokenPricing(model, tier, ContextClassShort, price.CacheWriteMode, *contexts.Short); err != nil {
				return err
			}
		}
		if contexts.Long != nil {
			if err := validateTokenPricing(model, tier, ContextClassLong, price.CacheWriteMode, *contexts.Long); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateTokenPricing(model, tier, contextClass, cacheWriteMode string, price TokenPricing) error {
	fields := map[string]string{
		"input_usd_per_million":        price.InputUSDPerMillion,
		"cached_input_usd_per_million": price.CachedInputUSDPerMillion,
		"output_usd_per_million":       price.OutputUSDPerMillion,
	}
	for field, value := range fields {
		if _, err := billing.ParsePrice(value); err != nil {
			return fmt.Errorf("pricing v2 model %q tier %q context %q %s: %w", model, tier, contextClass, field, err)
		}
	}
	if cacheWriteMode == CacheWriteSeparate {
		if price.CacheWriteUSDPerMillion == nil {
			return fmt.Errorf("pricing v2 model %q tier %q context %q requires cache_write_usd_per_million", model, tier, contextClass)
		}
		if _, err := billing.ParsePrice(*price.CacheWriteUSDPerMillion); err != nil {
			return fmt.Errorf("pricing v2 model %q tier %q context %q cache_write_usd_per_million: %w", model, tier, contextClass, err)
		}
	} else if price.CacheWriteUSDPerMillion != nil {
		return fmt.Errorf("pricing v2 model %q tier %q context %q must omit cache_write_usd_per_million", model, tier, contextClass)
	}
	return nil
}

func (p UsagePricing) ModelSnapshot(model string) ([]byte, ModelPricing, bool, error) {
	rule, ok := p.Models[model]
	if !ok {
		return nil, ModelPricing{}, false, nil
	}
	if p.SchemaVersion != PricingSchemaV2 {
		return nil, rule, true, nil
	}
	encoded, err := json.Marshal(PricingSnapshot{
		SchemaVersion: PricingSchemaV2, FallbackPolicy: p.FallbackPolicy,
		Model: model, Rule: rule,
	})
	if err != nil {
		return nil, ModelPricing{}, false, fmt.Errorf("encode pricing snapshot: %w", err)
	}
	return encoded, rule, true, nil
}

func ParsePricingSnapshot(raw []byte) (PricingSnapshot, error) {
	var snapshot PricingSnapshot
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, fmt.Errorf("decode pricing snapshot: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return snapshot, fmt.Errorf("decode pricing snapshot: %w", err)
	}
	if snapshot.SchemaVersion != PricingSchemaV2 || !usagePricingModelPattern.MatchString(snapshot.Model) {
		return snapshot, fmt.Errorf("invalid pricing snapshot header")
	}
	var encoded struct {
		Rule map[string]json.RawMessage `json:"rule"`
	}
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return snapshot, fmt.Errorf("inspect pricing snapshot: %w", err)
	}
	if err := validateModelPricingFieldPresence(PricingSchemaV2, snapshot.Model, encoded.Rule); err != nil {
		return snapshot, err
	}
	if err := validateV2FallbackPolicy(snapshot.FallbackPolicy); err != nil {
		return snapshot, err
	}
	if err := validateV2ModelPricing(snapshot.Model, snapshot.Rule); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// NormalizeServiceTier converts request/response API spellings to catalog
// keys. The empty string is deliberately not normalized: a missing response
// tier triggers the conservative max-published policy.
func NormalizeServiceTier(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "default", "standard":
		return PricingTierStandard, true
	case "flex":
		return PricingTierFlex, true
	case "priority", "fast":
		return PricingTierFast, true
	default:
		return "", false
	}
}

func (p UsagePricing) ValidateRequestedServiceTier(model, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	tier, ok := NormalizeServiceTier(raw)
	if !ok {
		return fmt.Errorf("service tier %q is not configured", raw)
	}
	rule, ok := p.Models[model]
	if !ok {
		return fmt.Errorf("model %q is not configured", model)
	}
	if p.SchemaVersion == PricingSchemaV1 {
		if tier != PricingTierStandard {
			return fmt.Errorf("service tier %q is not represented by pricing v1", raw)
		}
		return nil
	}
	if _, ok := rule.ServiceTiers[tier]; !ok {
		return fmt.Errorf("service tier %q is not configured for model %q", raw, model)
	}
	return nil
}

func (s PricingSnapshot) Select(actualServiceTier string, inputTokens int64) (PricingDecision, error) {
	if inputTokens < 0 {
		return PricingDecision{}, fmt.Errorf("negative input tokens")
	}
	contextClass := ContextClassShort
	if inputTokens > s.Rule.LongContextThresholdTokens {
		contextClass = ContextClassLong
	}
	decision := PricingDecision{ContextClass: contextClass}
	var selected *TokenPricing
	overMaximum := inputTokens > s.Rule.MaxInputTokens
	if overMaximum {
		decision.FallbackReason = AppendFallbackReason(decision.FallbackReason, FallbackMissingPriceCombination)
	}
	missingTier := strings.TrimSpace(actualServiceTier) == ""
	unknownTier := false
	tier := ""
	if missingTier {
		decision.FallbackReason = AppendFallbackReason(decision.FallbackReason, FallbackMissingServiceTier)
	} else {
		var ok bool
		tier, ok = NormalizeServiceTier(actualServiceTier)
		if !ok {
			unknownTier = true
			decision.FallbackReason = AppendFallbackReason(decision.FallbackReason, FallbackUnknownServiceTier)
		}
	}
	if !overMaximum && !missingTier && !unknownTier {
		if contexts, configured := s.Rule.ServiceTiers[tier]; configured {
			selected = contextPrice(contexts, contextClass)
			if selected != nil {
				decision.PricingServiceTier = tier
			} else {
				decision.FallbackReason = AppendFallbackReason(decision.FallbackReason, FallbackMissingPriceCombination)
			}
		} else {
			decision.FallbackReason = AppendFallbackReason(decision.FallbackReason, FallbackMissingPriceCombination)
		}
	}
	if selected == nil {
		var ok bool
		if !overMaximum && (missingTier || unknownTier) {
			selected, ok = maxPublishedForContext(s.Rule, contextClass)
		}
		if !ok {
			decision.FallbackReason = AppendFallbackReason(decision.FallbackReason, FallbackMissingPriceCombination)
			selected, ok = maxPublished(s.Rule)
		}
		if !ok {
			return PricingDecision{}, fmt.Errorf("pricing snapshot contains no published prices")
		}
		decision.PricingServiceTier = PricingTierMaxPublished
	}
	decision.InputUSDPerMillion = selected.InputUSDPerMillion
	decision.CachedInputUSDPerMillion = selected.CachedInputUSDPerMillion
	decision.OutputUSDPerMillion = selected.OutputUSDPerMillion
	decision.CacheWriteUSDPerMillion = "0"
	if selected.CacheWriteUSDPerMillion != nil {
		decision.CacheWriteUSDPerMillion = *selected.CacheWriteUSDPerMillion
	}
	return decision, nil
}

func contextPrice(value ServiceTierPricing, contextClass string) *TokenPricing {
	if contextClass == ContextClassLong {
		return value.Long
	}
	return value.Short
}

func maxPublishedForContext(rule ModelPricing, contextClass string) (*TokenPricing, bool) {
	values := make([]TokenPricing, 0, len(rule.ServiceTiers))
	for _, tier := range rule.ServiceTiers {
		if price := contextPrice(tier, contextClass); price != nil {
			values = append(values, *price)
		}
	}
	return componentMaximum(values, rule.CacheWriteMode)
}

func maxPublished(rule ModelPricing) (*TokenPricing, bool) {
	values := make([]TokenPricing, 0, len(rule.ServiceTiers)*2)
	for _, tier := range rule.ServiceTiers {
		if tier.Short != nil {
			values = append(values, *tier.Short)
		}
		if tier.Long != nil {
			values = append(values, *tier.Long)
		}
	}
	return componentMaximum(values, rule.CacheWriteMode)
}

func componentMaximum(values []TokenPricing, cacheWriteMode string) (*TokenPricing, bool) {
	if len(values) == 0 {
		return nil, false
	}
	maximum := TokenPricing{
		InputUSDPerMillion: "0", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
	}
	cacheWrite := "0"
	for _, value := range values {
		maximum.InputUSDPerMillion = maxDecimal(maximum.InputUSDPerMillion, value.InputUSDPerMillion)
		maximum.CachedInputUSDPerMillion = maxDecimal(maximum.CachedInputUSDPerMillion, value.CachedInputUSDPerMillion)
		maximum.OutputUSDPerMillion = maxDecimal(maximum.OutputUSDPerMillion, value.OutputUSDPerMillion)
		if value.CacheWriteUSDPerMillion != nil {
			cacheWrite = maxDecimal(cacheWrite, *value.CacheWriteUSDPerMillion)
		}
	}
	if cacheWriteMode == CacheWriteSeparate {
		maximum.CacheWriteUSDPerMillion = &cacheWrite
	}
	return &maximum, true
}

func maxDecimal(left, right string) string {
	l, _ := billing.ParsePrice(left)
	r, _ := billing.ParsePrice(right)
	parts := func(value string) string {
		pieces := strings.SplitN(value, ".", 2)
		fraction := ""
		if len(pieces) == 2 {
			fraction = pieces[1]
		}
		return fmt.Sprintf("%018s%-012s", pieces[0], fraction)
	}
	if parts(r) > parts(l) {
		return r
	}
	return l
}

func AppendFallbackReason(existing, reason string) string {
	if reason == "" {
		return existing
	}
	set := make(map[string]struct{})
	for _, value := range strings.Split(existing, ",") {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	set[reason] = struct{}{}
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("unexpected trailing JSON value")
	}
	return err
}
