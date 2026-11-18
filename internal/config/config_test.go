package config

import (
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("GATEWAY_PUBLIC_URL", "https://codex.example.test")
	t.Setenv("DATABASE_URL", "postgres://gateway:test@postgres/gateway")
	t.Setenv("SIDECAR_URL", "http://codex-compat:8317")
	t.Setenv("SIDECAR_API_KEY", "internal-only")
	t.Setenv("CPA_MANAGEMENT_KEY", "management-only-"+strings.Repeat("m", 27))
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	t.Setenv("KEY_HMAC_PEPPER", secret)
	t.Setenv("TOKEN_HMAC_PEPPER", secret)
	t.Setenv("API_KEY_ENCRYPTION_KEY", secret)
	t.Setenv("API_KEY_ENCRYPTION_KEY_FILE", "")
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", "")
	t.Setenv("GATEWAY_USAGE_PRICING_OVERRIDE_FILE", "")
	// Retired token/day variables must have no runtime effect, including no
	// startup validation. Monetary billing now governs token consumption.
	t.Setenv("LIMIT_KEY_TOKENS_DAY", "not-an-integer")
	t.Setenv("LIMIT_USER_TOKENS_DAY", "not-an-integer")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.KeyConcurrent != 16 || cfg.Limits.UserConcurrent != 16 ||
		cfg.Limits.GlobalConcurrent != 32 ||
		cfg.Limits.KeyRequestsPerDay != 10000 || cfg.Limits.UserRequestsPerDay != 20000 {
		t.Fatalf("unexpected limits: %+v", cfg.Limits)
	}
	if cfg.BodyLimit != 64<<20 {
		t.Fatalf("unexpected body limit: %d", cfg.BodyLimit)
	}
	if cfg.RPID != "codex.example.test" {
		t.Fatalf("unexpected RP ID: %s", cfg.RPID)
	}
	if got := string(cfg.APIKeyEncryptionKey); got != strings.Repeat("x", 32) {
		t.Fatalf("unexpected API key encryption key length/content: %d", len(cfg.APIKeyEncryptionKey))
	}
}

func TestRejectsHTTPAndShortSecrets(t *testing.T) {
	t.Setenv("GATEWAY_PUBLIC_URL", "http://codex.example.test")
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("SIDECAR_URL", "http://codex-compat:8317")
	t.Setenv("SIDECAR_API_KEY", "internal-only")
	t.Setenv("KEY_HMAC_PEPPER", base64.RawURLEncoding.EncodeToString([]byte("short")))
	t.Setenv("TOKEN_HMAC_PEPPER", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("API_KEY_ENCRYPTION_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("API_KEY_ENCRYPTION_KEY_FILE", "")
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", "")
	t.Setenv("GATEWAY_USAGE_PRICING_OVERRIDE_FILE", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid configuration")
	}
}

func TestLoadUsesBuiltInPricing(t *testing.T) {
	setValidLoadEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	builtIn, err := DefaultUsagePricing()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.UsagePricing, builtIn) {
		t.Fatal("Load did not use the built-in pricing catalog")
	}
	if cfg.UsagePricingSource.Overridden() || cfg.UsagePricingSource.CatalogAsOf != builtIn.CatalogAsOf ||
		cfg.UsagePricingSource.ModelCount != len(builtIn.Models) || len(cfg.UsagePricingSource.Digest) != 64 {
		t.Fatalf("unexpected pricing source: %+v", cfg.UsagePricingSource)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("built-in pricing fails deployment validation: %v", err)
	}
}

func TestLoadRejectsRetiredUsagePricingJSON(t *testing.T) {
	setValidLoadEnvironment(t)
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", validUsagePricingJSON)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_USAGE_PRICING_JSON is no longer supported") ||
		!strings.Contains(err.Error(), "scripts/pricing.sh") {
		t.Fatalf("retired pricing setting was not refused: %v", err)
	}
	if strings.Contains(err.Error(), "gpt-exact") {
		t.Fatal("refusal must not echo the retired value")
	}
}

func TestLoadAppliesPricingOverrideFile(t *testing.T) {
	setValidLoadEnvironment(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "override.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"usd_cny_rate":"7.05","fx_as_of":"2026-11-18"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_USAGE_PRICING_OVERRIDE_FILE", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsagePricing.USDCNYRate != "7.05" || cfg.UsagePricing.FXAsOf != "2026-11-18" ||
		!cfg.UsagePricingSource.OverrideFX || cfg.UsagePricingSource.OverridePath != path {
		t.Fatalf("override not applied: %+v", cfg.UsagePricingSource)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"models":{"gpt-unknown":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("invalid override was accepted")
	}
	t.Setenv("GATEWAY_USAGE_PRICING_OVERRIDE_FILE", filepath.Join(directory, "missing", "override.json"))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "is deploy/pricing mounted?") {
		t.Fatalf("missing override directory was accepted: %v", err)
	}
}

func TestLoadAPIKeyEncryptionKeyFromFile(t *testing.T) {
	setValidLoadEnvironment(t)
	key := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	path := t.TempDir() + "/api-key-encryption-key"
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("API_KEY_ENCRYPTION_KEY", "")
	t.Setenv("API_KEY_ENCRYPTION_KEY_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(cfg.APIKeyEncryptionKey); got != strings.Repeat("k", 32) {
		t.Fatalf("unexpected key length/content: %d", len(cfg.APIKeyEncryptionKey))
	}
}

func TestLoadRejectsInvalidAPIKeyEncryptionKey(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "missing", value: ""},
		{name: "invalid base64", value: "not base64!"},
		{name: "31 bytes", value: base64.RawURLEncoding.EncodeToString(make([]byte, 31))},
		{name: "33 bytes", value: base64.RawURLEncoding.EncodeToString(make([]byte, 33))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setValidLoadEnvironment(t)
			t.Setenv("API_KEY_ENCRYPTION_KEY", test.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "API_KEY_ENCRYPTION_KEY") {
				t.Fatalf("expected API key encryption key error, got %v", err)
			}
		})
	}
}

