package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LegacyUsagePricingEnv names the retired deployment variable that used to
// carry the complete pricing catalog. The catalog is now built into the binary.
const LegacyUsagePricingEnv = "GATEWAY_USAGE_PRICING_JSON"

// UsagePricingOverrideEnv names the optional server-local override file.
const UsagePricingOverrideEnv = "GATEWAY_USAGE_PRICING_OVERRIDE_FILE"

// The reviewed pricing catalog is maintained in the repository and compiled
// into every Gateway binary, so a deployment needs no pricing configuration.
//
//go:embed data/pricing.json
var defaultUsagePricingJSON []byte

// DefaultUsagePricingJSON returns a copy of the built-in catalog document.
func DefaultUsagePricingJSON() []byte {
	return append([]byte(nil), defaultUsagePricingJSON...)
}

// DefaultUsagePricing parses and validates the built-in catalog. Every call
// returns an independent value.
func DefaultUsagePricing() (UsagePricing, error) {
	pricing, err := ParseUsagePricing(string(defaultUsagePricingJSON))
	if err != nil {
		return UsagePricing{}, fmt.Errorf("built-in pricing catalog: %w", err)
	}
	return pricing, nil
}

// RetiredCodexModels lists Codex IDs removed from the pinned CPA catalog. Their
// usage and ledger history remains readable, but they cannot be priced again.
func RetiredCodexModels() []string {
	return []string{"gpt-5.5"}
}

// rejectLegacyUsagePricing refuses a deployment that still carries the retired
// pricing variable. Ignoring it would silently replace any operator-specific
// prices with the built-in catalog.
func rejectLegacyUsagePricing() error {
	if strings.TrimSpace(os.Getenv(LegacyUsagePricingEnv)) != "" {
		return errors.New(LegacyUsagePricingEnv + " is no longer supported: prices are built into the Gateway image. " +
			"Save the value with ./scripts/pricing.sh export-legacy, delete the line from .env and convert it with " +
			"./scripts/pricing.sh migrate-env (docs/operations.md, 迁移到内置价格目录)")
	}
	return nil
}

// validateRetiredCodexPricing keeps a stale pricing catalog from reactivating a
// model that the pinned CPA no longer serves at the next catalog sync.
func (c Config) validateRetiredCodexPricing() error {
	for _, model := range RetiredCodexModels() {
		if _, ok := c.UsagePricing.Models[model]; ok {
			return fmt.Errorf("pricing catalog contains retired CPA model %q", model)
		}
	}
	return nil
}
