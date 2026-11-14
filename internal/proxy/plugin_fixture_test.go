package proxy

import (
	"net/http"
	"testing"
)

// Shared fixtures for tests that play CPA with the Gateway plugin.

// testManagementKey is CPA's management key in tests. Account routes must
// carry it, and generation requests must not.
const testManagementKey = "management-secret-0123456789abcdef0123456789"

// The response headers the removed source-patched image used for attribution.
// CPA sets neither; a response that carries them must attribute nothing.
const (
	retiredAccountHeader      = "X-Codex-Upstream-Account"
	retiredConversationHeader = "X-Codex-Conversation-Hash"
)

// withManagement gives a client CPA's management key, as the server does for
// every client it builds.
func withManagement(client *Client) *Client {
	client.UsePluginManagement(testManagementKey)
	return client
}

// pluginPath is the request path of a plugin management route.
func pluginPath(route string) string { return pluginManagementBase + "/" + route }

// reportAllocation plays the plugin's attempt callback for the dispatch that
// r belongs to.
func reportAllocation(t *testing.T, ledger *AllocationLedger, provider string, r *http.Request, accountID, conversationHash string) {
	t.Helper()
	report := AllocationReport{Token: r.Header.Get(dispatchHeader), UserID: r.Header.Get(gatewayUserHeader), AccountID: accountID, ConversationHash: conversationHash}
	if err := ledger.Record(provider, report); err != nil {
		t.Errorf("record allocation: %v", err)
	}
}
