//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stableIndex is deliberately re-derived here instead of imported, so the test
// fails if the plugin's account index ever drifts from the documented formula.
func stableIndex(accountID string) string {
	inner := sha256.Sum256([]byte(accountID))
	outer := sha256.Sum256([]byte("auth_index_seed:codex-account:" + hex.EncodeToString(inner[:])))
	return hex.EncodeToString(outer[:8])
}

func groups(layers ...string) string { return "[" + strings.Join(layers, ",") + "]" }

func groupLayer(id string, accounts ...string) string {
	raw, _ := json.Marshal(accounts)
	return `{"group_id":"` + id + `","account_ids":` + string(raw) + `}`
}

const (
	groupA        = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	personalLayer = `{"group_id":"00000000-0000-0000-0000-000000000000","all_accounts":true}`
)

func TestAllocation(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()

	t.Run("gateway choice is honored and reported", func(t *testing.T) {
		h.resetObservations()
		h.gateway.prefer = []string{b.id}
		got := h.do(call{session: "019a0000-0000-7000-8000-000000000001"})
		if got.status != http.StatusOK || !strings.Contains(got.body, "response.completed") {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if served := h.served(); len(served) != 1 || served[0] != "b" {
			t.Fatalf("provider saw %v, want the Gateway's choice b", served)
		}
		reports := h.gateway.reportsFor(got.token)
		if len(reports) != 1 || reports[0].AccountID != b.id || reports[0].UserID != userAlice || !strings.HasPrefix(reports[0].ConversationHash, "conv-") {
			t.Fatalf("unexpected attribution: %+v", reports)
		}
	})

	t.Run("internal headers never reach the provider", func(t *testing.T) {
		h.resetObservations()
		got := h.do(call{groups: groups(personalLayer), headers: map[string]string{"X-Codex-Upstream-Account": "0123456789abcdef", "X-Codex-Conversation-Hash": "conv-00000000000000000000000000000000"}})
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		calls := h.provider.seen()
		if len(calls) != 1 {
			t.Fatalf("provider saw %d calls", len(calls))
		}
		for name := range calls[0].Headers {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "x-codex-gateway-") || lower == "x-codex-upstream-account" || lower == "x-codex-conversation-hash" {
				t.Fatalf("internal header %s reached the provider", name)
			}
		}
		if strings.Contains(calls[0].Bearer, internalKey) {
			t.Fatal("the internal key reached the provider")
		}
	})

	t.Run("a bound conversation stays on its account", func(t *testing.T) {
		h.resetObservations()
		h.gateway.prefer = []string{a.id}
		session := "019a0000-0000-7000-8000-000000000002"
		h.do(call{session: session})
		h.gateway.prefer = []string{b.id}
		h.do(call{session: session})
		if served := h.served(); len(served) != 2 || served[0] != "a" || served[1] != "a" {
			t.Fatalf("provider saw %v, want both turns on a", served)
		}
	})

	t.Run("two callers with one session id do not share a binding", func(t *testing.T) {
		h.resetObservations()
		session := "019a0000-0000-7000-8000-000000000003"
		h.gateway.prefer = []string{a.id}
		first := h.do(call{affinity: 1, session: session})
		h.gateway.prefer = []string{b.id}
		second := h.do(call{affinity: 2, user: userBob, session: session})
		if served := h.served(); len(served) != 2 || served[0] != "a" || served[1] != "b" {
			t.Fatalf("provider saw %v, want a then b", served)
		}
		one, two := h.gateway.reportsFor(first.token), h.gateway.reportsFor(second.token)
		if len(one) != 1 || len(two) != 1 || one[0].ConversationHash == two[0].ConversationHash {
			t.Fatalf("conversation hashes are not isolated per caller: %+v %+v", one, two)
		}
	})

	t.Run("an unauthorized user reaches no account", func(t *testing.T) {
		h.resetObservations()
		h.gateway.permitted[userBob] = map[string]bool{}
		defer delete(h.gateway.permitted, userBob)
		got := h.do(call{user: userBob, affinity: 2})
		if got.status != http.StatusServiceUnavailable || !strings.Contains(got.body, "gateway_allocation_unavailable") {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if len(h.provider.seen()) != 0 {
			t.Fatal("an unauthorized request reached the provider")
		}
	})

	t.Run("authorization narrows the pool", func(t *testing.T) {
		h.resetObservations()
		h.gateway.permitted[userBob] = map[string]bool{a.id: true}
		defer delete(h.gateway.permitted, userBob)
		h.gateway.prefer = []string{b.id}
		for i := 0; i < 3; i++ {
			if got := h.do(call{user: userBob, affinity: 2}); got.status != http.StatusOK {
				t.Fatalf("status %d: %s", got.status, got.body)
			}
		}
		for _, name := range h.served() {
			if name != "a" {
				t.Fatalf("user reached account %s outside its entitlement", name)
			}
		}
	})

	t.Run("a group scope restricts the pool", func(t *testing.T) {
		h.resetObservations()
		h.gateway.prefer = []string{a.id}
		if got := h.do(call{groups: groups(groupLayer(groupA, b.id))}); got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if served := h.served(); len(served) != 1 || served[0] != "b" {
			t.Fatalf("provider saw %v, want only the group's account b", served)
		}
		h.resetObservations()
		got := h.do(call{groups: groups(groupLayer(groupA))})
		if got.status != http.StatusTooManyRequests || len(h.provider.seen()) != 0 {
			t.Fatalf("an empty group scope was not refused: status %d, %d provider calls", got.status, len(h.provider.seen()))
		}
		if reports := h.gateway.reportsFor(got.token); len(reports) != 1 || reports[0].Rejected != "upstream_concurrency_exceeded" {
			t.Fatalf("the refusal was not reported: %+v", reports)
		}
	})

	t.Run("requests without a trusted identity are refused", func(t *testing.T) {
		h.resetObservations()
		for name, c := range map[string]call{
			"no request token": {noToken: true},
			"malformed user":   {user: "not-a-uuid"},
			"malformed groups": {groups: `[{"group_id":"x"}]`},
		} {
			if got := h.do(c); got.status < 400 {
				t.Fatalf("%s: status %d", name, got.status)
			}
		}
		if got := h.do(call{key: managementKey}); got.status != http.StatusUnauthorized {
			t.Fatalf("the management key opened the data plane: %d", got.status)
		}
		if got := h.do(call{key: strings.Repeat("x", 50)}); got.status != http.StatusUnauthorized {
			t.Fatalf("a wrong key was accepted: %d", got.status)
		}
		if len(h.provider.seen()) != 0 {
			t.Fatal("an untrusted request reached the provider")
		}
	})

	if !h.alive() {
		t.Fatal("CPA exited during the scenarios")
	}
}

func TestServerErrorMovesToNextAuthorizedAccount(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()
	h.provider.set(a.token, "e500")
	h.gateway.prefer = []string{a.id, b.id}
	got := h.do(call{session: "019a0000-0000-7000-8000-000000000011"})
	if got.status != http.StatusOK {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	if served := h.served(); len(served) != 2 || served[0] != "a" || served[1] != "b" {
		t.Fatalf("provider saw %v, want a then b", served)
	}
	reports := h.gateway.reportsFor(got.token)
	if len(reports) != 2 || reports[0].AccountID != a.id || reports[1].AccountID != b.id {
		t.Fatalf("the final account was not the last report: %+v", reports)
	}
	// The conversation now belongs to b, not to the account that failed.
	h.resetObservations()
	h.provider.set(a.token, "")
	h.do(call{session: "019a0000-0000-7000-8000-000000000011"})
	if served := h.served(); len(served) != 1 || served[0] != "b" {
		t.Fatalf("next turn went to %v, want b", served)
	}
}

func TestRequestErrorIsNotReplayed(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()
	h.provider.set(a.token, "e400")
	h.gateway.prefer = []string{a.id, b.id}
	got := h.do(call{})
	if got.status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	if served := h.served(); len(served) != 1 || served[0] != "a" {
		t.Fatalf("provider saw %v, want a single attempt on a", served)
	}
}

func TestUsageLimitLocksAccountDurably(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()
	h.provider.set(a.token, "limit")
	h.gateway.prefer = []string{a.id, b.id}
	got := h.do(call{})
	if got.status != http.StatusOK {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	statePath := filepath.Join(h.authDir, ".gateway-account-state")
	h.eventually("the quota lock to be persisted", func() bool {
		raw, err := os.ReadFile(statePath)
		return err == nil && strings.Contains(string(raw), a.id) && strings.Contains(string(raw), `"quota_exhausted":true`)
	})
	info, err := os.Stat(statePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, %v", info.Mode().Perm(), err)
	}
	// The provider recovers, but the account stays locked until the Owner acts.
	h.provider.set(a.token, "")
	h.stop()
	h.start()
	h.resetObservations()
	h.gateway.prefer = []string{a.id, b.id}
	for i := 0; i < 3; i++ {
		if got = h.do(call{}); got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
	}
	for _, name := range h.served() {
		if name != "b" {
			t.Fatalf("a quota-locked account served a request after restart (%v)", h.served())
		}
	}
}

func TestConcurrencyLimit(t *testing.T) {
	h := newHarness(t)
	a := h.addAccount("a", nil)
	h.start()
	h.gateway.limits[a.id] = 1
	h.provider.set(a.token, "hold")
	done := make(chan result, 1)
	go func() { done <- h.do(call{session: "019a0000-0000-7000-8000-000000000021", timeout: 3 * time.Second}) }()
	h.eventually("the first stream to reach the provider", func() bool { return len(h.provider.seen()) == 1 })
	got := h.do(call{session: "019a0000-0000-7000-8000-000000000022"})
	if got.status != http.StatusTooManyRequests {
		t.Fatalf("a second conversation was admitted over the limit: %d %s", got.status, got.body)
	}
	if reports := h.gateway.reportsFor(got.token); len(reports) != 1 || reports[0].Rejected != "upstream_concurrency_exceeded" {
		t.Fatalf("the refusal was not reported: %+v", reports)
	}
	if calls := h.provider.seen(); len(calls) != 1 {
		t.Fatalf("provider saw %d calls, want only the first stream", len(calls))
	}
	// The first client disconnects; its slot is released without a hold.
	<-done
	h.provider.set(a.token, "")
	h.eventually("the slot to be released", func() bool {
		return h.do(call{session: "019a0000-0000-7000-8000-000000000023"}).status == http.StatusOK
	})
}

func TestFailsClosed(t *testing.T) {
	t.Run("gateway unreachable", func(t *testing.T) {
		h := newHarness(t)
		h.addAccount("a", nil)
		h.gatewayURL = "http://127.0.0.1:1"
		h.start()
		got := h.do(call{})
		if got.status != http.StatusServiceUnavailable || len(h.provider.seen()) != 0 {
			t.Fatalf("status %d with %d provider calls: %s", got.status, len(h.provider.seen()), got.body)
		}
	})
	t.Run("plugin disabled", func(t *testing.T) {
		h := newHarness(t)
		h.addAccount("a", nil)
		h.pluginEnabled = false
		h.start()
		got := h.do(call{})
		if got.status != http.StatusServiceUnavailable || len(h.provider.seen()) != 0 {
			t.Fatalf("status %d with %d provider calls: %s", got.status, len(h.provider.seen()), got.body)
		}
	})
	t.Run("unreadable control document", func(t *testing.T) {
		h := newHarness(t)
		h.addAccount("a", nil)
		if err := os.WriteFile(filepath.Join(h.authDir, ".gateway-account-state"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		h.dataPlaneClosed = true
		h.start()
		got := h.do(call{})
		if got.status < 400 || len(h.provider.seen()) != 0 {
			t.Fatalf("status %d with %d provider calls: %s", got.status, len(h.provider.seen()), got.body)
		}
	})
	t.Run("credential without a verified identity", func(t *testing.T) {
		h := newHarness(t)
		h.addAccount("a", map[string]any{"account_id": "", "id_token": "", "access_token": "opaque-token"})
		h.start()
		got := h.do(call{})
		if got.status != http.StatusServiceUnavailable || len(h.provider.seen()) != 0 {
			t.Fatalf("status %d with %d provider calls: %s", got.status, len(h.provider.seen()), got.body)
		}
	})
}

func TestPerAccountEgress(t *testing.T) {
	h := newHarness(t)
	a := h.addAccount("a", map[string]any{"proxy_url": ""})
	h.start()
	if got := h.do(call{}); got.status != http.StatusOK {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	if calls := h.provider.seen(); len(calls) != 1 || calls[0].Via != "global" {
		t.Fatalf("unexpected egress %+v", calls)
	}
	// Bind the account to its own egress through CPA's native field patch.
	status, body := h.management(http.MethodPatch, "/v0/management/auth-files/fields", managementKey, `{"name":"codex-a.json","proxy_url":"`+h.egress+`"}`)
	if status != http.StatusOK {
		t.Fatalf("field patch failed: %d %s", status, body)
	}
	h.eventually("the account egress to take effect", func() bool {
		h.resetObservations()
		if got := h.do(call{}); got.status != http.StatusOK {
			return false
		}
		calls := h.provider.seen()
		return len(calls) == 1 && calls[0].Via == "account-egress" && calls[0].Bearer == a.token
	})
}

func TestManagementBoundary(t *testing.T) {
	h := newHarness(t)
	h.addAccount("a", nil)
	h.start()
	for name, key := range map[string]string{"no key": "", "data-plane key": internalKey} {
		if status, _ := h.management(http.MethodGet, "/v0/management/auth-files", key, ""); status != http.StatusUnauthorized {
			t.Fatalf("%s opened the management API: %d", name, status)
		}
	}
	if status, _ := h.management(http.MethodGet, "/v0/management/auth-files", managementKey, ""); status != http.StatusOK {
		t.Fatalf("the management key was refused: %d", status)
	}
}
