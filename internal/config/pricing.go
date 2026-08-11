package config

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"
)

const (
	maxUsagePricingDecimalLength = 40
	maxUsagePricingValue         = "1000000000"
	maxUsagePricingModels        = 1000
)

var (
	plainDecimalPattern      = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	usagePricingModelPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// UsagePricing is an operator-supplied, immutable pricing snapshot. Decimal
// strings are retained verbatim and parsed as rationals when estimates are
// calculated, so monetary arithmetic never passes through float64.
type UsagePricing struct {
	CatalogAsOf string                  `json:"catalog_as_of"`
	FXAsOf      string                  `json:"fx_as_of"`
	USDCNYRate  string                  `json:"usd_cny_rate"`
	Models      map[string]ModelPricing `json:"models"`
}

type ModelPricing struct {
	InputUSDPerMillion       string `json:"input_usd_per_million"`
	CachedInputUSDPerMillion string `json:"cached_input_usd_per_million"`
	OutputUSDPerMillion      string `json:"output_usd_per_million"`
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
	if _, err := time.Parse("2006-01-02", pricing.CatalogAsOf); err != nil {
		return pricing, fmt.Errorf("pricing catalog_as_of must be YYYY-MM-DD")
	}
	if _, err := time.Parse("2006-01-02", pricing.FXAsOf); err != nil {
		return pricing, fmt.Errorf("pricing fx_as_of must be YYYY-MM-DD")
	}
	if err := validPositiveDecimal(pricing.USDCNYRate, false); err != nil {
		return pricing, fmt.Errorf("pricing usd_cny_rate: %w", err)
	}
	if len(pricing.Models) == 0 {
		return pricing, fmt.Errorf("pricing models must contain at least one exact model")
	}
	if len(pricing.Models) > maxUsagePricingModels {
		return pricing, fmt.Errorf("pricing models must contain at most %d entries", maxUsagePricingModels)
	}
	for model, price := range pricing.Models {
		if !usagePricingModelPattern.MatchString(model) {
			return pricing, fmt.Errorf("pricing model names must match [A-Za-z0-9._:-]{1,128}")
		}
		for field, value := range map[string]string{
			"input_usd_per_million":        price.InputUSDPerMillion,
			"cached_input_usd_per_million": price.CachedInputUSDPerMillion,
			"output_usd_per_million":       price.OutputUSDPerMillion,
		} {
			if err := validPositiveDecimal(value, true); err != nil {
				return pricing, fmt.Errorf("pricing model %q %s: %w", model, field, err)
			}
		}
	}
	return pricing, nil
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

func validPositiveDecimal(value string, allowZero bool) error {
	if len(value) == 0 || len(value) > maxUsagePricingDecimalLength || !plainDecimalPattern.MatchString(value) {
		return fmt.Errorf("must be a plain non-negative decimal string")
	}
	r, ok := new(big.Rat).SetString(value)
	maximum, _ := new(big.Rat).SetString(maxUsagePricingValue)
	if !ok || r.Sign() < 0 || (!allowZero && r.Sign() == 0) || r.Cmp(maximum) > 0 {
		return fmt.Errorf("must be a bounded non-negative decimal string")
	}
	return nil
}
