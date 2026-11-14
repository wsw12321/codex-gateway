package server

import (
	"net/http"
	"testing"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
)

// Shared fixtures for tests that play CPA with the Gateway plugin.

// testManagementKey is CPA's management key in tests. Account routes carry
// it; generation requests carry the data-plane key.
const testManagementKey = "management-secret-0123456789abcdef0123456789"

// pluginRoutePrefix is where CPA serves the plugin's management routes.
const pluginRoutePrefix = "/v0/management/gateway/"

// withManagement gives a client CPA's management key, as New does for every
// client it builds.
func withManagement(client *gatewayproxy.Client) *gatewayproxy.Client {
	client.UsePluginManagement(testManagementKey)
	return client
}

// pluginPath is the request path of a plugin management route.
func pluginPath(route string) string { return pluginRoutePrefix + route }

// useLedger gives the server an allocation ledger and attaches it to clients,
// as New does.
func useLedger(s *Server, clients ...*gatewayproxy.Client) {
	if s.allocations == nil {
		s.allocations = gatewayproxy.NewAllocationLedger()
	}
	for _, client := range clients {
		client.UseAllocationLedger(s.allocations)
	}
}

// reportAllocation plays the plugin's attempt callback for the dispatch that
// r belongs to.
func reportAllocation(t *testing.T, s *Server, provider string, r *http.Request, accountID string) {
	t.Helper()
	report := gatewayproxy.AllocationReport{
		Token: r.Header.Get("X-Codex-Gateway-Request"), UserID: r.Header.Get("X-Codex-Gateway-User"), AccountID: accountID,
	}
	if err := s.allocations.Record(provider, report); err != nil {
		t.Errorf("record allocation: %v", err)
	}
}
