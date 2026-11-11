package gatewayplugin

import (
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type managementRegistration struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

func (p *Plugin) managementRoutes() managementRegistration {
	return managementRegistration{}
}

func managementJSON(status int, value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"internal_error"}`)
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		Body:       body,
	})
}

func (p *Plugin) handleManagement([]byte) []byte {
	return managementJSON(http.StatusNotFound, map[string]string{"error": "not_found"})
}
