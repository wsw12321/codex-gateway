//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	quotaPath = "/v0/management/gateway/upstream-accounts/quota?id="
	quotaRPC  = `{"method":"account/rateLimits/read","id":6}`
)

// TestCodexPassiveQuota checks that the Owner's Codex quota is CPA's own
// passive observation of the rate-limit headers on a real response, read back
// through CPA's management listing. The quota routes never reach a provider.
func TestCodexPassiveQuota(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()
	facade := "/v0/management/gateway/gateway-management/codex/accounts/quota?id="

	// Nothing observed yet.
	status, body := h.management(http.MethodPost, quotaPath+a.id, managementKey, quotaRPC)
	if status != http.StatusConflict || body != `{"error":"quota_not_observed"}` {
		t.Fatalf("before any request: %d %s", status, body)
	}
	if status, body = h.management(http.MethodPost, facade+a.id, managementKey, "{}"); status != http.StatusOK || body != `{"quota":[],"status":"ok"}` {
		t.Fatalf("credential page before any request: %d %s", status, body)
	}

	// One Codex request through CPA; its response carries the headers.
	h.provider.set(a.token, "quota-signals")
	h.gateway.prefer = []string{a.id, b.id}
	if got := h.do(call{}); got.status != http.StatusOK {
		t.Fatalf("codex request: %d %s", got.status, got.body)
	}
	served := len(h.provider.seen())
	var answer struct {
		ID     int `json:"id"`
		Result struct {
			RateLimits struct {
				LimitID   string `json:"limitId"`
				Primary   *struct{ UsedPercent, WindowDurationMins, ResetsAt int64 }
				Secondary *struct{ UsedPercent, WindowDurationMins int64 }
			} `json:"rateLimits"`
			ByLimitID map[string]struct {
				Primary *struct{ UsedPercent int64 }
			} `json:"rateLimitsByLimitId"`
		} `json:"result"`
		ObservedAt time.Time `json:"observed_at"`
	}
	h.eventually("the passive observation", func() bool {
		status, body = h.management(http.MethodPost, quotaPath+a.id, managementKey, quotaRPC)
		return status == http.StatusOK
	})
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.ID != 6 || answer.Result.RateLimits.LimitID != "codex" ||
		answer.Result.RateLimits.Primary == nil || answer.Result.RateLimits.Primary.UsedPercent != 30 ||
		answer.Result.RateLimits.Primary.WindowDurationMins != 300 || answer.Result.RateLimits.Primary.ResetsAt != 1794200000 ||
		answer.Result.RateLimits.Secondary == nil || answer.Result.RateLimits.Secondary.UsedPercent != 55 ||
		len(answer.Result.ByLimitID) != 2 || answer.Result.ByLimitID["code_review"].Primary == nil || answer.Result.ByLimitID["code_review"].Primary.UsedPercent != 10 ||
		answer.ObservedAt.IsZero() || time.Since(answer.ObservedAt) > time.Minute {
		t.Fatalf("unexpected quota answer: %s", body)
	}
	if strings.Contains(body, "plus") || strings.Contains(body, a.token) {
		t.Fatalf("plan or credential data reached the answer: %s", body)
	}
	// The credential page reduces the same observation to remaining shares.
	status, body = h.management(http.MethodPost, facade+a.id, managementKey, "{}")
	if status != http.StatusOK || !strings.HasPrefix(body, `{"observed_at":"`) ||
		!strings.HasSuffix(body, `"quota":[{"model":"code_review","remaining_fraction":0.9},{"model":"codex","remaining_fraction":0.7}],"status":"ok"}`) {
		t.Fatalf("credential page quota: %d %s", status, body)
	}
	// Reading quota sent nothing upstream.
	if calls := h.provider.seen(); len(calls) != served {
		t.Fatalf("a quota read reached the provider: %+v", calls[served:])
	}
	for _, call := range h.provider.seen() {
		if call.Path != "/backend-api/codex/responses" {
			t.Fatalf("a request other than CPA's own reached the provider: %+v", call)
		}
	}

	// An observation the projection cannot accept becomes a fixed code.
	h.provider.set(b.token, "quota-signals-invalid")
	h.gateway.prefer = []string{b.id, a.id}
	if got := h.do(call{}); got.status != http.StatusOK {
		t.Fatalf("codex request: %d %s", got.status, got.body)
	}
	h.eventually("the invalid observation", func() bool {
		status, body = h.management(http.MethodPost, quotaPath+b.id, managementKey, quotaRPC)
		return status != http.StatusConflict
	})
	if status != http.StatusBadGateway || body != `{"error":"quota_percent_out_of_range"}` {
		t.Fatalf("invalid observation: %d %s", status, body)
	}

	// Only the management key reaches the route.
	for name, key := range map[string]string{"no key": "", "data-plane key": internalKey} {
		if status, _ = h.management(http.MethodPost, quotaPath+a.id, key, quotaRPC); status != http.StatusUnauthorized {
			t.Fatalf("%s reached the quota route: %d", name, status)
		}
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
