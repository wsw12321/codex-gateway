package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func builtInForOverrideTest(t *testing.T) UsagePricing {
	t.Helper()
	pricing, err := DefaultUsagePricing()
	if err != nil {
		t.Fatal(err)
	}
	return pricing
}

// overrideDocument returns a JSON object built from the built-in catalog,
// changed by edit, for table tests that work on the raw document.
func overrideDocument(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	var catalog map[string]any
	if err := json.Unmarshal(DefaultUsagePricingJSON(), &catalog); err != nil {
		t.Fatal(err)
	}
	models := catalog["models"].(map[string]any)
	document := map[string]any{
		"schema_version": 2,
		"models":         map[string]any{"gpt-6-sol": models["gpt-6-sol"]},
	}
	edit(document)
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func solStandardShort(document map[string]any) map[string]any {
	rule := document["models"].(map[string]any)["gpt-6-sol"].(map[string]any)
	return rule["service_tiers"].(map[string]any)["standard"].(map[string]any)["short"].(map[string]any)
}

func applyOverrideDocument(t *testing.T, raw []byte) (UsagePricing, error) {
	t.Helper()
	override, err := ParseUsagePricingOverride(raw)
	if err != nil {
		return UsagePricing{}, err
	}
	return ApplyUsagePricingOverride(builtInForOverrideTest(t), override)
}

func TestPricingOverrideChangesOnlyNamedPrices(t *testing.T) {
	base := builtInForOverrideTest(t)
	raw := overrideDocument(t, func(document map[string]any) {
		solStandardShort(document)["output_usd_per_million"] = "8.50"
		document["usd_cny_rate"], document["fx_as_of"] = "7.05", "2026-11-18"
	})
	merged, err := applyOverrideDocument(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Models["gpt-6-sol"].ServiceTiers["standard"].Short.OutputUSDPerMillion; got != "8.5" {
		t.Fatalf("overridden output price = %q", got)
	}
	if merged.USDCNYRate != "7.05" || merged.FXAsOf != "2026-11-18" || merged.CatalogAsOf != base.CatalogAsOf {
		t.Fatalf("unexpected merged metadata: %s %s %s", merged.USDCNYRate, merged.FXAsOf, merged.CatalogAsOf)
	}
	for model := range base.Models {
		before, err := base.ModelPriceStructureID(model)
		if err != nil {
			t.Fatal(err)
		}
		after, err := merged.ModelPriceStructureID(model)
		if err != nil || after != before {
			t.Fatalf("%s structure changed: %v", model, err)
		}
		if model != "gpt-6-sol" && !reflect.DeepEqual(base.Models[model], merged.Models[model]) {
			t.Fatalf("%s changed although the override does not name it", model)
		}
	}
	if fresh := builtInForOverrideTest(t); !reflect.DeepEqual(fresh, base) {
		t.Fatal("applying an override changed the built-in catalog")
	}
	if base.Models["gpt-6-sol"].ServiceTiers["standard"].Short.OutputUSDPerMillion == "8.5" {
		t.Fatal("applying an override mutated the base catalog")
	}
}

func TestPricingOverrideRejectsInvalidDocuments(t *testing.T) {
	setPrice := func(field, value string) func(map[string]any) {
		return func(document map[string]any) { solStandardShort(document)[field] = value }
	}
	tests := map[string][]byte{
		"empty":            []byte(" \n"),
		"not an object":    []byte(`[]`),
		"unknown field":    overrideDocument(t, func(d map[string]any) { d["catalog_as_of"] = "2026-11-18" }),
		"fallback policy":  overrideDocument(t, func(d map[string]any) { d["fallback_policy"] = map[string]any{} }),
		"trailing data":    append(overrideDocument(t, func(map[string]any) {}), []byte(` {}`)...),
		"missing schema":   overrideDocument(t, func(d map[string]any) { delete(d, "schema_version") }),
		"schema 1":         overrideDocument(t, func(d map[string]any) { d["schema_version"] = 1 }),
		"schema 3":         overrideDocument(t, func(d map[string]any) { d["schema_version"] = 3 }),
		"rate without day": overrideDocument(t, func(d map[string]any) { d["usd_cny_rate"] = "7.1" }),
		"day without rate": overrideDocument(t, func(d map[string]any) { d["fx_as_of"] = "2026-11-18" }),
		"bad day": overrideDocument(t, func(d map[string]any) {
			d["usd_cny_rate"], d["fx_as_of"] = "7.1", "18/11/2026"
		}),
		"zero rate": overrideDocument(t, func(d map[string]any) {
			d["usd_cny_rate"], d["fx_as_of"] = "0", "2026-11-18"
		}),
		"unknown model": overrideDocument(t, func(d map[string]any) {
			models := d["models"].(map[string]any)
			models["gpt-unpriced"] = models["gpt-6-sol"]
		}),
		"bad model name": overrideDocument(t, func(d map[string]any) {
			models := d["models"].(map[string]any)
			models["gpt 6"] = models["gpt-6-sol"]
		}),
		"internal model": overrideDocument(t, func(d map[string]any) {
			var catalog map[string]any
			_ = json.Unmarshal(DefaultUsagePricingJSON(), &catalog)
			d["models"] = map[string]any{InternalGovernanceModel: catalog["models"].(map[string]any)[InternalGovernanceModel]}
		}),
		"null rule": overrideDocument(t, func(d map[string]any) { d["models"].(map[string]any)["gpt-6-sol"] = nil }),
		"v1 field": overrideDocument(t, func(d map[string]any) {
			d["models"].(map[string]any)["gpt-6-sol"].(map[string]any)["input_usd_per_million"] = ""
		}),
		"zero price":    overrideDocument(t, setPrice("cached_input_usd_per_million", "0")),
		"negative":      overrideDocument(t, setPrice("input_usd_per_million", "-1")),
		"missing price": overrideDocument(t, func(d map[string]any) { delete(solStandardShort(d), "cache_write_usd_per_million") }),
		"extra price":   overrideDocument(t, setPrice("cache_write_5m_usd_per_million", "1")),
		"threshold": overrideDocument(t, func(d map[string]any) {
			d["models"].(map[string]any)["gpt-6-sol"].(map[string]any)["long_context_threshold_tokens"] = 200000
		}),
		"cache mode": overrideDocument(t, func(d map[string]any) {
			rule := d["models"].(map[string]any)["gpt-6-sol"].(map[string]any)
			rule["cache_write_mode"] = CacheWriteIncludedInInput
		}),
		"removed tier": overrideDocument(t, func(d map[string]any) {
			delete(d["models"].(map[string]any)["gpt-6-sol"].(map[string]any)["service_tiers"].(map[string]any), "fast")
		}),
		"removed context": overrideDocument(t, func(d map[string]any) {
			tiers := d["models"].(map[string]any)["gpt-6-sol"].(map[string]any)["service_tiers"].(map[string]any)
			delete(tiers["flex"].(map[string]any), "long")
		}),
		"duplicate model": []byte(strings.Replace(string(overrideDocument(t, func(map[string]any) {})),
			`"models":{"gpt-6-sol":`, `"models":{"gpt-6-sol":{},"gpt-6-sol":`, 1)),
		"duplicate price": []byte(strings.Replace(string(overrideDocument(t, func(map[string]any) {})),
			`"output_usd_per_million":"10"`, `"output_usd_per_million":"10","output_usd_per_million":"1"`, 1)),
		"oversized": append(overrideDocument(t, func(map[string]any) {}), []byte(strings.Repeat(" ", MaxUsagePricingDocumentBytes))...),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := applyOverrideDocument(t, raw); err == nil {
				t.Fatal("invalid override was accepted")
			}
		})
	}
}

