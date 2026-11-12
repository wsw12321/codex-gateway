// Package gatewayplugin implements the Codex Gateway integration for CLIProxyAPI
// as an out-of-tree plugin. It contains no cgo: the C ABI boundary lives in the
// main package, so every decision here is testable with plain Go tests.
package gatewayplugin

import (
	"errors"
	"net/url"
	"os"
	"strconv"
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
	// GlobalProxyURL is the deployment's global egress. CPA itself runs
	// without a proxy-url, so an account the Gateway leaves on the global
	// egress gets this URL written into its credential like any other route.
	// A credential without a proxy can then only connect directly, which the
	// sidecar's network refuses: no CPA fallback reaches the internet.
	GlobalProxyURL string
	// EgressReconcileInterval spaces the background passes that write every
	// account's route into its credential, so CPA's own refreshes never use a
	// route the Gateway no longer binds. Zero disables them.
	EgressReconcileInterval time.Duration
}

const (
	defaultGatewayURL      = "http://gateway:8080"
	defaultInternalKeyFile = "/run/secrets/sidecar_api_key"
	defaultLoopbackURL     = "http://127.0.0.1:8317"
	defaultEgressProxyHost = "egress-allowlist:3129"
	minInternalKeyLength   = 43

	defaultEgressReconcileSeconds = 60
	minEgressReconcileSeconds     = 10
	maxEgressReconcileSeconds     = 3600
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
		GlobalProxyURL:     strings.TrimRight(strings.TrimSpace(getenv("CPA_PLUGIN_GLOBAL_PROXY_URL")), "/"),
	}
	if !validOrigin(cfg.GatewayURL) || !validOrigin(cfg.LoopbackURL) || cfg.AuthDir == "" || !validOrigin("http://"+cfg.EgressProxyHost) {
		return Config{}, ErrConfig
	}
	// The global egress is required and must be a plain listener distinct from
	// the per-account one: a route credential is never sent to it.
	if !validOrigin(cfg.GlobalProxyURL) || strings.TrimPrefix(cfg.GlobalProxyURL, "http://") == cfg.EgressProxyHost {
		return Config{}, ErrConfig
	}
	interval, err := reconcileInterval(getenv("CPA_PLUGIN_EGRESS_RECONCILE_SECONDS"))
	if err != nil {
		return Config{}, ErrConfig
	}
	cfg.EgressReconcileInterval = interval
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

// reconcileInterval parses the background egress pass interval in seconds.
func reconcileInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultEgressReconcileSeconds * time.Second, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < minEgressReconcileSeconds || seconds > maxEgressReconcileSeconds {
		return 0, ErrConfig
	}
	return time.Duration(seconds) * time.Second, nil
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
