package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPluginManagementPath(t *testing.T) {
	for route, want := range map[string][2]string{
		"upstream-accounts":                                          {"/v0/management/gateway/upstream-accounts", ""},
		"upstream-accounts/":                                         {"/v0/management/gateway/upstream-accounts", ""},
		"upstream-accounts/capabilities":                             {"/v0/management/gateway/upstream-accounts/capabilities", ""},
		"anthropic-accounts/concurrency":                             {"/v0/management/gateway/anthropic-accounts/concurrency", ""},
		"upstream-accounts/0123456789abcdef/status":                  {"/v0/management/gateway/upstream-accounts/status", "id=0123456789abcdef"},
		"antigravity-accounts/0123456789abcdef/quota":                {"/v0/management/gateway/antigravity-accounts/quota", "id=0123456789abcdef"},
		"model-identification/accounts/0123456789abcdef/models":      {"/v0/management/gateway/model-identification/accounts/models", "id=0123456789abcdef"},
		"gateway-management/codex/oauth":                             {"/v0/management/gateway/gateway-management/codex/oauth", ""},
		"gateway-management/anthropic/oauth/callback":                {"/v0/management/gateway/gateway-management/anthropic/oauth/callback", ""},
		"gateway-management/antigravity/oauth/status":                {"/v0/management/gateway/gateway-management/antigravity/oauth/status", ""},
		"gateway-management/codex/credentials":                       {"/v0/management/gateway/gateway-management/codex/credentials", ""},
		"gateway-management/codex/accounts/0123456789abcdef/refresh": {"/v0/management/gateway/gateway-management/codex/accounts/refresh", "id=0123456789abcdef"},
		"gateway-management/codex/accounts/0123456789abcdef":         {"/v0/management/gateway/gateway-management/codex/accounts", "id=0123456789abcdef"},
		// Only a well-formed account id is lifted out of the path.
		"upstream-accounts/0123456789ABCDEF/status": {"/v0/management/gateway/upstream-accounts/0123456789ABCDEF/status", ""},
	} {
		mapped, query := PluginManagementPath(route)
		if mapped != want[0] || query != want[1] {
			t.Errorf("PluginManagementPath(%q) = %q, %q; want %q, %q", route, mapped, query, want[0], want[1])
		}
	}
}

// pluginManagementClient returns a client whose transport records each
// management request and answers with body.
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
		client.antigravity = true
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

// The data-plane key must never be offered to an account route. A client
// without the management key calls none of them.
func TestAccountRoutesNeedTheManagementKey(t *testing.T) {
	const account = "0123456789abcdef"
	var seen []*http.Request
	client := pluginManagementClient(t, "codex", &seen, func(*http.Request) (int, string) { return 200, `{}` })
	client.UsePluginManagement("")
	ctx := context.Background()
	_, errList := client.ListUpstreamAccounts(ctx)
	_, errStatus := client.SetUpstreamAccountStatus(ctx, account, true)
	_, errQuota := client.QueryUpstreamAccountQuota(ctx, account)
	_, errConcurrency := client.UpstreamAccountConcurrency(ctx)
	for name, err := range map[string]error{
		"capability": client.RequireAccountAccessCapability(ctx), "list": errList, "status": errStatus,
		"quota": errQuota, "concurrency": errConcurrency,
	} {
		var failure *InternalAPIError
		if !errors.As(err, &failure) || failure.Code != "sidecar_unavailable" {
			t.Errorf("%s without the management key: %v", name, err)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("%d requests were sent without the management key", len(seen))
	}
}

func TestSyncAccountEgress(t *testing.T) {
	const account = "0123456789abcdef"
	for kind, namespace := range map[string]string{"codex": "upstream-accounts", "anthropic": "anthropic-accounts", "antigravity": "antigravity-accounts"} {
		var seen []*http.Request
		var bodies []string
		answer := `{"id":"` + account + `","revision":"r1"}`
		client := pluginManagementClient(t, kind, &seen, func(r *http.Request) (int, string) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(raw))
			return 200, answer
		})
		if err := client.SyncAccountEgress(context.Background(), account); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		r := seen[0]
		if got := r.Method + " " + r.URL.Path + " " + r.URL.RawQuery; got != "PUT /v0/management/gateway/"+namespace+"/egress id="+account {
			t.Fatalf("%s: request %q", kind, got)
		}
		// The route is resolved by the plugin, never sent by the Gateway.
		if bodies[0] != "{}" || r.Header.Get("Authorization") != "Bearer management-secret" {
			t.Fatalf("%s: body %q, auth %q", kind, bodies[0], r.Header.Get("Authorization"))
		}
		for _, invalid := range []string{
			`{"id":"fedcba9876543210","revision":"r1"}`,
			`{"id":"` + account + `","revision":""}`,
			`{"id":"` + account + `"}`,
			`{"id":"` + account + `","revision":"r1","proxy_url":"http://x"}`,
		} {
			answer = invalid
			if err := client.SyncAccountEgress(context.Background(), account); err == nil {
				t.Fatalf("%s: accepted %s", kind, invalid)
			}
		}
	}
	var seen []*http.Request
	client := pluginManagementClient(t, "codex", &seen, func(*http.Request) (int, string) { return 200, `{}` })
	if err := client.SyncAccountEgress(context.Background(), "not-an-id"); err == nil || len(seen) != 0 {
		t.Fatal("a malformed account id was sent")
	}
	client.UsePluginManagement("")
	if err := client.SyncAccountEgress(context.Background(), account); err == nil {
		t.Fatal("pushed without the management key")
	}
}

