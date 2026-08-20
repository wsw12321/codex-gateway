// Package config loads and validates the gateway's process configuration.
// Secrets are deliberately accepted only through the environment (normally
// Docker secrets mounted by the entrypoint) and are never rendered back out.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBodyLimit         = int64(64 << 20)
	minSecretBytes           = 32
	apiKeyEncryptionKeyBytes = 32
)

type Limits struct {
	KeyRPM             int
	KeyConcurrent      int
	KeyRequestsPerDay  int64
	UserRPM            int
	UserConcurrent     int
	UserRequestsPerDay int64
	GlobalConcurrent   int
}

type Config struct {
	ListenAddress       string
	PublicURL           *url.URL
	RPID                string
	RPOrigins           []string
	DatabaseURL         string
	SidecarURL          *url.URL
	SidecarToken        string
	KeyPepper           []byte
	TokenPepper         []byte
	APIKeyEncryptionKey []byte
	TrustedProxy        []*net.IPNet
	BodyLimit           int64
	SessionIdle         time.Duration
	SessionMax          time.Duration
	ReauthMaxAge        time.Duration
	Limits              Limits
	UsagePricing        UsagePricing
	DevInsecure         bool
}

func Load() (Config, error) {
	databaseURL, err := envOrFile("DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	sidecarToken, err := envOrFile("SIDECAR_API_KEY")
	if err != nil {
		return Config{}, err
	}
	keyPepper, err := envOrFile("KEY_HMAC_PEPPER")
	if err != nil {
		return Config{}, err
	}
	tokenPepper, err := envOrFile("TOKEN_HMAC_PEPPER")
	if err != nil {
		return Config{}, err
	}
	apiKeyEncryptionKey, err := envOrFile("API_KEY_ENCRYPTION_KEY")
	if err != nil {
		return Config{}, err
	}
	usagePricing, err := ParseUsagePricing(os.Getenv("GATEWAY_USAGE_PRICING_JSON"))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddress: envDefault("GATEWAY_LISTEN", ":8080"),
		DatabaseURL:   databaseURL,
		SidecarToken:  sidecarToken,
		BodyLimit:     defaultBodyLimit,
		SessionIdle:   12 * time.Hour,
		SessionMax:    7 * 24 * time.Hour,
		ReauthMaxAge:  5 * time.Minute,
		UsagePricing:  usagePricing,
		DevInsecure:   envBool("GATEWAY_DEV_INSECURE_HTTP", false),
		Limits: Limits{
			KeyRPM:             30,
			KeyConcurrent:      16,
			KeyRequestsPerDay:  10000,
			UserRPM:            60,
			UserConcurrent:     16,
			UserRequestsPerDay: 20000,
			GlobalConcurrent:   32,
		},
	}

	if cfg.PublicURL, err = parseURL("GATEWAY_PUBLIC_URL", os.Getenv("GATEWAY_PUBLIC_URL")); err != nil {
		return Config{}, err
	}
	if cfg.SidecarURL, err = parseURL("SIDECAR_URL", os.Getenv("SIDECAR_URL")); err != nil {
		return Config{}, err
	}
	cfg.RPID = envDefault("WEBAUTHN_RP_ID", cfg.PublicURL.Hostname())
	cfg.RPOrigins = splitCSV(envDefault("WEBAUTHN_ORIGINS", cfg.PublicURL.Scheme+"://"+cfg.PublicURL.Host))

	if cfg.KeyPepper, err = decodeSecret("KEY_HMAC_PEPPER", keyPepper); err != nil {
		return Config{}, err
	}
	if cfg.TokenPepper, err = decodeSecret("TOKEN_HMAC_PEPPER", tokenPepper); err != nil {
		return Config{}, err
	}
	if cfg.APIKeyEncryptionKey, err = decodeExactSecret("API_KEY_ENCRYPTION_KEY", apiKeyEncryptionKey, apiKeyEncryptionKeyBytes); err != nil {
		return Config{}, err
	}
	if cfg.TrustedProxy, err = parseCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS")); err != nil {
		return Config{}, err
	}

	intValues := []struct {
		name string
		dst  *int
	}{
		{"LIMIT_KEY_RPM", &cfg.Limits.KeyRPM},
		{"LIMIT_KEY_CONCURRENT", &cfg.Limits.KeyConcurrent},
		{"LIMIT_USER_RPM", &cfg.Limits.UserRPM},
		{"LIMIT_USER_CONCURRENT", &cfg.Limits.UserConcurrent},
		{"LIMIT_GLOBAL_CONCURRENT", &cfg.Limits.GlobalConcurrent},
	}
	for _, item := range intValues {
		if err := envPositiveInt(item.name, item.dst); err != nil {
			return Config{}, err
		}
	}
	int64Values := []struct {
		name string
		dst  *int64
	}{
		{"LIMIT_KEY_REQUESTS_DAY", &cfg.Limits.KeyRequestsPerDay},
		{"LIMIT_USER_REQUESTS_DAY", &cfg.Limits.UserRequestsPerDay},
		{"GATEWAY_BODY_LIMIT_BYTES", &cfg.BodyLimit},
	}
	for _, item := range int64Values {
		if err := envPositiveInt64(item.name, item.dst); err != nil {
			return Config{}, err
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.PublicURL == nil || c.SidecarURL == nil {
		return errors.New("public and sidecar URLs are required")
	}
	if c.PublicURL.Scheme != "https" && !c.DevInsecure {
		return errors.New("GATEWAY_PUBLIC_URL must use https")
	}
	if c.PublicURL.User != nil || c.PublicURL.RawQuery != "" || c.PublicURL.Fragment != "" {
		return errors.New("GATEWAY_PUBLIC_URL must not contain credentials, query, or fragment")
	}
	if c.SidecarURL.Scheme != "http" && c.SidecarURL.Scheme != "https" {
		return errors.New("SIDECAR_URL must use http or https")
	}
	if c.SidecarURL.User != nil || c.SidecarURL.RawQuery != "" || c.SidecarURL.Fragment != "" {
		return errors.New("SIDECAR_URL must not contain credentials, query, or fragment")
	}
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if len(c.KeyPepper) < minSecretBytes || len(c.TokenPepper) < minSecretBytes {
		return errors.New("HMAC peppers must be at least 32 bytes")
	}
	if len(c.APIKeyEncryptionKey) != apiKeyEncryptionKeyBytes {
		return fmt.Errorf("API_KEY_ENCRYPTION_KEY must decode to exactly %d bytes", apiKeyEncryptionKeyBytes)
	}
	if c.SidecarToken == "" {
		return errors.New("SIDECAR_API_KEY is required")
	}
	if c.RPID == "" || len(c.RPOrigins) == 0 {
		return errors.New("WebAuthn RP ID and origins are required")
	}
	if c.BodyLimit <= 0 || c.BodyLimit > defaultBodyLimit {
		return fmt.Errorf("body limit must be between 1 and %d bytes", defaultBodyLimit)
	}
	return nil
}

func parseURL(name, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%s is not an absolute URL", name)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func decodeSecret(name, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil || len(b) < minSecretBytes {
		return nil, fmt.Errorf("%s must be base64-encoded and at least %d bytes", name, minSecretBytes)
	}
	return b, nil
}

func decodeExactSecret(name, raw string, size int) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil || len(b) != size {
		return nil, fmt.Errorf("%s must be base64-encoded and decode to exactly %d bytes", name, size)
	}
	return b, nil
}

func parseCIDRs(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, value := range splitCSV(raw) {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", value, err)
		}
		out = append(out, network)
	}
	return out, nil
}

func splitCSV(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envOrFile(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	path := strings.TrimSpace(os.Getenv(name + "_FILE"))
	if value != "" && path != "" {
		return "", fmt.Errorf("set only one of %s or %s_FILE", name, name)
	}
	if path == "" {
		return value, nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, err)
	}
	return strings.TrimSpace(string(contents)), nil
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envPositiveInt(name string, dst *int) error {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("%s must be a positive integer", name)
	}
	*dst = parsed
	return nil
}

func envPositiveInt64(name string, dst *int64) error {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("%s must be a positive integer", name)
	}
	*dst = parsed
	return nil
}
