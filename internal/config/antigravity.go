package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// The default is a native CPA ID. Reasoning settings belong in the request.
	AntigravityPublicModel = "gemini-3.8-flash-high"
	AntigravityCLIModel    = AntigravityPublicModel
)

// AntigravityModels returns the reviewed native CPA catalog. Returning a fresh
// slice prevents callers from modifying the allowlist used at other boundaries.
func AntigravityModels() []string {
	return []string{
		"gemini-3-flash", "gemini-3.8-flash-high",
		"gemini-3.1-flash-lite", "gemini-3.5-flash-lite",
		"gemini-4-argon-high",
	}
}

// RetiredAntigravityModels retains names only for configuration migration and
// history. Restricted API key allowlists must not be rewritten to these names.
func RetiredAntigravityModels() []string {
	return []string{
		"gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3.1-pro-high",
		"gemini-3.6-flash-high", "gemini-3.6-flash-medium",
		"gemini-3.7-flash-high", "gemini-3.7-flash-medium", "gemini-3.7-flash",
		"gemini-3.8-flash-medium",
		"gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.1-flash-lite-preview",
		"gemini-3.6-flash", "gemini-3.8-flash",
	}
}

// AntigravityPricingModel identifies the Google API family used for local
// equivalent billing; Antigravity subscription charges are not token charges.
func AntigravityPricingModel(model string) (string, bool) {
	switch model {
	case "gemini-3-flash":
		return "gemini-3-flash-preview", true
	case "gemini-3.8-flash-high":
		return "gemini-3.8-flash", true
	case "gemini-3.1-flash-lite":
		return "gemini-3.1-flash-lite-preview", true
	case "gemini-3.5-flash-lite":
		return "gemini-3.5-flash-lite", true
	case "gemini-4-argon-high":
		return "gemini-4-argon", true
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

// rejectRetiredIntegrations refuses a deployment that still selects the
// source-patched CPA image or the Antigravity bridge. Both were removed;
// ignoring the setting would run the plugin contract against an image that
// does not speak it, or silently move bridge traffic to CPA accounts.
func rejectRetiredIntegrations() error {
	if integration := strings.TrimSpace(os.Getenv("CPA_INTEGRATION")); integration != "" && integration != "plugin" {
		return errors.New("CPA_INTEGRATION only supports plugin; the source-patched CPA image was removed")
	}
	if transport := strings.TrimSpace(os.Getenv("ANTIGRAVITY_TRANSPORT")); transport != "" && transport != "cpa" {
		return errors.New("ANTIGRAVITY_TRANSPORT only supports cpa; the legacy bridge was removed")
	}
	for _, name := range []string{"ANTIGRAVITY_BRIDGE_URL", "ANTIGRAVITY_BRIDGE_API_KEY", "ANTIGRAVITY_BRIDGE_API_KEY_FILE"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return fmt.Errorf("%s is no longer supported; the legacy Antigravity bridge was removed", name)
		}
	}
	return nil
}

func (c Config) validateAntigravity() error {
	// The plugin's account routes are served behind CPA's management key, which
	// must never be the key that travels with every generation request.
	if len(c.CPAManagementToken) < minSecretBytes || c.CPAManagementToken == c.SidecarToken {
		return errors.New("CPA_MANAGEMENT_KEY is required, must contain at least 32 bytes and differ from SIDECAR_API_KEY")
	}
	for model := range c.UsagePricing.Models {
		if strings.HasPrefix(model, "gemini-") && !IsAntigravityModel(model) {
			return fmt.Errorf("ANTIGRAVITY pricing catalog contains unsupported model %q; use the reviewed AGY model IDs", model)
		}
	}
	for public, native := range c.AntigravityModelRoutes {
		if !IsAntigravityModel(public) || public != native {
			return errors.New("ANTIGRAVITY_MODEL_ROUTES_JSON requires reviewed CPA native IDs mapped to themselves")
		}
	}
	return nil
}
