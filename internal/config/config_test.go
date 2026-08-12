package config

import (
	"encoding/base64"
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
		cfg.Limits.KeyRequestsPerDay != 1000 || cfg.Limits.UserRequestsPerDay != 2000 {
		t.Fatalf("unexpected limits: %+v", cfg.Limits)
	}
	if cfg.BodyLimit != 64<<20 {
		t.Fatalf("unexpected body limit: %d", cfg.BodyLimit)
	}
	if cfg.RPID != "codex.example.test" {
		t.Fatalf("unexpected RP ID: %s", cfg.RPID)
	}
}

func TestRejectsHTTPAndShortSecrets(t *testing.T) {
	t.Setenv("GATEWAY_PUBLIC_URL", "http://codex.example.test")
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("SIDECAR_URL", "http://codex-compat:8317")
	t.Setenv("SIDECAR_API_KEY", "internal-only")
	t.Setenv("KEY_HMAC_PEPPER", base64.RawURLEncoding.EncodeToString([]byte("short")))
	t.Setenv("TOKEN_HMAC_PEPPER", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
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