func TestPricingOverrideExampleApplies(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pricing/override.example.json")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := applyOverrideDocument(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if merged.USDCNYRate == builtInForOverrideTest(t).USDCNYRate {
		t.Fatal("the example should demonstrate an exchange rate override")
	}
}

func writePricingOverride(t *testing.T, directory string, raw []byte) string {
	t.Helper()
	path := filepath.Join(directory, "override.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadUsagePricingSources(t *testing.T) {
	base := builtInForOverrideTest(t)
	builtIn, source, err := LoadUsagePricing("")
	if err != nil || !reflect.DeepEqual(builtIn, base) || source.Overridden() {
		t.Fatalf("empty path: %+v, %v", source, err)
	}
	baseDigest := source.Digest
	if len(baseDigest) != 64 {
		t.Fatalf("digest = %q", baseDigest)
	}
	directory := t.TempDir()
	missing := filepath.Join(directory, "override.json")
	if _, source, err := LoadUsagePricing(missing); err != nil || source.Overridden() || source.Digest != baseDigest {
		t.Fatalf("missing file: %+v, %v", source, err)
	}
	if _, _, err := LoadUsagePricing(filepath.Join(directory, "absent", "override.json")); err == nil {
		t.Fatal("a missing override directory must fail closed")
	}

	path := writePricingOverride(t, directory, overrideDocument(t, func(document map[string]any) {
		solStandardShort(document)["output_usd_per_million"] = "9"
	}))
	pricing, source, err := LoadUsagePricing(path)
	if err != nil {
		t.Fatal(err)
	}
	if source.OverridePath != path || !reflect.DeepEqual(source.OverrideModels, []string{"gpt-6-sol"}) ||
		source.OverrideFX || source.Digest == baseDigest || len(source.Warnings) != 0 ||
		source.CatalogAsOf != base.CatalogAsOf || source.ModelCount != len(base.Models) {
		t.Fatalf("override source: %+v", source)
	}
	if digest, err := UsagePricingDigest(pricing); err != nil || digest != source.Digest {
		t.Fatalf("digest is not reproducible: %s, %v", digest, err)
	}

	// Entries that change nothing would pin today's prices; they only warn.
	writePricingOverride(t, directory, overrideDocument(t, func(document map[string]any) {
		document["usd_cny_rate"], document["fx_as_of"] = base.USDCNYRate, base.FXAsOf
	}))
	if _, source, err := LoadUsagePricing(path); err != nil || len(source.Warnings) != 2 || source.Digest != baseDigest {
		t.Fatalf("redundant override: %+v, %v", source, err)
	}
	writePricingOverride(t, directory, []byte(`{"schema_version":2}`))
	if _, source, err := LoadUsagePricing(path); err != nil || len(source.Warnings) != 1 {
		t.Fatalf("empty override: %+v, %v", source, err)
	}

	writePricingOverride(t, directory, []byte(`{"schema_version":2,"models":{"gpt-unpriced":{}}}`))
	if _, _, err := LoadUsagePricing(path); err == nil {
		t.Fatal("invalid override file was accepted")
	}
	writePricingOverride(t, directory, append([]byte(`{"schema_version":2}`), make([]byte, MaxUsagePricingDocumentBytes)...))
	if _, _, err := LoadUsagePricing(path); err == nil {
		t.Fatal("oversized override file was accepted")
	}
}

func TestLoadUsagePricingRejectsNonRegularFiles(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "override.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadUsagePricing(filepath.Join(directory, "override.json")); err == nil {
		t.Fatal("a directory was accepted as the override file")
	}
	target := writePricingOverride(t, t.TempDir(), []byte(`{"schema_version":2}`))
	link := filepath.Join(t.TempDir(), "override.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symbolic links are unavailable:", err)
	}
	if _, _, err := LoadUsagePricing(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("a symbolic link was accepted: %v", err)
	}
}

func TestLoadUsagePricingRejectsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	path := writePricingOverride(t, t.TempDir(), []byte(`{"schema_version":2}`))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadUsagePricing(path); err == nil || !strings.Contains(err.Error(), "chmod 0755") {
		t.Fatalf("an unreadable override was accepted: %v", err)
	}
}

func legacyDocument(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	var catalog map[string]any
	if err := json.Unmarshal(DefaultUsagePricingJSON(), &catalog); err != nil {
		t.Fatal(err)
	}
	edit(catalog)
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func legacyRule(catalog map[string]any, model string) map[string]any {
	return catalog["models"].(map[string]any)[model].(map[string]any)
}

func TestMigrateLegacyUsagePricing(t *testing.T) {
	base := builtInForOverrideTest(t)
	identical, err := MigrateLegacyUsagePricing(base, DefaultUsagePricingJSON())
	if err != nil {
		t.Fatal(err)
	}
	if identical.Override != nil || identical.FXChanged || identical.FXDateOnly || identical.LegacyOlder ||
		len(identical.PriceChanged)+len(identical.StructureChanged)+len(identical.NotInBuiltIn)+len(identical.NewInBuiltIn) != 0 {
		t.Fatalf("identical legacy catalog produced changes: %+v", identical)
	}

	raw := legacyDocument(t, func(catalog map[string]any) {
		catalog["catalog_as_of"] = "2026-09-01"
		catalog["usd_cny_rate"], catalog["fx_as_of"] = "7.10", "2026-09-01"
		tiers := legacyRule(catalog, "gpt-5.4-mini")["service_tiers"].(map[string]any)
		tiers["standard"].(map[string]any)["short"].(map[string]any)["output_usd_per_million"] = "4.000"
		legacyRule(catalog, "gpt-6-sol")["long_context_threshold_tokens"] = 200000
		legacyRule(catalog, "claude-opus-5-5")["service_tiers"].(map[string]any)["standard"].(map[string]any)["short"].(map[string]any)["cached_input_usd_per_million"] = "0"
		models := catalog["models"].(map[string]any)
		models["gpt-legacy"] = models["gpt-5.4"]
		models[InternalGovernanceModel].(map[string]any)["service_tiers"].(map[string]any)["standard"].(map[string]any)["short"].(map[string]any)["output_usd_per_million"] = "1"
		delete(models, "gpt-6-astra")
	})
	migration, err := MigrateLegacyUsagePricing(base, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(migration.PriceChanged, []string{"gpt-5.4-mini"}) ||
		!reflect.DeepEqual(migration.StructureChanged, []string{"claude-opus-5-5", "gpt-6-sol"}) ||
		!reflect.DeepEqual(migration.NotInBuiltIn, []string{"gpt-legacy"}) ||
		!reflect.DeepEqual(migration.NewInBuiltIn, []string{"gpt-6-astra"}) ||
		!migration.FXChanged || migration.FXDateOnly || !migration.LegacyOlder {
		t.Fatalf("unexpected migration report: %+v", migration)
	}
	override := migration.Override
	if override == nil || override.USDCNYRate != "7.1" || override.FXAsOf != "2026-09-01" ||
		!reflect.DeepEqual(sortedModelNames(override.Models), []string{"gpt-5.4-mini"}) {
		t.Fatalf("unexpected override: %+v", override)
	}
	encoded, err := json.Marshal(override)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := applyOverrideDocument(t, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Models["gpt-5.4-mini"].ServiceTiers["standard"].Short.OutputUSDPerMillion; got != "4" {
		t.Fatalf("carried price = %q", got)
	}

	dateOnly, err := MigrateLegacyUsagePricing(base, legacyDocument(t, func(catalog map[string]any) {
		catalog["usd_cny_rate"], catalog["fx_as_of"] = "7.2000", "2026-01-01"
	}))
	if err != nil || dateOnly.Override != nil || !dateOnly.FXDateOnly || dateOnly.FXChanged {
		t.Fatalf("an equal rate with another date must not be carried: %+v, %v", dateOnly, err)
	}

	for name, raw := range map[string][]byte{
		"v1":      []byte(`{"catalog_as_of":"2026-08-01","fx_as_of":"2026-08-01","usd_cny_rate":"7.20","models":{"gpt-test":{"input_usd_per_million":"1","cached_input_usd_per_million":"0.1","output_usd_per_million":"2"}}}`),
		"invalid": []byte(`{"schema_version":2}`),
		"garbage": []byte(`GATEWAY_USAGE_PRICING_JSON={}`),
	} {
		if _, err := MigrateLegacyUsagePricing(base, raw); err == nil {
			t.Fatalf("%s legacy catalog was accepted", name)
		}
	}
}
