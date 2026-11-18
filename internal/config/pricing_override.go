package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/billing"
)

// MaxUsagePricingDocumentBytes bounds the override file and the legacy
// catalog accepted by the migration command.
const MaxUsagePricingDocumentBytes = 4 << 20

// UsagePricingOverride is the optional server-local deviation from the
// built-in catalog. It may only change prices and the fixed USD/CNY rate; the
// model set, every rule's structure, catalog_as_of and the fallback policy
// stay as built. Each listed model replaces the complete built-in rule.
type UsagePricingOverride struct {
	SchemaVersion int                     `json:"schema_version"`
	USDCNYRate    string                  `json:"usd_cny_rate,omitempty"`
	FXAsOf        string                  `json:"fx_as_of,omitempty"`
	Models        map[string]ModelPricing `json:"models,omitempty"`
}

// UsagePricingSource describes where the effective catalog came from. It holds
// names, dates and a digest, never prices, so it is safe to log.
type UsagePricingSource struct {
	CatalogAsOf    string
	ModelCount     int
	OverridePath   string
	OverrideModels []string
	OverrideFX     bool
	// Digest is the SHA-256 of the effective catalog. Gateway instances that
	// share a database should report the same value.
	Digest   string
	Warnings []string
}

// Overridden reports whether a server-local override file was applied.
func (s UsagePricingSource) Overridden() bool {
	return s.OverridePath != ""
}

