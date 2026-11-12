// Package gatewayplugin implements the Codex Gateway integration for CLIProxyAPI
// as an out-of-tree plugin. It contains no cgo: the C ABI boundary lives in the
// main package, so every decision here is testable with plain Go tests.
package gatewayplugin

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is resolved once from the process environment. The host passes a YAML
// document at registration, but every value this plugin needs is deployment
// wiring that the container entrypoint already owns, so the environment is the
// single source and the plugin carries no YAML dependency.
type Config struct {
	// GatewayURL is the fixed origin of the Gateway allocation callbacks.
	GatewayURL string
	// InternalKey authenticates the Gateway on the data plane and this plugin on
	// the Gateway's internal callbacks.
	InternalKey string
	// ManagementKey authenticates loopback calls to CPA's native management API.
	ManagementKey string
	// LoopbackURL is CPA's own listener as seen from inside the process.
	LoopbackURL string
	// AuthDir holds the OAuth credentials and the Gateway control documents.
	AuthDir string
	// AntigravityEnabled mirrors CPA_ANTIGRAVITY_ENABLED from the bridge rollback.
	AntigravityEnabled bool
	// BindingTTL bounds how long a conversation stays bound to an account.
	BindingTTL time.Duration
	// HoldTTL keeps a conversation's account slot between turns.
	HoldTTL time.Duration
	// CallbackTimeout bounds one Gateway allocation callback.
	CallbackTimeout time.Duration
	// EgressProxyHost is the only authority an account-bound egress URL may
	// name: the per-account listener of the egress service.
	EgressProxyHost string
}

const (
	defaultGatewayURL      = "http://gateway:8080"
	defaultInternalKeyFile = "/run/secrets/sidecar_api_key"
	defaultLoopbackURL     = "http://127.0.0.1:8317"
	defaultEgressProxyHost = "egress-allowlist:3129"
	minInternalKeyLength   = 43
)

// ErrConfig reports an unusable deployment configuration. The plugin stays
// registered but refuses every request, so the failure is closed and visible.
var ErrConfig = errors.New("gateway plugin configuration invalid")

// LoadConfig reads the configuration from getenv and readFile.
func LoadConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	cfg := Config{
		GatewayURL:         strings.TrimRight(valueOr(getenv("CPA_PLUGIN_GATEWAY_URL"), defaultGatewayURL), "/"),
		LoopbackURL:        strings.TrimRight(valueOr(getenv("CPA_PLUGIN_LOOPBACK_URL"), defaultLoopbackURL), "/"),
		AuthDir:            strings.TrimSpace(getenv("CPA_PLUGIN_AUTH_DIR")),
		AntigravityEnabled: !strings.EqualFold(strings.TrimSpace(getenv("CPA_ANTIGRAVITY_ENABLED")), "false"),
		BindingTTL:         time.Hour,
		HoldTTL:            5 * time.Minute,
		CallbackTimeout:    2 * time.Second,
		EgressProxyHost:    valueOr(getenv("CPA_PLUGIN_EGRESS_PROXY_HOST"), defaultEgressProxyHost),
	}
	if !validOrigin(cfg.GatewayURL) || !validOrigin(cfg.LoopbackURL) || cfg.AuthDir == "" || !validOrigin("http://"+cfg.EgressProxyHost) {
		return Config{}, ErrConfig
	}
	key, err := readKey(readFile, valueOr(getenv("CLIPROXY_API_KEY_FILE"), defaultInternalKeyFile))
	if err != nil {
		return Config{}, ErrConfig
	}
	cfg.InternalKey = key
	// The management key is optional: without it the credential facade is
	// unavailable while allocation keeps working.
	if path := strings.TrimSpace(getenv("CPA_MANAGEMENT_KEY_FILE")); path != "" {
		managementKey, errManagement := readKey(readFile, path)
		if errManagement != nil || managementKey == key {
			return Config{}, ErrConfig
		}
		cfg.ManagementKey = managementKey
	}
	return cfg, nil
}

func valueOr(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func validOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "http" && parsed.Host != "" && parsed.Path == "" &&
		parsed.RawQuery == "" && parsed.Fragment == "" && parsed.User == nil
}

func readKey(readFile func(string) ([]byte, error), path string) (string, error) {
	raw, err := readFile(path)
	if err != nil {
		return "", err
	}
	key := strings.TrimRight(string(raw), "\r\n")
	if len(key) < minInternalKeyLength {
		return "", ErrConfig
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return "", ErrConfig
	}
	return key, nil
}
