//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const (
	quotaPath = "/v0/management/gateway/upstream-accounts/quota?id="
	quotaRPC  = `{"method":"account/rateLimits/read","id":6}`
)

// TestQuotaQuery runs the Owner's live Codex quota query in a real CPA
// process: the provider is reached through the account's own route, the
// answer is normalized, and provider failures become fixed codes.
func TestQuotaQuery(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()

	status, body := h.management(http.MethodPost, quotaPath+a.id, managementKey, quotaRPC)
	if status != http.StatusOK {
		t.Fatalf("quota query: %d %s", status, body)
	}
	var answer struct {
		ID     int `json:"id"`
		Result struct {
			RateLimits struct {
				LimitID   string `json:"limitId"`
				Primary   *struct{ UsedPercent, WindowDurationMins int64 }
				Secondary *struct{ UsedPercent, WindowDurationMins int64 }
			} `json:"rateLimits"`
			ByLimitID map[string]struct {
				Primary *struct{ UsedPercent int64 }
			} `json:"rateLimitsByLimitId"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.ID != 6 || answer.Result.RateLimits.LimitID != "codex" ||
		answer.Result.RateLimits.Primary == nil || answer.Result.RateLimits.Primary.UsedPercent != 30 || answer.Result.RateLimits.Primary.WindowDurationMins != 300 ||
		answer.Result.RateLimits.Secondary == nil || answer.Result.RateLimits.Secondary.UsedPercent != 55 ||
		len(answer.Result.ByLimitID) != 2 || answer.Result.ByLimitID["code_review"].Primary == nil || answer.Result.ByLimitID["code_review"].Primary.UsedPercent != 10 {
		t.Fatalf("unexpected quota answer: %s", body)
	}
	if strings.Contains(body, "leak@example.com") || strings.Contains(body, "plus") {
		t.Fatalf("provider fields reached the answer: %s", body)
	}
	calls := h.provider.seen()
	if len(calls) != 1 {
		t.Fatalf("%d provider calls", len(calls))
	}
	call := calls[0]
	if call.Via != "global" || call.Host != "chatgpt.com" || call.Path != "/backend-api/wham/usage" || call.Bearer != a.token ||
		call.Headers.Get("Chatgpt-Account-Id") != "acct-a" || call.ProxyAuth != "" {
		t.Fatalf("unbound account's query did not use the global egress: %+v", call)
	}
	for name := range call.Headers {
		if strings.HasPrefix(strings.ToLower(name), "x-codex-gateway") || strings.EqualFold(name, "Cookie") {
			t.Fatalf("an internal header reached the provider: %s", name)
		}
	}

	// A bound account's query goes through its own route with its credential.
	h.gateway.mu.Lock()
	h.gateway.routes[b.id] = h.route("route-b", "secret-b")
	h.gateway.mu.Unlock()
	h.resetObservations()
	if status, body = h.management(http.MethodPost, quotaPath+b.id, managementKey, quotaRPC); status != http.StatusOK {
		t.Fatalf("bound quota query: %d %s", status, body)
	}
	calls = h.provider.seen()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("route-b:secret-b"))
	if len(calls) != 1 || calls[0].Via != "account-egress" || calls[0].ProxyAuth != wantAuth || calls[0].Bearer != b.token {
		t.Fatalf("bound account's query did not use its route: %+v", calls)
	}

	for behavior, want := range map[string]struct {
		status int
		code   string
	}{
		"quota-401":        {http.StatusUnauthorized, "quota_reauthentication_required"},
		"quota-redirect":   {http.StatusBadGateway, "quota_upstream_error"},
		"quota-schema":     {http.StatusBadGateway, "quota_rate_limit_missing"},
		"quota-fractional": {http.StatusBadGateway, "quota_percent_fractional"},
	} {
		h.provider.set(a.token, behavior)
		h.resetObservations()
		status, body = h.management(http.MethodPost, quotaPath+a.id, managementKey, quotaRPC)
		if status != want.status || body != `{"error":"`+want.code+`"}` {
			t.Fatalf("%s: %d %s", behavior, status, body)
		}
		// A redirect is answered, never followed.
		if calls = h.provider.seen(); len(calls) != 1 {
			t.Fatalf("%s: %d provider calls", behavior, len(calls))
		}
	}
	h.provider.set(a.token, "")

	// Without a confirmed route nothing leaves the process.
	h.gateway.mu.Lock()
	h.gateway.egressDown = true
	h.gateway.mu.Unlock()
	h.resetObservations()
	if status, body = h.management(http.MethodPost, quotaPath+a.id, managementKey, quotaRPC); status != http.StatusBadGateway || body != `{"error":"quota_upstream_unavailable"}` {
		t.Fatalf("unresolvable route: %d %s", status, body)
	}
	if calls = h.provider.seen(); len(calls) != 0 {
		t.Fatalf("a query left without a route: %+v", calls)
	}
	h.gateway.mu.Lock()
	h.gateway.egressDown = false
	h.gateway.mu.Unlock()

	// Only the management key reaches the route.
	for name, key := range map[string]string{"no key": "", "data-plane key": internalKey} {
		if status, _ = h.management(http.MethodPost, quotaPath+a.id, key, quotaRPC); status != http.StatusUnauthorized {
			t.Fatalf("%s reached the quota route: %d", name, status)
		}
	}

	// The credential page reduces the same query to remaining shares.
	status, body = h.management(http.MethodPost, "/v0/management/gateway/gateway-management/codex/accounts/quota?id="+a.id, managementKey, "{}")
	if status != http.StatusOK || body != `{"quota":[{"model":"code_review","remaining_fraction":0.9},{"model":"codex","remaining_fraction":0.7}],"status":"ok"}` {
		t.Fatalf("credential page quota: %d %s", status, body)
	}
}

// TestClaudePassiveQuota checks that the credential page's Claude quota is
// CPA's own passive observation of a real response, read back through CPA's
// management listing.
func TestClaudePassiveQuota(t *testing.T) {
	h := newHarness(t)
	h.addAccount("a", nil) // keeps the readiness probe's model listing non-empty
	c := h.addClaudeAccount("c", "0c1a0de000000001")
	h.start()
	path := "/v0/management/gateway/gateway-management/anthropic/accounts/quota?id=" + c.id

	status, body := h.management(http.MethodPost, path, managementKey, "{}")
	if status != http.StatusOK || body != `{"quota":[],"status":"ok","windows":[]}` {
		t.Fatalf("before any request: %d %s", status, body)
	}

	r := h.do(call{path: "/v1/messages", body: `{"model":"claude-opus-5","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`})
	if r.status != http.StatusOK {
		t.Fatalf("claude request: %d %s", r.status, r.body)
	}
	if calls := h.provider.seen(); len(calls) == 0 || calls[len(calls)-1].Host != "api.anthropic.com" || calls[len(calls)-1].Bearer != c.token {
		t.Fatalf("the Claude account did not serve the request: %+v", calls)
	}
	want := `"windows":[{"window":"five_hour","used_percent":42,"reset_at":"2026-11-09T04:53:20Z","status":"allowed_warning"},` +
		`{"window":"seven_day","used_percent":10,"status":"allowed"}]`
	h.eventually("the passive observation", func() bool {
		status, body = h.management(http.MethodPost, path, managementKey, "{}")
		return status == http.StatusOK && strings.Contains(body, want) && strings.Contains(body, `"observed_at":"`)
	})
	if strings.Contains(body, c.token) || strings.Contains(body, "c@example.com") {
		t.Fatalf("credential data reached the answer: %s", body)
	}
	if status, _ = h.management(http.MethodPost, "/v0/management/gateway/gateway-management/anthropic/accounts/quota?id="+h.accounts["a"].id, managementKey, "{}"); status != http.StatusNotFound {
		t.Fatalf("a Codex account answered as Claude: %d", status)
	}
}