// ParseUsagePricingOverride strictly decodes an override document. Whether its
// models and structures match a catalog is checked by ApplyUsagePricingOverride.
func ParseUsagePricingOverride(raw []byte) (UsagePricingOverride, error) {
	var override UsagePricingOverride
	if len(raw) > MaxUsagePricingDocumentBytes {
		return override, fmt.Errorf("pricing override must be at most %d bytes", MaxUsagePricingDocumentBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return override, errors.New("pricing override is empty; delete the file to use the built-in catalog")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&override); err != nil {
		return override, fmt.Errorf("parse pricing override: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return override, fmt.Errorf("parse pricing override: %w", err)
	}
	// A hand-edited file must not silently keep only the last of two values.
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return override, fmt.Errorf("parse pricing override: %w", err)
	}
	if override.SchemaVersion == 0 {
		return override, errors.New("pricing override must set schema_version")
	}
	if (override.USDCNYRate == "") != (override.FXAsOf == "") {
		return override, errors.New("pricing override must set usd_cny_rate and fx_as_of together")
	}
	if override.USDCNYRate != "" {
		if _, err := time.Parse("2006-01-02", override.FXAsOf); err != nil {
			return override, errors.New("pricing override fx_as_of must be YYYY-MM-DD")
		}
		rate, err := billing.ParseRate(override.USDCNYRate)
		if err != nil {
			return override, fmt.Errorf("pricing override usd_cny_rate: %w", err)
		}
		override.USDCNYRate = rate
	}
	fields, _, err := usagePricingRawFields(raw)
	if err != nil {
		return override, fmt.Errorf("inspect pricing override: %w", err)
	}
	for model := range override.Models {
		if !usagePricingModelPattern.MatchString(model) {
			return override, fmt.Errorf("pricing override model names must match [A-Za-z0-9._:-]{1,128}")
		}
		if fields[model] == nil {
			return override, fmt.Errorf("pricing override model %q must be a complete pricing rule", model)
		}
		if err := validateModelPricingFieldPresence(override.SchemaVersion, model, fields[model]); err != nil {
			return override, fmt.Errorf("pricing override: %w", err)
		}
	}
	return override, nil
}

// ApplyUsagePricingOverride returns base with the override applied. It fails
// closed when the override names a model the catalog no longer prices, or a
// rule whose structure (cache mode, limits, tiers, contexts) differs from the
// catalog's, so a repository update can never be silently mixed with a stale
// local rule. Every overriding price must be positive: a free model is a
// built-in decision, and the Owner can still set an audited zero price in the
// dashboard.
func ApplyUsagePricingOverride(base UsagePricing, override UsagePricingOverride) (UsagePricing, error) {
	if override.SchemaVersion != base.SchemaVersion {
		return UsagePricing{}, fmt.Errorf("pricing override schema_version %d does not match the built-in catalog (%d); rewrite the override for the current catalog",
			override.SchemaVersion, base.SchemaVersion)
	}
	result := base
	result.Models = make(map[string]ModelPricing, len(base.Models))
	for model, rule := range base.Models {
		result.Models[model] = rule
	}
	for _, model := range sortedModelNames(override.Models) {
		if model == InternalGovernanceModel {
			return UsagePricing{}, fmt.Errorf("pricing override must not change %q; it is fixed at zero", model)
		}
		if _, ok := base.Models[model]; !ok {
			return UsagePricing{}, fmt.Errorf("pricing override names model %q, which the built-in catalog does not price; remove it from the override", model)
		}
		rule, err := base.ValidateModelPriceOverride(model, override.Models[model])
		if err != nil {
			return UsagePricing{}, fmt.Errorf("pricing override for %q must keep the built-in structure and only change prices (%v); compare it with `pricing show -builtin -model %s`", model, err, model)
		}
		if err := requirePositivePrices(model, rule); err != nil {
			return UsagePricing{}, err
		}
		result.Models[model] = rule
	}
	if override.USDCNYRate != "" {
		result.USDCNYRate, result.FXAsOf = override.USDCNYRate, override.FXAsOf
	}
	// Revalidate the merged catalog exactly as a deployment document.
	encoded, err := json.Marshal(result)
	if err != nil {
		return UsagePricing{}, fmt.Errorf("encode merged pricing: %w", err)
	}
	merged, err := ParseUsagePricing(string(encoded))
	if err != nil {
		return UsagePricing{}, fmt.Errorf("validate merged pricing: %w", err)
	}
	return merged, nil
}

// LoadUsagePricing returns the built-in catalog with the override file at path
// applied. An empty path selects the built-in catalog alone. With a path, its
// directory must exist (it is the deployment's read-only mount); a missing file
// in it selects the built-in catalog. Any other problem, including an
// unreadable file or a symbolic link, fails closed.
func LoadUsagePricing(path string) (UsagePricing, UsagePricingSource, error) {
	base, err := DefaultUsagePricing()
	if err != nil {
		return UsagePricing{}, UsagePricingSource{}, err
	}
	pricing, source, err := loadUsagePricingOverride(base, strings.TrimSpace(path))
	if err != nil {
		return UsagePricing{}, UsagePricingSource{}, err
	}
	source.CatalogAsOf, source.ModelCount = base.CatalogAsOf, len(base.Models)
	if source.Digest, err = UsagePricingDigest(pricing); err != nil {
		return UsagePricing{}, UsagePricingSource{}, err
	}
	return pricing, source, nil
}

func loadUsagePricingOverride(base UsagePricing, path string) (UsagePricing, UsagePricingSource, error) {
	var source UsagePricingSource
	if path == "" {
		return base, source, nil
	}
	const permissions = "the directory must be enterable and the file readable by the Gateway (uid 10001): chmod 0755 the directory and 0644 the file; prices are not secret"
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return UsagePricing{}, source, fmt.Errorf("pricing override directory %s: %w (is deploy/pricing mounted?)", filepath.Dir(path), err)
	}
	if !directory.IsDir() {
		return UsagePricing{}, source, fmt.Errorf("pricing override directory %s is not a directory", filepath.Dir(path))
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return base, source, nil
	}
	if err != nil {
		return UsagePricing{}, source, fmt.Errorf("inspect pricing override %s: %w; %s", path, err, permissions)
	}
	if !info.Mode().IsRegular() {
		return UsagePricing{}, source, fmt.Errorf("pricing override %s must be a regular file, not a link or directory", path)
	}
	file, err := os.Open(path) // #nosec G304 -- operator-configured, read-only mounted pricing override path
	if err != nil {
		return UsagePricing{}, source, fmt.Errorf("open pricing override %s: %w; %s", path, err, permissions)
	}
	defer file.Close()
	// Recheck the opened file, so a swap after Lstat cannot change what is read.
	opened, err := file.Stat()
	if err != nil {
		return UsagePricing{}, source, fmt.Errorf("inspect pricing override %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return UsagePricing{}, source, fmt.Errorf("pricing override %s changed while it was opened", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxUsagePricingDocumentBytes+1))
	if err != nil {
		return UsagePricing{}, source, fmt.Errorf("read pricing override %s: %w", path, err)
	}
	override, err := ParseUsagePricingOverride(raw)
	if err != nil {
		return UsagePricing{}, source, fmt.Errorf("%s: %w", path, err)
	}
	merged, err := ApplyUsagePricingOverride(base, override)
	if err != nil {
		return UsagePricing{}, source, fmt.Errorf("%s: %w", path, err)
	}
	source.OverridePath = path
	source.OverrideModels = sortedModelNames(override.Models)
	source.OverrideFX = override.USDCNYRate != ""
	source.Warnings = overrideWarnings(base, merged, override)
	return merged, source, nil
}

// overrideWarnings reports entries that change nothing. They are harmless now
// but would silently pin today's prices once the built-in catalog moves on.
func overrideWarnings(base, merged UsagePricing, override UsagePricingOverride) []string {
	var warnings []string
	if override.USDCNYRate == "" && len(override.Models) == 0 {
		warnings = append(warnings, "pricing override changes nothing; delete it to use the built-in catalog")
	}
	for _, model := range sortedModelNames(override.Models) {
		current, err := NormalizeModelPrice(base.SchemaVersion, model, base.Models[model])
		if err == nil && reflect.DeepEqual(current, merged.Models[model]) {
			warnings = append(warnings, fmt.Sprintf("pricing override for %q equals the built-in prices; remove it so later catalog updates apply", model))
		}
	}
	if override.USDCNYRate != "" {
		rate, err := billing.ParseRate(base.USDCNYRate)
		if err == nil && rate == override.USDCNYRate && base.FXAsOf == override.FXAsOf {
			warnings = append(warnings, "pricing override exchange rate equals the built-in rate; remove it so later catalog updates apply")
		}
	}
	return warnings
}

// UsagePricingDigest returns the SHA-256 of the canonical encoding of the
// effective catalog. Decimals are canonicalized first, so instances whose
// digests match price identically however their files spell the numbers.
func UsagePricingDigest(pricing UsagePricing) (string, error) {
	canonical := pricing
	canonical.Models = make(map[string]ModelPricing, len(pricing.Models))
	for model, rule := range pricing.Models {
		normalized, err := NormalizeModelPrice(pricing.SchemaVersion, model, rule)
		if err != nil {
			return "", fmt.Errorf("canonicalize pricing digest: %w", err)
		}
		canonical.Models[model] = normalized
	}
	rate, err := billing.ParseRate(pricing.USDCNYRate)
	if err != nil {
		return "", fmt.Errorf("canonicalize pricing digest: %w", err)
	}
	canonical.USDCNYRate = rate
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode pricing digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func requirePositivePrices(model string, rule ModelPricing) error {
	for tier, contexts := range rule.ServiceTiers {
		for contextClass, price := range map[string]*TokenPricing{ContextClassShort: contexts.Short, ContextClassLong: contexts.Long} {
			if price == nil {
				continue
			}
			values := map[string]*string{
				"input_usd_per_million":          &price.InputUSDPerMillion,
				"cached_input_usd_per_million":   &price.CachedInputUSDPerMillion,
				"output_usd_per_million":         &price.OutputUSDPerMillion,
				"cache_write_usd_per_million":    price.CacheWriteUSDPerMillion,
				"cache_write_5m_usd_per_million": price.CacheWrite5mUSDPerMillion,
				"cache_write_1h_usd_per_million": price.CacheWrite1hUSDPerMillion,
			}
			for field, value := range values {
				if value == nil {
					continue
				}
				if _, err := billing.ParsePositivePrice(*value); err != nil {
					return fmt.Errorf("pricing override for %q tier %q context %q %s must be positive", model, tier, contextClass, field)
				}
			}
		}
	}
	return nil
}

// rejectDuplicateJSONKeys scans an already decoded document for an object
// that repeats a key, which encoding/json would otherwise resolve silently.
func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return scanJSONValue(decoder, "$")
}

func scanJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, _ := keyToken.(string)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate key %q in %s", key, path)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	_, err = decoder.Token()
	return err
}

// LegacyPricingMigration is the comparison of a retired single-line pricing
// catalog with the built-in one.
type LegacyPricingMigration struct {
	// Override is nil when the built-in catalog already reproduces every
	// carried price and the exchange rate.
	Override *UsagePricingOverride
	// PriceChanged lists models whose legacy prices differ but whose structure
	// matches; they are carried into Override.
	PriceChanged []string
	// StructureChanged lists models whose legacy rule has a different
	// structure or a zero price, which an override cannot carry. The built-in
	// rule wins; prices must be reviewed again.
	StructureChanged []string
	// NotInBuiltIn lists legacy models the built-in catalog does not price.
	// They are dropped and become unavailable.
	NotInBuiltIn []string
	// NewInBuiltIn lists manageable built-in models that the legacy catalog
	// lacked. They become available to users after the upgrade.
	NewInBuiltIn []string
	// FXChanged reports that the legacy USD/CNY rate differs and is carried.
	FXChanged bool
	// FXDateOnly reports that only fx_as_of differs; nothing is carried.
	FXDateOnly bool
	// LegacyCatalogAsOf and BuiltInCatalogAsOf are the two review dates.
	LegacyCatalogAsOf, BuiltInCatalogAsOf string
	// LegacyOlder reports a legacy catalog reviewed before the built-in one:
	// its differing prices may be outdated official prices, not choices.
	LegacyOlder bool
}

// MigrateLegacyUsagePricing compares a retired GATEWAY_USAGE_PRICING_JSON
// document with the built-in catalog and returns the minimal override that
// keeps every operator-specific price that can still be expressed.
func MigrateLegacyUsagePricing(builtIn UsagePricing, raw []byte) (LegacyPricingMigration, error) {
	var result LegacyPricingMigration
	if len(raw) > MaxUsagePricingDocumentBytes {
		return result, fmt.Errorf("legacy pricing must be at most %d bytes", MaxUsagePricingDocumentBytes)
	}
	legacy, err := ParseUsagePricing(string(raw))
	if err != nil {
		return result, fmt.Errorf("legacy pricing: %w", err)
	}
	if legacy.SchemaVersion != builtIn.SchemaVersion {
		return result, fmt.Errorf("legacy pricing uses schema_version %d but the built-in catalog uses %d; convert it manually", legacy.SchemaVersion, builtIn.SchemaVersion)
	}
	result.LegacyCatalogAsOf, result.BuiltInCatalogAsOf = legacy.CatalogAsOf, builtIn.CatalogAsOf
	// Both dates are validated YYYY-MM-DD strings, so they compare in order.
	result.LegacyOlder = legacy.CatalogAsOf < builtIn.CatalogAsOf
	override := UsagePricingOverride{SchemaVersion: builtIn.SchemaVersion, Models: map[string]ModelPricing{}}
	for _, model := range sortedModelNames(legacy.Models) {
		if model == InternalGovernanceModel {
			continue
		}
		builtInRule, ok := builtIn.Models[model]
		if !ok {
			result.NotInBuiltIn = append(result.NotInBuiltIn, model)
			continue
		}
		rule, err := builtIn.ValidateModelPriceOverride(model, legacy.Models[model])
		if err != nil || requirePositivePrices(model, rule) != nil {
			result.StructureChanged = append(result.StructureChanged, model)
			continue
		}
		current, err := NormalizeModelPrice(builtIn.SchemaVersion, model, builtInRule)
		if err != nil {
			return result, fmt.Errorf("built-in pricing for %q: %w", model, err)
		}
		if !reflect.DeepEqual(rule, current) {
			result.PriceChanged = append(result.PriceChanged, model)
			override.Models[model] = rule
		}
	}
	for _, model := range builtIn.ManageableModelNames() {
		if _, ok := legacy.Models[model]; !ok {
			result.NewInBuiltIn = append(result.NewInBuiltIn, model)
		}
	}
	legacyRate, err := billing.ParseRate(legacy.USDCNYRate)
	if err != nil {
		return result, fmt.Errorf("legacy pricing usd_cny_rate: %w", err)
	}
	builtInRate, err := billing.ParseRate(builtIn.USDCNYRate)
	if err != nil {
		return result, fmt.Errorf("built-in pricing usd_cny_rate: %w", err)
	}
	if legacyRate != builtInRate {
		result.FXChanged = true
		override.USDCNYRate, override.FXAsOf = legacyRate, legacy.FXAsOf
	} else if legacy.FXAsOf != builtIn.FXAsOf {
		result.FXDateOnly = true
	}
	if len(override.Models) == 0 {
		override.Models = nil
	}
	if override.Models == nil && !result.FXChanged {
		return result, nil
	}
	// The generated document must load exactly as an operator's file would.
	encoded, err := json.Marshal(override)
	if err != nil {
		return result, fmt.Errorf("encode pricing override: %w", err)
	}
	parsed, err := ParseUsagePricingOverride(encoded)
	if err != nil {
		return result, fmt.Errorf("validate generated pricing override: %w", err)
	}
	if _, err := ApplyUsagePricingOverride(builtIn, parsed); err != nil {
		return result, fmt.Errorf("validate generated pricing override: %w", err)
	}
	result.Override = &override
	return result, nil
}

func sortedModelNames(models map[string]ModelPricing) []string {
	names := make([]string, 0, len(models))
	for model := range models {
		names = append(names, model)
	}
	sort.Strings(names)
	return names
}
