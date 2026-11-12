package gatewayplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Model identification in plugin mode. The probes themselves are ordinary
// Responses requests the Gateway sends through CPA, pinned to one account by
// the Gateway's allocation callbacks; CPA builds every upstream request. This
// plugin only reports the protocol and lists the models CPA registered for
// one credential.
const (
	protocolModelIdentification = "model_identification_cpa_v1"
	modelIdentificationPrefix   = "model-identification/"
	maxIdentificationModels     = 256
)

// diagnosticError is the Gateway's strict diagnostic error envelope.
func diagnosticError(status int, code, stage string) []byte {
	return managementJSON(status, map[string]map[string]string{"error": {"code": code, "stage": stage}})
}

func (p *Plugin) modelIdentification(method, action string, query url.Values) []byte {
	switch {
	case method == http.MethodGet && action == "capabilities" && len(query) == 0:
		return managementJSON(http.StatusOK, map[string]string{"protocol": protocolModelIdentification})
	case method == http.MethodGet && action == "accounts/models":
		return p.identificationModels(query)
	}
	return managementError(http.StatusNotFound, "not_found")
}

// identificationModels lists the models CPA registered for one Codex
// credential, from CPA's own management API.
func (p *Plugin) identificationModels(query url.Values) []byte {
	account, code, _ := p.credentialAccount(ProviderCodex, query)
	switch code {
	case "":
	case "upstream_account_not_found":
		return diagnosticError(http.StatusNotFound, "probe_account_not_found", "preflight")
	default:
		return diagnosticError(http.StatusServiceUnavailable, "probe_unavailable", "preflight")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, data, err := p.cpaManagement(ctx, http.MethodGet, "/v0/management/auth-files/models", url.Values{"name": {account.Name}}, nil)
	var listing struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &listing) != nil || listing.Models == nil {
		return diagnosticError(http.StatusServiceUnavailable, "probe_unavailable", "preflight")
	}
	models := make([]string, 0, len(listing.Models))
	seen := map[string]bool{}
	for _, model := range listing.Models {
		if !validModelID(model.ID) || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		models = append(models, model.ID)
		if len(models) == maxIdentificationModels {
			break
		}
	}
	return managementJSON(http.StatusOK, map[string]any{"account_id": account.ID, "models": models})
}

// validModelID is the Gateway's model identifier contract.
func validModelID(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for i := 0; i < len(model); i++ {
		c := model[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':' || c == '/') {
			return false
		}
	}
	return true
}
