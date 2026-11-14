package config

import (
	"strings"
	"testing"
)

func TestCPADefaultsAndBoundaries(t *testing.T) {
	setValidLoadEnvironment(t)
	t.Setenv("ANTIGRAVITY_MODEL_ROUTES_JSON", "{}")
	t.Setenv("CPA_MANAGEMENT_KEY", strings.Repeat("m", 43))
	t.Setenv("CPA_MANAGEMENT_KEY_FILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AntigravityModelRoutes) != 5 || cfg.CPAManagementToken != strings.Repeat("m", 43) {
		t.Fatal("CPA defaults missing")
	}
	for _, change := range []func(*Config){
		// The plugin's account routes need CPA's management key, and it must
		// not be the key sent with every generation request.
		func(c *Config) { c.CPAManagementToken = "" },
		func(c *Config) { c.CPAManagementToken = c.SidecarToken },
		func(c *Config) { c.CPAManagementToken = "short" },
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

func TestCPAManagementKeyIsRequired(t *testing.T) {
	setValidLoadEnvironment(t)
	t.Setenv("CPA_MANAGEMENT_KEY", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CPA_MANAGEMENT_KEY") {
		t.Fatalf("Load() = %v, want a refusal naming CPA_MANAGEMENT_KEY", err)
	}
}

// The source-patched CPA image is gone. A deployment that still selects it
// must not start as if it had selected the plugin.
func TestRetiredCPAIntegrationRefusesStartup(t *testing.T) {
	for _, value := range []string{"patch", "automatic"} {
		setValidLoadEnvironment(t)
		t.Setenv("CPA_INTEGRATION", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CPA_INTEGRATION") {
			t.Fatalf("CPA_INTEGRATION=%s: Load() = %v", value, err)
		}
	}
	// The value earlier Compose files set stays accepted.
	setValidLoadEnvironment(t)
	t.Setenv("CPA_INTEGRATION", "plugin")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
