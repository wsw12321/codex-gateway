package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestCPATransportDefaultsAndBoundaries(t *testing.T) {
	setValidLoadEnvironment(t)
	t.Setenv("ANTIGRAVITY_TRANSPORT", "")
	t.Setenv("ANTIGRAVITY_BRIDGE_URL", "")
	t.Setenv("ANTIGRAVITY_BRIDGE_API_KEY", "")
	t.Setenv("ANTIGRAVITY_BRIDGE_API_KEY_FILE", "")
	t.Setenv("ANTIGRAVITY_MODEL_ROUTES_JSON", "{}")
	t.Setenv("CPA_MANAGEMENT_KEY", strings.Repeat("m", 43))
	t.Setenv("CPA_MANAGEMENT_KEY_FILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UsesCPAAntigravity() || len(cfg.AntigravityModelRoutes) != 5 || cfg.CPAManagementToken != strings.Repeat("m", 43) {
		t.Fatal("CPA defaults missing")
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.AntigravityBridgeURL, _ = url.Parse("http://legacy:8318") },
		func(c *Config) { c.AntigravityBridgeToken = strings.Repeat("b", 43) },
		func(c *Config) { c.CPAManagementToken = c.SidecarToken },
		func(c *Config) { c.CPAManagementToken = "short" },
		func(c *Config) { c.AntigravityTransport = "automatic" },
		func(c *Config) {
			c.AntigravityModelRoutes = map[string]string{"gemini-3.1-pro-high": "gemini-3.1-pro-high"}
		},
		func(c *Config) {
			c.AntigravityModelRoutes = map[string]string{"gemini-pro-agent": "gemini-3.1-pro-low"}
		},
	} {
		test := cfg
		change(&test)
		if err := test.Validate(); err == nil {
			t.Fatal("accepted unsafe CPA configuration")
		}
	}
}

func TestCPAIntegrationSetting(t *testing.T) {
	setValidLoadEnvironment(t)
	t.Setenv("ANTIGRAVITY_TRANSPORT", "")
	t.Setenv("ANTIGRAVITY_BRIDGE_URL", "")
	t.Setenv("ANTIGRAVITY_BRIDGE_API_KEY", "")
	t.Setenv("ANTIGRAVITY_BRIDGE_API_KEY_FILE", "")
	t.Setenv("ANTIGRAVITY_MODEL_ROUTES_JSON", "{}")
	t.Setenv("CPA_INTEGRATION", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// The source-patched image stays the default until the plugin is accepted.
	if cfg.CPAIntegration != "patch" || cfg.UsesCPAPlugin() {
		t.Fatalf("default integration = %q", cfg.CPAIntegration)
	}
	t.Setenv("CPA_INTEGRATION", "plugin")
	if cfg, err = Load(); err != nil || !cfg.UsesCPAPlugin() {
		t.Fatalf("plugin integration rejected: %v", err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.CPAIntegration = "automatic" },
		// The legacy bridge allocates on its own; the plugin cannot cover it.
		func(c *Config) { c.AntigravityTransport = "legacy-bridge" },
	} {
		test := cfg
		change(&test)
		if err := test.Validate(); err == nil {
			t.Fatal("accepted an unsupported CPA integration")
		}
	}
}
