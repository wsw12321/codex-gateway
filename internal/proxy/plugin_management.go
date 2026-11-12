package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// pluginManagementBase is where unmodified CPA serves the Gateway plugin's
// management routes. They sit behind CPA's management key, not the data-plane
// key, so the two credentials stay separate.
const pluginManagementBase = "/v0/management/gateway"

// UsePluginManagement switches the client's internal account API from the
// source-patched image's /internal/* routes to the plugin's management routes,
// authenticated with CPA's management key.
func (c *Client) UsePluginManagement(managementToken string) {
	if c != nil {
		c.managementToken = managementToken
	}
}

// PluginManagementPath maps an /internal/* path of the source-patched image to
// the plugin's route. CPA matches plugin routes by exact path, so an account
// id cannot be a path segment: it moves to the id query parameter.
//
//	/internal/upstream-accounts/0123456789abcdef/status
//	  -> /v0/management/gateway/upstream-accounts/status?id=0123456789abcdef
func PluginManagementPath(path string) (mapped string, query string) {
	segments := strings.Split(strings.TrimPrefix(path, "/internal/"), "/")
	kept := segments[:0:0]
	for _, segment := range segments {
		if query == "" && upstreamAccountPattern.MatchString(segment) {
			query = url.Values{"id": {segment}}.Encode()
			continue
		}
		kept = append(kept, segment)
	}
	return pluginManagementBase + "/" + strings.Join(kept, "/"), query
}

// internalTarget resolves an /internal/* path to the URL and bearer token of
// the integration in use.
func (c *Client) internalTarget(path string) (*url.URL, string) {
	target := *c.baseURL
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	token := c.token
	if c.managementToken != "" {
		path, target.RawQuery = PluginManagementPath(path)
		token = c.managementToken
	}
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	return &target, token
}

// SyncAccountEgress asks the plugin to apply one account's egress binding now.
// The plugin resolves the route from the Gateway itself; nothing about the
// route travels in this request.
func (c *Client) SyncAccountEgress(ctx context.Context, accountID string) error {
	if c == nil || c.managementToken == "" {
		return &InternalAPIError{StatusCode: http.StatusServiceUnavailable, Code: "provider_account_unavailable"}
	}
	if !upstreamAccountPattern.MatchString(accountID) {
		return &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "invalid_upstream_account"}
	}
	var wire map[string]json.RawMessage
	if err := c.internalJSONBody(ctx, http.MethodPut, "/internal/upstream-accounts/"+accountID+"/egress", "{}", &wire); err != nil {
		return err
	}
	var id, revision string
	if len(wire) != 2 || json.Unmarshal(wire["id"], &id) != nil || id != accountID ||
		json.Unmarshal(wire["revision"], &revision) != nil || revision == "" {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	return nil
}
