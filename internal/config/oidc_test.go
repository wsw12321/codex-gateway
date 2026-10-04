package config

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func validOIDCConfig() Config {
	public, _ := url.Parse("https://gateway.example.test")
	proxy, _ := url.Parse("http://172.28.30.4:3128")
	return Config{OIDCEnabled: true, OIDCIssuer: "https://project.supabase.co/auth/v1", OIDCClientID: "client", OIDCClientSecret: "secret", OIDCProxyURL: proxy, PublicURL: public}
}

func TestValidateOIDC(t *testing.T) {
	if err := validOIDCConfig().ValidateOIDC(); err != nil {
		t.Fatal(err)
	}
	for _, issuer := range []string{"http://project.supabase.co/auth/v1", "https://project.supabase.co:8443/auth/v1", "https://project.supabase.co/auth/../v1", "https://project.supabase.co/auth/%76%31", "https://project.supabase.co/auth/v1/", "https://user:pass@project.supabase.co/auth/v1", "https://project.supabase.co/auth/v1?q=x", "https://project.supabase.co/auth/v1#fragment", "https://127.0.0.1/auth/v1", "https://localhost/auth/v1"} {
		t.Run(issuer, func(t *testing.T) {
			cfg := validOIDCConfig()
			cfg.OIDCIssuer = issuer
			if err := cfg.ValidateOIDC(); err == nil {
				t.Fatal("unsafe issuer accepted")
			}
		})
	}
	for _, proxy := range []string{"", "http://squid:3128", "http://172.28.30.4", "https://172.28.30.4:3128", "http://8.8.8.8:3128", "http://127.0.0.1:3128", "http://172.28.30.4:3128/path", "http://172.28.30.4:3128?q=x", "http://user:pass@172.28.30.4:3128"} {
		t.Run(proxy, func(t *testing.T) {
			cfg := validOIDCConfig()
			cfg.OIDCProxyURL, _ = url.Parse(proxy)
			if err := cfg.ValidateOIDC(); err == nil {
				t.Fatal("unsafe proxy accepted")
			}
		})
	}
}

func TestLoadOIDCSecretAndDisabledDefault(t *testing.T) {
	setValidLoadEnvironment(t)
	t.Setenv("OIDC_ENABLED", "")
	t.Setenv("OIDC_CLIENT_SECRET_FILE", "/nonexistent/disabled-secret")
	t.Setenv("OIDC_AUTHORIZATION_URL", "invalid-while-disabled")
	cfg, err := Load()
	if err != nil || cfg.OIDCEnabled {
		t.Fatalf("disabled feature required OIDC settings: %v", err)
	}
	secretFile := t.TempDir() + "/oidc-secret"
	if err := os.WriteFile(secretFile, []byte("oidc-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OIDC_ENABLED", "true")
	t.Setenv("OIDC_ISSUER", "https://project.supabase.co/auth/v1")
	t.Setenv("OIDC_AUTHORIZATION_URL", " https://accounts.example.test/oauth/authorize ")
	t.Setenv("OIDC_CLIENT_ID", "gateway-client")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	t.Setenv("OIDC_CLIENT_SECRET_FILE", secretFile)
	t.Setenv("OIDC_PROXY_URL", "http://172.28.30.4:3128")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OIDCEnabled || cfg.OIDCClientSecret != "oidc-secret" {
		t.Fatal("OIDC secret not loaded from file")
	}
	if cfg.OIDCAuthorizationURL != "https://accounts.example.test/oauth/authorize" {
		t.Fatal("browser authorization URL was not loaded")
	}
	t.Setenv("OIDC_ENABLED", "perhaps")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OIDC_ENABLED") {
		t.Fatalf("invalid enable flag accepted: %v", err)
	}
}

func TestValidateOIDCAuthorizationURL(t *testing.T) {
	for _, endpoint := range []string{"", "https://accounts.example.test", "https://accounts.example.test/", "https://accounts.example.test/oauth/authorize", "https://accounts.example.test:443/oauth/authorize"} {
		t.Run("valid "+endpoint, func(t *testing.T) {
			cfg := validOIDCConfig()
			cfg.OIDCAuthorizationURL = endpoint
			if err := cfg.ValidateOIDC(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, endpoint := range []string{
		"/authorize", "//accounts.example.test/authorize", "http://accounts.example.test/authorize",
		"https:accounts.example.test/authorize", "https://accounts.example.test:8443/authorize",
		"https://accounts.example.test:/authorize", "https://user:pass@accounts.example.test/authorize",
		"https://accounts.example.test/authorize?client_id=untrusted", "https://accounts.example.test/authorize?",
		"https://accounts.example.test/authorize#fragment", "https://accounts.example.test/authorize#",
		"https://accounts.example.test/a/../authorize", "https://accounts.example.test//authorize",
		"https://accounts.example.test/%61uthorize", "https://accounts.example.test/a%2fauthorize",
		"https://accounts.example.test/a\\authorize", "https://accounts.example.test/authorize\n",
		"https://127.0.0.1/authorize", "https://[::1]/authorize", "https://localhost/authorize",
		"https://127.1/authorize", "https://0177.0.0.1/authorize", "https://0x7f.0.0.1/authorize",
		"https://.example.test/authorize", "https://accounts..test/authorize", "https://accounts.example.test./authorize",
		"https://-accounts.example.test/authorize", "https://accounts_.example.test/authorize",
	} {
		t.Run("invalid "+endpoint, func(t *testing.T) {
			cfg := validOIDCConfig()
			cfg.OIDCAuthorizationURL = endpoint
			if err := cfg.ValidateOIDC(); err == nil || !strings.Contains(err.Error(), "OIDC_AUTHORIZATION_URL") {
				t.Fatalf("unsafe authorization URL accepted: %v", err)
			}
		})
	}
}