func TestLoadRejectsAPIKeyEncryptionKeyAndFileTogether(t *testing.T) {
	setValidLoadEnvironment(t)
	path := t.TempDir() + "/api-key-encryption-key"
	if err := os.WriteFile(path, []byte("unused"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("API_KEY_ENCRYPTION_KEY_FILE", path)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "set only one of API_KEY_ENCRYPTION_KEY") {
		t.Fatalf("expected env/file conflict, got %v", err)
	}
}

func TestValidateRequiresExactAPIKeyEncryptionKeyLength(t *testing.T) {
	for _, size := range []int{0, 31, 33} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			cfg := validConfigForValidation(t)
			cfg.APIKeyEncryptionKey = make([]byte, size)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "API_KEY_ENCRYPTION_KEY") {
				t.Fatalf("expected exact-length validation error, got %v", err)
			}
		})
	}
}

func setValidLoadEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("GATEWAY_PUBLIC_URL", "https://codex.example.test")
	t.Setenv("DATABASE_URL", "postgres://gateway:test@postgres/gateway")
	t.Setenv("DATABASE_URL_FILE", "")
	t.Setenv("SIDECAR_URL", "http://codex-compat:8317")
	t.Setenv("SIDECAR_API_KEY", "internal-only")
	t.Setenv("SIDECAR_API_KEY_FILE", "")
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("KEY_HMAC_PEPPER", secret)
	t.Setenv("KEY_HMAC_PEPPER_FILE", "")
	t.Setenv("TOKEN_HMAC_PEPPER", secret)
	t.Setenv("TOKEN_HMAC_PEPPER_FILE", "")
	t.Setenv("API_KEY_ENCRYPTION_KEY", secret)
	t.Setenv("API_KEY_ENCRYPTION_KEY_FILE", "")
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", "")
	t.Setenv("GATEWAY_USAGE_PRICING_OVERRIDE_FILE", "")
	t.Setenv("CPA_MANAGEMENT_KEY", "management-only-"+strings.Repeat("m", 27))
	t.Setenv("CPA_MANAGEMENT_KEY_FILE", "")
	// Retired settings refuse startup; keep the ambient environment out.
	for _, name := range []string{"CPA_INTEGRATION", "ANTIGRAVITY_TRANSPORT", "ANTIGRAVITY_BRIDGE_URL", "ANTIGRAVITY_BRIDGE_API_KEY", "ANTIGRAVITY_BRIDGE_API_KEY_FILE"} {
		t.Setenv(name, "")
	}
}

func validConfigForValidation(t *testing.T) Config {
	t.Helper()
	publicURL, err := url.Parse("https://codex.example.test")
	if err != nil {
		t.Fatal(err)
	}
	sidecarURL, err := url.Parse("http://codex-compat:8317")
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		PublicURL: publicURL, SidecarURL: sidecarURL,
		DatabaseURL: "postgres://example", SidecarToken: "internal-only",
		CPAManagementToken: "management-only-" + strings.Repeat("m", 27),
		KeyPepper:          make([]byte, 32), TokenPepper: make([]byte, 32),
		APIKeyEncryptionKey: make([]byte, 32),
		RPID:                "codex.example.test", RPOrigins: []string{"https://codex.example.test"},
		BodyLimit: defaultBodyLimit,
	}
}

func TestBrowserClientURLConfiguration(t *testing.T) {
	for _, test := range []struct {
		url     string
		dev, ok bool
	}{
		{"", false, true},
		{"https://ai.water555.com", false, true},
		{"https://ai.water555.com/workbench/", false, true},
		{"http://127.0.0.1:4174", true, true},
		{"http://ai.water555.com", false, false},
		{"ftp://ai.water555.com", true, false},
		{"https://user:password@ai.water555.com", false, false},
		{"https://ai.water555.com?next=evil", false, false},
		{"https://ai.water555.com?", false, false},
		{"https://ai.water555.com/#code=secret", false, false},
		{"//ai.water555.com", false, false},
		{"javascript:alert(1)", true, false},
	} {
		t.Run(test.url, func(t *testing.T) {
			setValidLoadEnvironment(t)
			t.Setenv("GATEWAY_BROWSER_CLIENT_URL", test.url)
			t.Setenv("GATEWAY_DEV_INSECURE_HTTP", strconv.FormatBool(test.dev))
			cfg, err := Load()
			if (err == nil) != test.ok {
				t.Fatalf("URL %q: err=%v", test.url, err)
			}
			if err == nil && (cfg.BrowserClientURL != nil) != (test.url != "") {
				t.Fatal("unexpected enablement")
			}
		})
	}
}
