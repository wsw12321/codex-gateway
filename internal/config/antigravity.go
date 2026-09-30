package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// The default is a native CPA ID. Reasoning settings belong in the request.
	AntigravityPublicModel       = "gemini-pro-agent"
	LegacyAntigravityPublicModel = "gemini-3.1-pro-high"
	AntigravityCLIModel          = AntigravityPublicModel
)

// AntigravityModels returns the reviewed native CPA catalog. Returning a fresh
// slice prevents callers from modifying the allowlist used at other boundaries.
func AntigravityModels() []string {
	return []string{
		"gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3-flash",
		"gemini-3.6-flash-high", "gemini-3.7-flash-high", "gemini-3.8-flash-high",
		"gemini-3.1-flash-lite", "gemini-3.5-flash-lite",
	}
}

// LegacyAntigravityModels is exclusively for the controlled legacy bridge
// rollback transport. These IDs must never extend the native CPA allowlist.
func LegacyAntigravityModels() []string {
	return []string{
		"gemini-3.1-pro-high",
		"gemini-3.6-flash-high", "gemini-3.6-flash-medium",
		"gemini-3.7-flash-high", "gemini-3.7-flash-medium",
		"gemini-3.8-flash-high", "gemini-3.8-flash-medium",
	}
}

func IsLegacyAntigravityModel(model string) bool {
	for _, legacy := range LegacyAntigravityModels() {
		if model == legacy {
			return true
		}
	}
	return false
}

// RetiredAntigravityModels retains names only for configuration migration and
// history. Restricted API key allowlists must not be rewritten to these names.
func RetiredAntigravityModels() []string {
	return []string{
		"gemini-3.1-pro-high", "gemini-3.6-flash-medium", "gemini-3.7-flash-medium", "gemini-3.8-flash-medium",
		"gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.1-flash-lite-preview",
		"gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.8-flash",
	}
}

// AntigravityPricingModel identifies the Google API family used for local
// equivalent billing; Antigravity subscription charges are not token charges.
func AntigravityPricingModel(model string) (string, bool) {
	switch model {
	case "gemini-pro-agent", "gemini-3.1-pro-low":
		return "gemini-3.1-pro-preview", true
	case "gemini-3-flash":
		return "gemini-3-flash-preview", true
	case "gemini-3.6-flash-high":
		return "gemini-3.6-flash", true
	case "gemini-3.7-flash-high":
		return "gemini-3.7-flash", true
	case "gemini-3.8-flash-high":
		return "gemini-3.8-flash", true
	case "gemini-3.1-flash-lite":
		return "gemini-3.1-flash-lite-preview", true
	case "gemini-3.5-flash-lite":
		return "gemini-3.5-flash-lite", true
	default:
		return "", false
	}
}

func IsAntigravityModel(model string) bool {
	_, ok := AntigravityPricingModel(model)
	return ok
}

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

func (c Config) UsesCPAAntigravity() bool { return c.AntigravityTransport == "cpa" }

func (c Config) validateAntigravity() error {
	if c.AntigravityTransport != "" && c.AntigravityTransport != "cpa" && c.AntigravityTransport != "legacy-bridge" {
		return errors.New("ANTIGRAVITY_TRANSPORT must be cpa or legacy-bridge")
	}
	if c.CPAManagementToken != "" && (len(c.CPAManagementToken) < minSecretBytes || c.CPAManagementToken == c.SidecarToken) {
		return errors.New("CPA_MANAGEMENT_KEY must contain at least 32 bytes and differ from SIDECAR_API_KEY")
	}
	for model := range c.UsagePricing.Models {
		if strings.HasPrefix(model, "gemini-") && !IsAntigravityModel(model) && !IsLegacyAntigravityModel(model) {
			return fmt.Errorf("ANTIGRAVITY pricing catalog contains unsupported model %q; use the reviewed AGY model IDs", model)
		}
	}
	if c.UsesCPAAntigravity() {
		if c.AntigravityBridgeURL != nil || c.AntigravityBridgeToken != "" {
			return errors.New("ANTIGRAVITY_TRANSPORT=cpa cannot configure legacy bridge credentials or URL")
		}
		for public, native := range c.AntigravityModelRoutes {
			if !IsAntigravityModel(public) || public != native {
				return errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON requires reviewed CPA native IDs mapped to themselves")
			}
		}
		return nil
	}
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
		if !IsLegacyAntigravityModel(public) || public != cli {
			return errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON only supports reviewed AGY model IDs mapped to themselves")
		}
	}
	return nil
}
