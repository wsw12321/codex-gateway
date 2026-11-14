package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// pluginManagementBase is where CPA serves the Gateway plugin's management
// routes. They sit behind CPA's management key, not the data-plane key, so the
// two credentials stay separate.
const pluginManagementBase = "/v0/management/gateway"

// UsePluginManagement sets CPA's management key, which authenticates every
// account route the plugin serves.
func (c *Client) UsePluginManagement(managementToken string) {
	if c != nil {
		c.managementToken = managementToken
	}
}

// PluginManagementPath turns a route relative to the plugin's management base
// into the request path and query. CPA matches plugin routes by exact path, so
// an account id cannot be a path segment: it moves to the id query parameter.
//
//	upstream-accounts/0123456789abcdef/status
//	  -> /v0/management/gateway/upstream-accounts/status?id=0123456789abcdef
func PluginManagementPath(route string) (mapped string, query string) {
	segments := strings.Split(strings.Trim(route, "/"), "/")
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

// accountNamespace is the plugin's route namespace for this client's provider.
func (c *Client) accountNamespace() string {
	switch {
	case c.anthropic:
		return "anthropic-accounts"
	case c.antigravity:
		return "antigravity-accounts"
	}
	return "upstream-accounts"
}

// managementTarget resolves a plugin route to its URL. Without the management
// key there is no route to call: the data-plane key must never reach one.
func (c *Client) managementTarget(route string) (*url.URL, bool) {
	if c.managementToken == "" {
		return nil, false
	}
	target := *c.baseURL
	target.RawPath, target.Fragment = "", ""
	path, query := PluginManagementPath(route)
	target.Path, target.RawQuery = strings.TrimRight(c.baseURL.Path, "/")+path, query
	return &target, true
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
	if err := c.accountJSONBody(ctx, http.MethodPut, accountID+"/egress", "{}", &wire); err != nil {
		return err
	}
	var id, revision string
	if len(wire) != 2 || json.Unmarshal(wire["id"], &id) != nil || id != accountID ||
		json.Unmarshal(wire["revision"], &revision) != nil || revision == "" {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	return nil
}