// TestPluginQuotaContract feeds the plugin's own golden output to the strict
// quota parser, so the two modules cannot drift apart unnoticed.
func TestPluginQuotaContract(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("..", "..", "deploy", "codex-compat", "plugin", "internal", "gatewayplugin", "testdata", "codex_quota_rpc.json"))
	if err != nil {
		t.Fatal(err)
	}
	const account = "0123456789abcdef"
	var seen []*http.Request
	client := pluginManagementClient(t, "codex", &seen, func(r *http.Request) (int, string) {
		if r.URL.Path == "/v0/management/gateway/upstream-accounts/capabilities" {
			return 200, `{"protocol":"` + AccountAccessProtocolV1 + `","group_upstream_account_scope":"` + GroupUpstreamAccountScopeV1 + `"}`
		}
		return 200, string(golden)
	})
	quota, err := client.QueryUpstreamAccountQuota(context.Background(), account)
	if err != nil {
		t.Fatalf("the plugin's quota answer was rejected: %v", err)
	}
	request := seen[len(seen)-1]
	body, _ := io.ReadAll(request.Body)
	if request.Method != http.MethodPost || request.URL.Path != "/v0/management/gateway/upstream-accounts/quota" ||
		request.URL.RawQuery != "id="+account || request.Header.Get("Authorization") != "Bearer management-secret" ||
		string(body) != accountRateLimitsRequestBody {
		t.Fatalf("unexpected quota request: %s %s %q", request.Method, request.URL, body)
	}
	primary := quota.Result.RateLimits.Primary
	review := quota.Result.RateLimitsByLimitID["code_review"]
	if primary == nil || primary.UsedPercent != 25 || primary.WindowDurationMins == nil || *primary.WindowDurationMins != 300 ||
		quota.Result.RateLimits.RateLimitReachedType == nil || review.Primary == nil || review.Secondary != nil {
		t.Fatalf("parsed quota %+v", quota.Result)
	}
	// The plugin answers from CPA's passive observation and says when it was
	// made.
	if quota.ObservedAt == nil || !quota.ObservedAt.Equal(time.Date(2026, 11, 9, 2, 6, 40, 0, time.UTC)) {
		t.Fatalf("observation time %v", quota.ObservedAt)
	}
	for name, observed := range map[string]string{
		"not a time":  `"yesterday"`,
		"no timezone": `"2026-11-09T02:06:40"`,
		"far future":  `"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"`,
		"before 2000": `"1999-12-31T23:59:59Z"`,
		"number":      `1794190000`,
		"null":        `null`,
	} {
		answer := strings.Replace(string(golden), `"2026-11-09T02:06:40Z"`, observed, 1)
		invalid := pluginManagementClient(t, "codex", &seen, func(*http.Request) (int, string) { return 200, answer })
		if _, err := invalid.QueryUpstreamAccountQuota(context.Background(), account); err == nil && name != "null" {
			t.Fatalf("%s observation time was accepted", name)
		} else if name == "null" && err != nil {
			t.Fatalf("an absent observation time was refused: %v", err)
		}
	}

	// The plugin's fixed error codes map to the same Gateway codes as the
	// source-patched image's.
	for _, test := range []struct {
		status int
		body   string
		code   string
	}{
		{401, `{"error":"quota_reauthentication_required"}`, "upstream_reauthentication_required"},
		{409, `{"error":"upstream_account_disabled"}`, "upstream_account_disabled"},
		{429, `{"error":"quota_upstream_rate_limited"}`, "upstream_quota_rate_limited"},
		{502, `{"error":"quota_rate_limit_missing"}`, "upstream_quota_rate_limit_missing"},
		{502, `{"error":"quota_upstream_unavailable"}`, "upstream_quota_unavailable"},
		{502, `{"error":"quota_account_identity_unavailable"}`, "upstream_quota_account_identity_unavailable"},
		{409, `{"error":"quota_not_observed"}`, "upstream_quota_not_observed"},
		{503, `{"error":"quota_observation_unavailable"}`, "upstream_quota_observation_unavailable"},
		{504, `{"error":"quota_upstream_timeout"}`, "upstream_quota_timeout"},
	} {
		failing := pluginManagementClient(t, "codex", &seen, func(*http.Request) (int, string) { return test.status, test.body })
		_, err = failing.QueryUpstreamAccountQuota(context.Background(), account)
		var internalErr *InternalAPIError
		if !errors.As(err, &internalErr) || internalErr.SafeCode() != test.code {
			t.Fatalf("%d %s: got %v, want %s", test.status, test.body, err, test.code)
		}
	}
}
