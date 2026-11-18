package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
)

func runPricingForTest(t *testing.T, overridePath, stdin string, arguments ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		if name == config.UsagePricingOverrideEnv {
			return overridePath
		}
		return ""
	}
	err := runPricing(arguments, strings.NewReader(stdin), &stdout, &stderr, getenv)
	return stdout.String(), stderr.String(), err
}

func TestPricingShowPrintsCatalogAndModelOverrideStart(t *testing.T) {
	stdout, _, err := runPricingForTest(t, "", "", "show")
	if err != nil {
		t.Fatal(err)
	}
	var catalog config.UsagePricing
	if err := json.Unmarshal([]byte(stdout), &catalog); err != nil {
		t.Fatal(err)
	}
	builtIn, err := config.DefaultUsagePricing()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != len(builtIn.Models) || catalog.CatalogAsOf != builtIn.CatalogAsOf {
		t.Fatal("show did not print the built-in catalog")
	}

	// A broken live override must not block authoring from the built-in rule.
	broken := filepath.Join(t.TempDir(), "override.json")
	if err := os.WriteFile(broken, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if stdout, _, err := runPricingForTest(t, broken, "", "show"); err == nil || stdout != "" {
		t.Fatalf("show accepted a broken override: %q", stdout)
	}
	stdout, _, err = runPricingForTest(t, broken, "", "show", "-builtin", "-model", "gpt-6-sol")
	if err != nil {
		t.Fatal(err)
	}
	override, err := config.ParseUsagePricingOverride([]byte(stdout))
	if err != nil {
		t.Fatal(err)
	}
	if len(override.Models) != 1 || override.USDCNYRate != "" {
		t.Fatalf("unexpected override start: %+v", override)
	}
	if _, err := config.ApplyUsagePricingOverride(builtIn, override); err != nil {
		t.Fatalf("show -model output is not a valid override: %v", err)
	}
	for _, model := range []string{"gpt-unpriced", config.InternalGovernanceModel} {
		if stdout, _, err := runPricingForTest(t, "", "", "show", "-model", model); err == nil || stdout != "" {
			t.Fatalf("show -model %s: %q, %v", model, stdout, err)
		}
	}
}

func TestPricingCheckReportsOverride(t *testing.T) {
	stdout, _, err := runPricingForTest(t, "", "", "check")
	if err != nil || !strings.Contains(stdout, "使用内置目录") || !strings.Contains(stdout, "校验通过") {
		t.Fatalf("check without override: %q, %v", stdout, err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "override.json")
	stdout, _, err = runPricingForTest(t, path, "", "check")
	if err != nil || !strings.Contains(stdout, "不存在，使用内置目录") {
		t.Fatalf("check with missing override: %q, %v", stdout, err)
	}
	example, err := os.ReadFile("../../deploy/pricing/override.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runPricingForTest(t, path, "", "check")
	if err != nil || !strings.Contains(stdout, "覆盖价格的模型：gpt-5.4-mini") ||
		!strings.Contains(stdout, "覆盖汇率") || !strings.Contains(stdout, "sha256") {
		t.Fatalf("check with example override: %q, %v", stdout, err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"models":{"gpt-unpriced":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if stdout, _, err := runPricingForTest(t, path, "", "check"); err == nil || stdout != "" {
		t.Fatalf("check accepted an invalid override: %q", stdout)
	}
}

func TestPricingMigrateEnv(t *testing.T) {
	stdout, stderr, err := runPricingForTest(t, "", string(config.DefaultUsagePricingJSON()), "migrate-env")
	if err != nil || stdout != "" || !strings.Contains(stderr, "无需覆盖文件") {
		t.Fatalf("identical legacy catalog: stdout %q, stderr %q, %v", stdout, stderr, err)
	}

	var catalog map[string]any
	if err := json.Unmarshal(config.DefaultUsagePricingJSON(), &catalog); err != nil {
		t.Fatal(err)
	}
	catalog["usd_cny_rate"] = "7.00"
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runPricingForTest(t, "", string(raw), "migrate-env")
	if err != nil || !strings.Contains(stderr, "汇率与内置目录不同") {
		t.Fatalf("changed legacy catalog: stderr %q, %v", stderr, err)
	}
	override, err := config.ParseUsagePricingOverride([]byte(stdout))
	if err != nil || override.USDCNYRate != "7" {
		t.Fatalf("migrate-env output %q: %+v, %v", stdout, override, err)
	}

	for _, input := range []string{"", " \n", `{"schema_version":2}`, strings.Repeat(" ", config.MaxUsagePricingDocumentBytes+1)} {
		if stdout, _, err := runPricingForTest(t, "", input, "migrate-env"); err == nil || stdout != "" {
			t.Fatalf("migrate-env accepted %.20q: %q", input, stdout)
		}
	}
}

func TestPricingRejectsUnknownArguments(t *testing.T) {
	for _, arguments := range [][]string{
		nil, {"unknown"}, {"check", "extra"}, {"migrate-env", "extra"}, {"show", "extra"}, {"show", "-unknown"},
	} {
		if stdout, _, err := runPricingForTest(t, "", "", arguments...); err == nil || stdout != "" {
			t.Fatalf("pricing %v: %q, %v", arguments, stdout, err)
		}
	}
}
