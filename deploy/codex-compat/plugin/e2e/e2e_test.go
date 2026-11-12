//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/base64"
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
		h.skipEgressWait = true // an unidentified credential is never routed
		h.start()
		got := h.do(call{})
		if got.status != http.StatusServiceUnavailable || len(h.provider.seen()) != 0 {
			t.Fatalf("status %d with %d provider calls: %s", got.status, len(h.provider.seen()), got.body)
		}
	})
}

// TestPerAccountEgress follows an Owner binding an account to its own egress.
// The Gateway is the only source of the route: the plugin writes it into the
// credential, and CPA then uses it for every call on that account.
func TestPerAccountEgress(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", map[string]any{"proxy_url": "http://planted.example:8080"})
	h.start()

	// CPA has no proxy of its own, so every credential carries its route
	// explicitly once the plugin's start-up pass has run: a, which came with
	// no proxy, and b, whose planted proxy the Gateway never bound, both carry
	// the global egress before either serves anything.
	for _, name := range []string{"a", "b"} {
		if proxy := h.credentialProxy(name); proxy != h.proxy {
			t.Fatalf("%s carries %q, want the global egress written out", name, proxy)
		}
	}
	h.gateway.prefer = []string{b.id, a.id}
	h.resetObservations()
	if got := h.do(call{}); got.status != http.StatusOK {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	if calls := h.provider.seen(); len(calls) != 1 || calls[0].Bearer != b.token || calls[0].Via != "global" {
		t.Fatalf("b on the global egress: %+v", calls)
	}

	// The Owner binds a to its own egress. The Gateway pushes the binding.
	routeA := h.route("route-a", "secret-a")
	h.gateway.routes[a.id] = routeA
	status, body := h.management(http.MethodPut, "/v0/management/gateway/upstream-accounts/egress?id="+a.id, managementKey, "{}")
	if status != http.StatusOK || strings.Contains(body, "secret-a") || !strings.Contains(body, `"revision":"route:`) {
		t.Fatalf("egress sync: %d %s", status, body)
	}
	if proxy := h.credentialProxy("a"); proxy != routeA {
		t.Fatalf("a's credential carries %q", proxy)
	}
	h.gateway.prefer = []string{a.id, b.id}
	h.resetObservations()
	h.do(call{})
	calls := h.provider.seen()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("route-a:secret-a"))
	if len(calls) != 1 || calls[0].Bearer != a.token || calls[0].Via != "account-egress" || calls[0].ProxyAuth != wantAuth {
		t.Fatalf("a after binding: %+v", calls)
	}

	// A rebinding the push never reached is caught on the next pick: a is
	// skipped once while its new route is written, then used through it.
	routeA2 := h.route("route-a2", "secret-a2")
	h.gateway.routes[a.id] = routeA2
	h.resetObservations()
	h.do(call{})
	if served := h.served(); len(served) != 1 || served[0] != "b" {
		t.Fatalf("provider saw %v while a carried a stale route", served)
	}
	h.resetObservations()
	h.do(call{})
	calls = h.provider.seen()
	if len(calls) != 1 || calls[0].Bearer != a.token || calls[0].ProxyAuth != "Basic "+base64.StdEncoding.EncodeToString([]byte("route-a2:secret-a2")) {
		t.Fatalf("a after rebinding: %+v", calls)
	}

	// The route survives a restart: it lives in the credential.
	h.stop()
	h.start()
	h.resetObservations()
	h.do(call{})
	if calls = h.provider.seen(); len(calls) != 1 || calls[0].Bearer != a.token || calls[0].Via != "account-egress" {
		t.Fatalf("a after restart: %+v", calls)
	}

	// When the Gateway cannot confirm any route, nothing is sent at all.
	h.gateway.egressDown = true
	h.resetObservations()
	if got := h.do(call{}); got.status != http.StatusServiceUnavailable || len(h.provider.seen()) != 0 {
		t.Fatalf("status %d with %d provider calls while routes were unknown", got.status, len(h.provider.seen()))
	}
	h.gateway.egressDown = false

	// Unbinding returns a to the global egress.
	delete(h.gateway.routes, a.id)
	if status, body = h.management(http.MethodPut, "/v0/management/gateway/upstream-accounts/egress?id="+a.id, managementKey, ""); status != http.StatusOK {
		t.Fatalf("egress sync: %d %s", status, body)
	}
	if proxy := h.credentialProxy("a"); proxy != h.proxy {
		t.Fatalf("a carries %q after unbinding, want the global egress written out", proxy)
	}
	h.resetObservations()
	h.do(call{})
	if calls = h.provider.seen(); len(calls) != 1 || calls[0].Bearer != a.token || calls[0].Via != "global" {
		t.Fatalf("a after unbinding: %+v", calls)
	}
	// The management key is required to change a route.
	if status, _ = h.management(http.MethodPut, "/v0/management/gateway/upstream-accounts/egress?id="+a.id, internalKey, ""); status != http.StatusUnauthorized {
		t.Fatalf("the data-plane key changed a route: %d", status)
	}
}

