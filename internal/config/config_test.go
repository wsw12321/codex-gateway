package config

import (
	"encoding/base64"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("GATEWAY_PUBLIC_URL", "https://codex.example.test")
	t.Setenv("DATABASE_URL", "postgres://gateway:test@postgres/gateway")
	t.Setenv("SIDECAR_URL", "http://codex-compat:8317")
	t.Setenv("SIDECAR_API_KEY", "internal-only")
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	t.Setenv("KEY_HMAC_PEPPER", secret)
	t.Setenv("TOKEN_HMAC_PEPPER", secret)
	t.Setenv("API_KEY_ENCRYPTION_KEY", secret)
	t.Setenv("API_KEY_ENCRYPTION_KEY_FILE", "")
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", `{"catalog_as_of":"2026-08-01","fx_as_of":"2026-08-01","usd_cny_rate":"7.20","models":{"gpt-test":{"input_usd_per_million":"1.25","cached_input_usd_per_million":"0.125","output_usd_per_million":"10"}}}`)
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
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", validUsagePricingJSON)

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid configuration")
	}
}

func TestLoadRequiresUsagePricing(t *testing.T) {
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected missing usage pricing to be rejected")
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
	t.Setenv("GATEWAY_USAGE_PRICING_JSON", validUsagePricingJSON)
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
		KeyPepper: make([]byte, 32), TokenPepper: make([]byte, 32),
		APIKeyEncryptionKey: make([]byte, 32),
		RPID:                "codex.example.test", RPOrigins: []string{"https://codex.example.test"},
		BodyLimit: defaultBodyLimit,
	}
}
