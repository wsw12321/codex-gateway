package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPluginManagementPath(t *testing.T) {
	for path, want := range map[string][2]string{
		"/internal/upstream-accounts":                                    {"/v0/management/gateway/upstream-accounts", ""},
		"/internal/upstream-accounts/capabilities":                       {"/v0/management/gateway/upstream-accounts/capabilities", ""},
		"/internal/anthropic-accounts/concurrency":                       {"/v0/management/gateway/anthropic-accounts/concurrency", ""},
		"/internal/upstream-accounts/0123456789abcdef/status":            {"/v0/management/gateway/upstream-accounts/status", "id=0123456789abcdef"},
		"/internal/antigravity-accounts/0123456789abcdef/quota":          {"/v0/management/gateway/antigravity-accounts/quota", "id=0123456789abcdef"},
		"/internal/model-identification/accounts/0123456789abcdef/probe": {"/v0/management/gateway/model-identification/accounts/probe", "id=0123456789abcdef"},
		"/internal/gateway-management/codex/oauth":                       {"/v0/management/gateway/gateway-management/codex/oauth", ""},
		// Only a well-formed account id is lifted out of the path.
		"/internal/upstream-accounts/0123456789ABCDEF/status": {"/v0/management/gateway/upstream-accounts/0123456789ABCDEF/status", ""},
	} {
		mapped, query := PluginManagementPath(path)
		if mapped != want[0] || query != want[1] {
			t.Errorf("PluginManagementPath(%q) = %q, %q; want %q, %q", path, mapped, query, want[0], want[1])
		}
	}
}

// pluginManagementClient returns a client in plugin mode whose transport
// records each internal request and answers with body.
func pluginManagementClient(t *testing.T, kind string, seen *[]*http.Request, respond func(*http.Request) (int, string)) *Client {
	t.Helper()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*seen = append(*seen, r)
		status, body := respond(r)
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	base, _ := url.Parse("http://compat.invalid")
	client := NewWithHTTPClient(base, "data-plane-secret", &http.Client{Transport: transport})
	switch kind {
	case "anthropic":
		client.anthropic = true
	case "antigravity":
		client.antigravity, client.cpaNative = true, true
	}
	client.UsePluginManagement("management-secret")
	return client
}

func TestPluginIntegrationUsesManagementRoutesAndKey(t *testing.T) {
	const account = "0123456789abcdef"
	for kind, namespace := range map[string]string{"codex": "upstream-accounts", "anthropic": "anthropic-accounts", "antigravity": "antigravity-accounts"} {
		t.Run(kind, func(t *testing.T) {
			var seen []*http.Request
			client := pluginManagementClient(t, kind, &seen, func(r *http.Request) (int, string) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					return 200, `{"protocol":"` + AccountAccessProtocolV1 + `","group_upstream_account_scope":"` + GroupUpstreamAccountScopeV1 + `","anthropic_messages":"` + AnthropicMessagesProtocolV1 + `"}`
				case strings.HasSuffix(r.URL.Path, "/status"):
					return 200, `{"id":"` + account + `","status":"unavailable","cliproxy_status":"active","gateway_manual_status":"manual_disabled","gateway_quota_status":"available"}`
				case strings.HasSuffix(r.URL.Path, "/concurrency"):
					return 200, `{"sampled_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","accounts":[{"id":"` + account + `","active_requests":2,"concurrent_limit":3}]}`
				}
				return 200, `{"accounts":[{"id":"` + account + `","masked_email":"a***@example.com","plan":"plus","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available","last_synced_at":"2026-11-11T00:00:00Z"}]}`
			})
			ctx := context.Background()
			if err := client.RequireAccountAccessCapability(ctx); err != nil {
				t.Fatalf("capability: %v", err)
			}
			accounts, err := client.ListUpstreamAccounts(ctx)
			if err != nil || len(accounts) != 1 || accounts[0].ID != account {
				t.Fatalf("list: %+v, %v", accounts, err)
			}
			if status, errStatus := client.SetUpstreamAccountStatus(ctx, account, false); errStatus != nil || status.Status != "unavailable" {
				t.Fatalf("status: %+v, %v", status, errStatus)
			}
			if concurrency, errConcurrency := client.UpstreamAccountConcurrency(ctx); errConcurrency != nil || len(concurrency.Accounts) != 1 || concurrency.Accounts[0].ActiveRequests != 2 {
				t.Fatalf("concurrency: %+v, %v", concurrency, errConcurrency)
			}
			want := []string{
				"GET /v0/management/gateway/" + namespace + "/capabilities ",
				"GET /v0/management/gateway/" + namespace + " ",
				"PUT /v0/management/gateway/" + namespace + "/status id=" + account,
				"GET /v0/management/gateway/" + namespace + "/concurrency ",
			}
			if len(seen) != len(want) {
				t.Fatalf("%d requests, want %d", len(seen), len(want))
			}
			for i, r := range seen {
				if got := r.Method + " " + r.URL.Path + " " + r.URL.RawQuery; got != want[i] {
					t.Errorf("request %d = %q, want %q", i, got, want[i])
				}
				// The data-plane key must never be offered to the management API.
				if r.Header.Get("Authorization") != "Bearer management-secret" {
					t.Errorf("request %d used the wrong credential", i)
				}
			}
		})
	}
}

func TestPluginIntegrationFailsClosedWithoutThePlugin(t *testing.T) {
	// Unmodified CPA without the plugin has no such route.
	var seen []*http.Request
	client := pluginManagementClient(t, "codex", &seen, func(*http.Request) (int, string) { return 404, `{"error":"not found"}` })
	if err := client.RequireAccountAccessCapability(context.Background()); err == nil {
		t.Fatal("a CPA without the plugin passed the capability probe")
	}
	// A plugin that does not bind account egress must not satisfy a Gateway
	// that requires it.
	client = pluginManagementClient(t, "codex", &seen, func(*http.Request) (int, string) {
		return 200, `{"protocol":"` + AccountAccessProtocolV1 + `","group_upstream_account_scope":"` + GroupUpstreamAccountScopeV1 + `"}`
	})
	client.RequireAccountEgress(true)
	if err := client.RequireAccountAccessCapability(context.Background()); err == nil {
		t.Fatal("account egress was treated as available although the plugin does not advertise it")
	}
}