// TestDeadEgressNeverFallsBack binds an account to an egress node that is
// down. The account's request fails at its own listener and is never sent
// through the global egress or directly; the scheduler may still serve the
// request with another authorized account through that account's egress.
func TestDeadEgressNeverFallsBack(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	dead := "http://" + deadRouteUser + ":secret-dead@" + strings.TrimPrefix(h.egress, "http://")
	h.gateway.routes[a.id] = dead
	h.start()
	if proxy := h.credentialProxy("a"); proxy != dead {
		t.Fatalf("a carries %q", proxy)
	}
	noFallback := func(what string, attempted bool) {
		t.Helper()
		for _, call := range h.provider.seen() {
			if call.Bearer == a.token {
				t.Fatalf("%s: a reached the provider via %s: %+v", what, call.Via, call)
			}
		}
		refused := h.provider.refusedConnects()
		if attempted && len(refused) == 0 {
			t.Fatalf("%s: a was never attempted through its own egress", what)
		}
		for _, connect := range refused {
			if connect.Via != "account-egress" {
				t.Fatalf("%s: a's route was presented to %s", what, connect.Via)
			}
		}
	}

	// Only a is authorized: the request fails rather than leaving another way.
	h.gateway.permitted[userAlice] = map[string]bool{a.id: true}
	h.gateway.prefer = []string{a.id}
	h.resetObservations()
	if got := h.do(call{}); got.status < 500 {
		t.Fatalf("status %d with a's egress down: %s", got.status, got.body)
	}
	noFallback("a alone", true)

	// With b authorized as well, b serves through its own (global) egress.
	// CPA may skip a altogether now that it has failed; if it tries a again,
	// that attempt again ends at a's own listener.
	delete(h.gateway.permitted, userAlice)
	h.gateway.prefer = []string{a.id, b.id}
	h.resetObservations()
	if got := h.do(call{}); got.status != http.StatusOK {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	noFallback("a then b", false)
	if served := h.served(); len(served) != 1 || served[0] != "b" || h.provider.seen()[0].Via != "global" {
		t.Fatalf("provider saw %v", h.provider.seen())
	}
}

// TestAccountManagement drives the plugin's account routes exactly as the
// Gateway does: behind the management key, with the account id as a query
// parameter.
func TestAccountManagement(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()
	const base = "/v0/management/gateway/upstream-accounts"

	for name, key := range map[string]string{"no key": "", "data-plane key": internalKey} {
		if status, _ := h.management(http.MethodGet, base+"/capabilities", key, ""); status != http.StatusUnauthorized {
			t.Fatalf("%s reached the plugin's routes: %d", name, status)
		}
	}
	status, body := h.management(http.MethodGet, base+"/capabilities", managementKey, "")
	if status != http.StatusOK || !strings.Contains(body, `"protocol":"upstream_account_access_v1"`) || !strings.Contains(body, `"account_egress":"account_egress_v1"`) {
		t.Fatalf("capabilities: %d %s", status, body)
	}
	if status, body = h.management(http.MethodGet, "/v0/management/gateway/anthropic-accounts/capabilities", managementKey, ""); status != http.StatusOK || !strings.Contains(body, "anthropic_messages_v1") {
		t.Fatalf("anthropic capabilities: %d %s", status, body)
	}

	status, body = h.management(http.MethodGet, base, managementKey, "")
	var listing struct {
		Accounts []map[string]string `json:"accounts"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &listing) != nil || len(listing.Accounts) != 2 {
		t.Fatalf("list: %d %s", status, body)
	}
	for _, row := range listing.Accounts {
		name := "a"
		if row["id"] == b.id {
			name = "b"
		} else if row["id"] != a.id {
			t.Fatalf("unexpected account %v", row)
		}
		if len(row) != 8 || row["masked_email"] != name+"***@example.com" || row["plan"] != "plus" || row["status"] != "available" || row["cliproxy_status"] != "active" {
			t.Fatalf("account row %v", row)
		}
	}
	for _, secret := range []string{a.token, "refresh-a", "acct-a", "a@example.com"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the account listing leaks %q", secret)
		}
	}

	// Disabling an account takes it out of allocation at once and durably.
	status, body = h.management(http.MethodPut, base+"/status?id="+a.id, managementKey, `{"enabled":false}`)
	if status != http.StatusOK || !strings.Contains(body, `"gateway_manual_status":"manual_disabled"`) || !strings.Contains(body, `"status":"unavailable"`) {
		t.Fatalf("disable: %d %s", status, body)
	}
	h.resetObservations()
	h.gateway.prefer = []string{a.id, b.id}
	for i := 0; i < 3; i++ {
		if got := h.do(call{}); got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
	}
	for _, name := range h.served() {
		if name != "b" {
			t.Fatalf("a disabled account served a request: %v", h.served())
		}
	}
	h.stop()
	h.start()
	h.resetObservations()
	h.do(call{})
	if served := h.served(); len(served) != 1 || served[0] != "b" {
		t.Fatalf("the disable did not survive a restart: %v", served)
	}
	status, body = h.management(http.MethodPut, base+"/status?id="+a.id, managementKey, `{"enabled":true}`)
	if status != http.StatusOK || !strings.Contains(body, `"status":"available"`) {
		t.Fatalf("enable: %d %s", status, body)
	}
	h.resetObservations()
	h.gateway.prefer = []string{a.id}
	h.do(call{})
	if served := h.served(); len(served) != 1 || served[0] != "a" {
		t.Fatalf("a re-enabled account was not used: %v", served)
	}

	for name, request := range map[string][2]string{
		"unknown account": {base + "/status?id=0123456789abcdef", `{"enabled":true}`},
		"missing id":      {base + "/status", `{"enabled":true}`},
	} {
		if status, _ = h.management(http.MethodPut, request[0], managementKey, request[1]); status != http.StatusNotFound {
			t.Fatalf("%s: status %d", name, status)
		}
	}
	if status, _ = h.management(http.MethodPut, base+"/status?id="+a.id, managementKey, `{"enabled":"yes"}`); status != http.StatusBadRequest {
		t.Fatalf("a malformed status request was accepted: %d", status)
	}

	// The snapshot counts the conversation the last request left held.
	status, body = h.management(http.MethodGet, base+"/concurrency", managementKey, "")
	var snapshot struct {
		SampledAt time.Time `json:"sampled_at"`
		Accounts  []struct {
			ID              string `json:"id"`
			ActiveRequests  *int64 `json:"active_requests"`
			ConcurrentLimit int64  `json:"concurrent_limit"`
		} `json:"accounts"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &snapshot) != nil || len(snapshot.Accounts) != 2 || time.Since(snapshot.SampledAt) > time.Minute {
		t.Fatalf("concurrency: %d %s", status, body)
	}
	for _, row := range snapshot.Accounts {
		if row.ActiveRequests == nil || *row.ActiveRequests < 0 || row.ConcurrentLimit < 1 {
			t.Fatalf("concurrency row %+v in %s", row, body)
		}
	}
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
