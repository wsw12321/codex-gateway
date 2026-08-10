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

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.KeyConcurrent != 4 || cfg.Limits.GlobalConcurrent != 12 {
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

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid configuration")
	}
}
