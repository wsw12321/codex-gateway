package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	AntigravityPublicModel = "gemini-3.1-pro-preview"
	AntigravityCLIModel    = "gemini-3.1-pro-high"
)

// ParseAntigravityModelRoutes accepts explicit public-to-CLI model mappings.
// Duplicate keys are rejected because silently replacing a route is unsafe.
func ParseAntigravityModelRoutes(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON must be a JSON object")
	}
	routes := make(map[string]string)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON has an invalid model key")
		}
		public, ok := key.(string)
		if !ok || !validRouteModel(public) || public == InternalGovernanceModel {
			return nil, errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON has an invalid public model")
		}
		if _, duplicate := routes[public]; duplicate {
			return nil, fmt.Errorf("ANTIGRAVITY_MODEL_ROUTES_JSON repeats model %q", public)
		}
		var cli string
		if err := decoder.Decode(&cli); err != nil || !validRouteModel(cli) {
			return nil, errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON has an invalid CLI model")
		}
		routes[public] = cli
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON is not terminated")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON has trailing data")
	}
	return routes, nil
}

func validRouteModel(model string) bool {
	if len(model) == 0 || len(model) > 128 {
		return false
	}
	for _, ch := range model {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') &&
			ch != '-' && ch != '_' && ch != '.' && ch != ':' {
			return false
		}
	}
	return true
}

func (c Config) validateAntigravity() error {
	if c.AntigravityBridgeURL == nil {
		if len(c.AntigravityModelRoutes) > 0 {
			return errors.New("ANTIGRAVITY_BRIDGE_URL is required when model routes are configured")
		}
		return nil
	}
	u := c.AntigravityBridgeURL
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("ANTIGRAVITY_BRIDGE_URL must use http or https without credentials, query, or fragment")
	}
	if len(c.AntigravityBridgeToken) < minSecretBytes {
		return errors.New("ANTIGRAVITY_BRIDGE_API_KEY must contain at least 32 bytes when the bridge URL is configured")
	}
	if c.AntigravityBridgeToken == c.SidecarToken {
		return errors.New("ANTIGRAVITY_BRIDGE_API_KEY must differ from SIDECAR_API_KEY")
	}
	for public, cli := range c.AntigravityModelRoutes {
		if public != AntigravityPublicModel || cli != AntigravityCLIModel {
			return errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON only supports gemini-3.1-pro-preview mapped to gemini-3.1-pro-high")
		}
	}
	return nil
}
