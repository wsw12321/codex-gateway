package config

import (
	"net/url"
	"os"
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

func TestLoadAntigravityBridgeSecretFileAndRoutes(t *testing.T) {
	setValidLoadEnvironment(t)
	t.Setenv("ANTIGRAVITY_TRANSPORT", "legacy-bridge")
	path := t.TempDir() + "/antigravity-secret"
	if err := os.WriteFile(path, []byte("independent-antigravity-bridge-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTIGRAVITY_BRIDGE_URL", "http://antigravity-bridge:8318/")
	t.Setenv("ANTIGRAVITY_BRIDGE_API_KEY", "")
	t.Setenv("ANTIGRAVITY_BRIDGE_API_KEY_FILE", path)
	t.Setenv("ANTIGRAVITY_MODEL_ROUTES_JSON", `{"gemini-3.1-pro-high":"gemini-3.1-pro-high"}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AntigravityBridgeURL.String() != "http://antigravity-bridge:8318" ||
		cfg.AntigravityBridgeToken != "independent-antigravity-bridge-secret" ||
		cfg.AntigravityModelRoutes["gemini-3.1-pro-high"] != "gemini-3.1-pro-high" {
		t.Fatal("bridge URL, secret, or model routes were not loaded")
	}
}

func TestValidateAntigravityConfiguration(t *testing.T) {
	t.Parallel()
	const bridgeToken = "independent-antigravity-bridge-secret"
	for _, test := range []struct {
		name      string
		url       string
		token     string
		routes    map[string]string
		wantError bool
	}{
		{name: "disabled"},
		{name: "unrouted bridge for rollback", url: "http://antigravity-bridge:8318", token: bridgeToken},
		{name: "supported mapping", url: "http://antigravity-bridge:8318", token: bridgeToken, routes: map[string]string{LegacyAntigravityPublicModel: LegacyAntigravityPublicModel}},
		{name: "missing URL", routes: map[string]string{"gemini-3.1-pro-high": "gemini-3.1-pro-high"}, wantError: true},
		{name: "missing secret", url: "http://antigravity-bridge:8318", wantError: true},
		{name: "short secret", url: "http://antigravity-bridge:8318", token: strings.Repeat("x", 31), wantError: true},
		{name: "reused secret", url: "http://antigravity-bridge:8318", token: strings.Repeat("s", 32), wantError: true},
		{name: "invalid scheme", url: "file://antigravity-bridge", token: bridgeToken, wantError: true},
		{name: "URL credential", url: "http://user:password@antigravity-bridge:8318", token: bridgeToken, wantError: true},
		{name: "URL query", url: "http://antigravity-bridge:8318/?secret=x", token: bridgeToken, wantError: true},
		{name: "URL fragment", url: "http://antigravity-bridge:8318/#x", token: bridgeToken, wantError: true},
		{name: "invalid model", url: "http://antigravity-bridge:8318", token: bridgeToken, routes: map[string]string{"a b": "model"}, wantError: true},
		{name: "unsupported public model", url: "http://antigravity-bridge:8318", token: bridgeToken, routes: map[string]string{"gemini-other": AntigravityCLIModel}, wantError: true},
		{name: "unsupported CLI model", url: "http://antigravity-bridge:8318", token: bridgeToken, routes: map[string]string{AntigravityPublicModel: "gemini-3.1-pro-low"}, wantError: true},
		{name: "retired API alias", url: "http://antigravity-bridge:8318", token: bridgeToken, routes: map[string]string{"gemini-3.1-pro-preview": AntigravityCLIModel}, wantError: true},
		{name: "cross-model mapping", url: "http://antigravity-bridge:8318", token: bridgeToken, routes: map[string]string{"gemini-3.8-flash-high": "gemini-3.8-flash-medium"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfigForValidation(t)
			cfg.SidecarToken = strings.Repeat("s", 32)
			if test.url != "" {
				cfg.AntigravityBridgeURL, _ = url.Parse(test.url)
			}
			cfg.AntigravityBridgeToken = test.token
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
	if len(models) != 8 {
		t.Fatalf("catalog contains %d models", len(models))
	}
	seen := map[string]bool{}
	cfg := validConfigForValidation(t)
	cfg.AntigravityTransport = "cpa"
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
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.8-flash", "gemini-3.8-flash-low", "gemini-3.8-flash-high-extra"} {
		if IsAntigravityModel(model) {
			t.Fatalf("accepted non-catalog model %q", model)
		}
	}
}

func TestRetiredGeminiPricingCannotReactivateModelAccess(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.8-flash"} {
		cfg := validConfigForValidation(t)
		cfg.UsagePricing.Models = map[string]ModelPricing{
			model: {InputUSDPerMillion: "2", CachedInputUSDPerMillion: "0.2", OutputUSDPerMillion: "12"},
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ANTIGRAVITY pricing catalog") {
			t.Fatalf("retired catalog %q: %v", model, err)
		}
	}
}
