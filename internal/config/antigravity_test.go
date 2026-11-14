package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAntigravityModelRoutes(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`null`, `[]`, `{"gemini-3.1-pro-high":null}`, `{"gemini-3.1-pro-high":123}`,
		`{"gemini-3.1-pro-high":""}`, `{"gemini-3.1-pro-high":"has spaces"}`,
		`{"":"gemini-3.1-pro-high"}`, `{"gemini-3.1-pro-high":"gemini-3.1-pro-high"} {}`,
		`{"gemini-3.1-pro-high":"first","gemini-3.1-pro-high":"second"}`,
		`{"gemini-3.1-pro-high":"first","gemini-3.1-pro-hig\u0068":"second"}`,
		`{"codex-auto-review":"gemini-3.1-pro-high"}`,
		`{"gemini-3.1-pro-high":"gemini-3.1-pro-high"`,
	} {
		if _, err := ParseAntigravityModelRoutes(raw); err == nil {
			t.Errorf("accepted invalid routes: %s", raw)
		}
	}
	routes, err := ParseAntigravityModelRoutes(`{"gemini-3.1-pro-high":"gemini-3.1-pro-high"}`)
	if err != nil || len(routes) != 1 || routes["gemini-3.1-pro-high"] != "gemini-3.1-pro-high" {
		t.Fatalf("routes = %#v, err = %v", routes, err)
	}
	for _, raw := range []string{"", "  ", "{}"} {
		if routes, err := ParseAntigravityModelRoutes(raw); err != nil || len(routes) != 0 {
			t.Fatalf("empty routes %q = %#v, %v", raw, routes, err)
		}
	}
}

func TestRetiredAntigravityBridgeSettingsRefuseStartup(t *testing.T) {
	for _, setting := range [][2]string{
		{"ANTIGRAVITY_TRANSPORT", "legacy-bridge"},
		{"ANTIGRAVITY_TRANSPORT", "automatic"},
		{"ANTIGRAVITY_BRIDGE_URL", "http://antigravity-bridge:8318"},
		{"ANTIGRAVITY_BRIDGE_API_KEY", "independent-antigravity-bridge-secret"},
		{"ANTIGRAVITY_BRIDGE_API_KEY_FILE", "/run/secrets/antigravity_bridge_api_key"},
	} {
		t.Run(setting[0]+"="+setting[1], func(t *testing.T) {
			setValidLoadEnvironment(t)
			t.Setenv(setting[0], setting[1])
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), setting[0]) {
				t.Fatalf("Load() = %v, want a refusal naming %s", err, setting[0])
			}
		})
	}
	// The value the Compose file used to set stays accepted.
	setValidLoadEnvironment(t)
	t.Setenv("ANTIGRAVITY_TRANSPORT", "cpa")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAntigravityModelRoutes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		routes    map[string]string
		wantError bool
	}{
		{name: "none"},
		{name: "reviewed model", routes: map[string]string{AntigravityPublicModel: AntigravityPublicModel}},
		{name: "invalid model", routes: map[string]string{"a b": "model"}, wantError: true},
		{name: "unsupported public model", routes: map[string]string{"gemini-other": AntigravityCLIModel}, wantError: true},
		{name: "unsupported native model", routes: map[string]string{AntigravityPublicModel: "gemini-3.1-pro-low"}, wantError: true},
		{name: "retired bridge model", routes: map[string]string{"gemini-3.1-pro-high": "gemini-3.1-pro-high"}, wantError: true},
		{name: "retired API alias", routes: map[string]string{"gemini-3.1-pro-preview": AntigravityCLIModel}, wantError: true},
		{name: "cross-model mapping", routes: map[string]string{"gemini-3.8-flash-high": "gemini-3-flash"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfigForValidation(t)
			cfg.AntigravityModelRoutes = test.routes
			err := cfg.Validate()
			if (err != nil) != test.wantError || err != nil && !strings.Contains(err.Error(), "ANTIGRAVITY_") {
				t.Fatalf("Validate() = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func TestAntigravityCatalogUsesExactCPAIDs(t *testing.T) {
	models := AntigravityModels()
	if AntigravityPublicModel != "gemini-3.8-flash-high" || AntigravityCLIModel != AntigravityPublicModel {
		t.Fatalf("default Antigravity model = %q/%q", AntigravityPublicModel, AntigravityCLIModel)
	}
	if len(models) != 5 {
		t.Fatalf("catalog contains %d models", len(models))
	}
	want := []string{
		"gemini-3-flash", "gemini-3.8-flash-high", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite",
		"gemini-4-argon-high",
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("catalog = %#v, want %#v", models, want)
	}
	seen := map[string]bool{}
	cfg := validConfigForValidation(t)
	cfg.AntigravityModelRoutes = map[string]string{}
	for _, model := range models {
		family, ok := AntigravityPricingModel(model)
		if seen[model] || !ok || !IsAntigravityModel(model) || family == "" {
			t.Fatalf("invalid catalog entry %q -> %q", model, family)
		}
		seen[model] = true
		cfg.AntigravityModelRoutes[model] = model
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	models[0] = "changed"
	if AntigravityModels()[0] == "changed" {
		t.Fatal("caller mutated shared catalog")
	}
	for _, model := range []string{"gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3.6-flash-high", "gemini-3.7-flash-high", "gemini-3.7-flash", "gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.8-flash", "gemini-3.8-flash-low", "gemini-3.8-flash-high-extra"} {
		if IsAntigravityModel(model) {
			t.Fatalf("accepted non-catalog model %q", model)
		}
	}
}

func TestRetiredGeminiPricingCannotReactivateModelAccess(t *testing.T) {
	for _, model := range []string{"gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3.6-flash-high", "gemini-3.7-flash-high", "gemini-3.7-flash", "gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.8-flash"} {
		cfg := validConfigForValidation(t)
		cfg.UsagePricing.Models = map[string]ModelPricing{
			model: {InputUSDPerMillion: "2", CachedInputUSDPerMillion: "0.2", OutputUSDPerMillion: "12"},
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ANTIGRAVITY pricing catalog") {
			t.Fatalf("retired catalog %q: %v", model, err)
		}
	}
}
