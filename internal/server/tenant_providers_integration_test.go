//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// fakeTenantCPA plays CPA with the Gateway plugin for the member routes:
// account listing and control, the ownership protocol and credential deletion.
type fakeTenantCPA struct {
	mu        sync.Mutex
	ownership string
	accounts  []string
	deleted   []string
}

func (c *fakeTenantCPA) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	route := strings.TrimPrefix(r.URL.Path, pluginRoutePrefix)
	switch {
	case r.Method == http.MethodGet && route == "upstream-accounts/capabilities":
		_, _ = w.Write([]byte(`{"protocol":"upstream_account_access_v1","group_upstream_account_scope":"group_upstream_account_scope_v1","account_egress":"account_egress_v1"}`))
	case r.Method == http.MethodGet && route == "upstream-accounts":
		var accounts []map[string]string
		for _, id := range c.accounts {
			accounts = append(accounts, map[string]string{"id": id, "masked_email": "u***@example.com", "plan": "plus", "status": "available",
				"cliproxy_status": "active", "gateway_manual_status": "enabled", "gateway_quota_status": "available", "last_synced_at": "2026-11-24T00:00:00Z"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": accounts})
	case r.Method == http.MethodGet && route == "upstream-accounts/concurrency":
		var accounts []map[string]any
		for _, id := range c.accounts {
			accounts = append(accounts, map[string]any{"id": id, "active_requests": 0})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sampled_at": time.Now().UTC(), "accounts": accounts})
	case r.Method == http.MethodPut && route == "upstream-accounts/status":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": r.URL.Query().Get("id"), "status": "unavailable", "cliproxy_status": "active",
			"gateway_manual_status": "manual_disabled", "gateway_quota_status": "available"})
	case r.Method == http.MethodGet && route == "gateway-management/codex/ownership":
		if c.ownership == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"protocol": c.ownership})
	case r.Method == http.MethodDelete && route == "gateway-management/codex/accounts":
		id := r.URL.Query().Get("id")
		c.deleted = append(c.deleted, id)
		c.accounts = slices.DeleteFunc(c.accounts, func(value string) bool { return value == id })
		_, _ = w.Write([]byte(`{"status":"deleted"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}
}

func TestTenantProvidersHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	alice, _, aliceCookie := profileHTTPMember(t, f, "tenant-alice")
	bob, _, bobCookie := profileHTTPMember(t, f, "tenant-bob")
	cpa := &fakeTenantCPA{ownership: "credential_ownership_v1"}
	sidecar := httptest.NewServer(http.HandlerFunc(cpa.serve))
	t.Cleanup(sidecar.Close)
	base, _ := url.Parse(sidecar.URL)
	f.s.config.SidecarURL = base
	f.s.config.CPAManagementToken = testManagementKey
	f.s.config.SidecarToken = "sidecar-secret"
	f.s.config.APIKeyEncryptionKey = []byte(strings.Repeat("e", 32))
	f.s.upstream = withManagement(gatewayproxy.New(base, "sidecar-secret"))
	f.s.anthropic, f.s.antigravity = nil, nil

	// The switch is off: the member routes do not exist, and the Owner never
	// uses them.
	f.send(t, "GET", "/admin/me/providers", nil, aliceCookie, 404)
	f.send(t, "GET", "/admin/me/egress-servers", nil, aliceCookie, 404)
	f.send(t, "GET", "/admin/me/providers", nil, f.cookie, 403)
	if state := invitationResponse(t, f.send(t, "GET", "/admin/state", nil, aliceCookie, 200)); state["marketplace"].(map[string]any)["enabled"] != false {
		t.Fatalf("state %v", state["marketplace"])
	}

	// Turning it on needs account egress.
	settings := invitationResponse(t, f.send(t, "GET", "/admin/market/settings", nil, f.cookie, 200))
	enable := map[string]any{"enabled": true, "commission_rate": "0", "max_accounts_per_user": 2, "max_plans_per_user": 1,
		"allow_member_suppliers": false, "max_egress_servers_per_user": 1, "revision": settings["revision"]}
	f.send(t, "PUT", "/admin/market/settings", enable, f.cookie, 409)
	f.s.config.EgressControlKey = strings.Repeat("k", 32)
	invitationResponse(t, f.send(t, "PUT", "/admin/market/settings", enable, f.cookie, 200))
	state := invitationResponse(t, f.send(t, "GET", "/admin/state", nil, aliceCookie, 200))["marketplace"].(map[string]any)
	if state["enabled"] != true || state["max_accounts_per_user"] != float64(2) || state["max_egress_servers_per_user"] != float64(1) {
		t.Fatalf("member marketplace state %v", state)
	}
	f.send(t, "GET", "/admin/me/providers", nil, f.cookie, 403)

	// Accounts arrive through logins, as the plugin's registration commits them.
	login := func(owner, account string) {
		t.Helper()
		scope := f.s.store.WithUpstreamOwner(owner)
		actor := f.owner.ID
		if owner != "" {
			actor = owner
		}
		registration, err := scope.CreateEgressRegistration(ctx, "codex", nil, store.EgressActor{UserID: actor})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.store.PrepareEgressRegistration(ctx, registration.ID, "codex", account); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.store.CommitEgressRegistration(ctx, registration.ID, "codex", account); err != nil {
			t.Fatal(err)
		}
	}
	const aliceAccount, bobAccount, platformAccount = "a000000000000001", "b000000000000002", "c000000000000003"
	login(alice.ID, aliceAccount)
	login(bob.ID, bobAccount)
	login("", platformAccount)
	cpa.accounts = []string{aliceAccount, bobAccount, platformAccount}

	listed := func(cookie string) []string {
		t.Helper()
		response := invitationResponse(t, f.send(t, "GET", "/admin/me/providers", nil, cookie, 200))
		var ids []string
		for _, item := range response["accounts"].([]any) {
			account := item.(map[string]any)
			if account["can_manage"] != true || account["provider"] != "codex" {
				t.Fatalf("account %v", account)
			}
			ids = append(ids, account["id"].(string))
		}
		return ids
	}
	if ids := listed(aliceCookie); !slices.Equal(ids, []string{aliceAccount}) {
		t.Fatalf("alice sees %v", ids)
	}

	// A member manages only their own accounts; anything else does not exist.
	member := "/admin/me/providers/codex/accounts/"
	f.send(t, "PUT", member+aliceAccount+"/alias", map[string]any{"alias": "mine"}, aliceCookie, 200)
	for _, id := range []string{bobAccount, platformAccount} {
		f.send(t, "PUT", member+id+"/alias", map[string]any{"alias": "theirs"}, aliceCookie, 404)
		f.send(t, "PUT", member+id+"/concurrent-limit", map[string]any{"concurrent_limit": 2}, aliceCookie, 404)
		f.send(t, "PUT", member+id+"/status", map[string]any{"enabled": false}, aliceCookie, 404)
		f.send(t, "POST", member+id+"/refresh", map[string]any{}, aliceCookie, 404)
		f.send(t, "DELETE", member+id, map[string]any{}, aliceCookie, 404)
	}
	// The Owner's routes manage platform accounts only.
	f.send(t, "PUT", "/admin/upstream-accounts/"+aliceAccount+"/alias", map[string]any{"alias": "owner"}, f.cookie, 404)
	f.send(t, "PUT", "/admin/upstream-accounts/"+aliceAccount+"/status", map[string]any{"enabled": false}, f.cookie, 404)
	f.send(t, "PUT", "/admin/upstream-accounts/"+platformAccount+"/alias", map[string]any{"alias": "platform"}, f.cookie, 200)

	// The Owner sees tenant accounts on their own page, and moderates them.
	tenantPage := invitationResponse(t, f.send(t, "GET", "/admin/tenant-providers", nil, f.cookie, 200))
	if accounts := tenantPage["accounts"].([]any); len(accounts) != 2 {
		t.Fatalf("tenant accounts %v", accounts)
	}
	f.send(t, "GET", "/admin/tenant-providers", nil, aliceCookie, 403)
	f.send(t, "PUT", "/admin/tenant-providers/"+aliceAccount+"/suspension", map[string]any{"suspended": true}, aliceCookie, 403)
	suspended := invitationResponse(t, f.send(t, "PUT", "/admin/tenant-providers/"+aliceAccount+"/suspension", map[string]any{"suspended": true}, f.cookie, 200))
	if suspended["admin_suspended"] != true || suspended["owner_username"] != alice.Username {
		t.Fatalf("suspended %v", suspended)
	}
	f.send(t, "PUT", "/admin/tenant-providers/"+platformAccount+"/suspension", map[string]any{"suspended": true}, f.cookie, 404)
	f.send(t, "PUT", member+aliceAccount+"/status", map[string]any{"enabled": true}, aliceCookie, 409)
	f.send(t, "PUT", "/admin/tenant-providers/"+aliceAccount+"/suspension", map[string]any{"suspended": false}, f.cookie, 200)

	// Only the Owner transfers.
	transfer := map[string]any{"owner_user_id": alice.ID, "expected_owner_user_id": bob.ID}
	f.send(t, "PUT", "/admin/upstream-accounts/"+bobAccount+"/owner", transfer, aliceCookie, 403)
	f.send(t, "PUT", "/admin/upstream-accounts/"+bobAccount+"/owner", map[string]any{"owner_user_id": alice.ID, "expected_owner_user_id": alice.ID}, f.cookie, 409)
	f.send(t, "PUT", "/admin/upstream-accounts/"+bobAccount+"/owner", transfer, f.cookie, 200)
	if ids := listed(aliceCookie); !slices.Equal(ids, []string{aliceAccount, bobAccount}) && !slices.Equal(ids, []string{bobAccount, aliceAccount}) {
		t.Fatalf("alice after transfer sees %v", ids)
	}
	if ids := listed(bobCookie); len(ids) != 0 {
		t.Fatalf("bob after transfer sees %v", ids)
	}

	// Member egress nodes: public IPv4 only, within the limit, private to
	// their owner.
	node := func(server string) map[string]any {
		return map[string]any{"name": "home " + server, "protocol": "ss", "server": server, "port": 8388, "cipher": "aes-256-gcm",
			"username": "", "password": "secret-password", "udp": false}
	}
	f.send(t, "POST", "/admin/me/egress-servers", node("proxy.example.com"), aliceCookie, 400)
	f.send(t, "POST", "/admin/me/egress-servers", node("192.168.1.10"), aliceCookie, 400)
	created := invitationResponse(t, f.send(t, "POST", "/admin/me/egress-servers", node("8.8.8.8"), aliceCookie, 201))["server"].(map[string]any)
	nodeID := created["id"].(string)
	f.send(t, "POST", "/admin/me/egress-servers", node("8.8.4.4"), aliceCookie, 409)
	if servers := invitationResponse(t, f.send(t, "GET", "/admin/me/egress-servers", nil, bobCookie, 200))["servers"].([]any); len(servers) != 0 {
		t.Fatalf("bob sees nodes %v", servers)
	}
	f.send(t, "PUT", "/admin/me/egress-servers/"+nodeID, node("1.1.1.1"), bobCookie, 404)
	f.send(t, "DELETE", "/admin/me/egress-servers/"+nodeID, map[string]any{}, bobCookie, 404)
	for _, server := range invitationResponse(t, f.send(t, "GET", "/admin/egress-servers", nil, f.cookie, 200))["servers"].([]any) {
		if server.(map[string]any)["id"] == nodeID {
			t.Fatal("the Owner's node list shows a member's node")
		}
	}
	f.send(t, "PUT", member+aliceAccount+"/egress", map[string]any{"egress_server_id": nodeID}, aliceCookie, 200)
	// The Owner removes a member's node only once no account is bound to it;
	// members never reach the Owner's route.
	f.send(t, "DELETE", "/admin/tenant-providers/egress-servers/"+nodeID, map[string]any{}, f.cookie, 409)
	f.send(t, "DELETE", "/admin/tenant-providers/egress-servers/"+nodeID, map[string]any{}, aliceCookie, 403)
	f.send(t, "PUT", "/admin/upstream-accounts/"+platformAccount+"/egress", map[string]any{"egress_server_id": nodeID}, f.cookie, 404)

	// Logins check the plugin and the limit before they start.
	importBody := map[string]any{"refresh_token": "rt-secret"}
	f.send(t, "POST", "/admin/me/providers/codex/credentials", importBody, aliceCookie, 409)
	cpa.mu.Lock()
	cpa.ownership = ""
	cpa.mu.Unlock()
	f.send(t, "POST", "/admin/me/providers/codex/credentials", importBody, aliceCookie, 503)

	// The plugin learns why a registration was refused.
	registration, err := f.s.store.WithUpstreamOwner(bob.ID).CreateEgressRegistration(ctx, "codex", nil, store.EgressActor{UserID: bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	refusal := httptest.NewRequest(http.MethodPost, "/internal/upstream-accounts/register-egress",
		strings.NewReader(`{"registration_id":"`+registration.ID+`","provider":"codex","account_id":"`+platformAccount+`"}`))
	refusal.Header.Set("Authorization", "Bearer sidecar-secret")
	refusal.Header.Set("Content-Type", "application/json")
	refused := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(refused, refusal)
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"account_owned_elsewhere"`) {
		t.Fatalf("refused registration: %d %s", refused.Code, refused.Body.String())
	}

	// Deleting drains the account in CPA, removes the credential and releases
	// the account.
	f.send(t, "DELETE", member+aliceAccount, map[string]any{}, aliceCookie, 200)
	if !slices.Contains(cpa.deleted, aliceAccount) {
		t.Fatalf("credential not deleted in CPA: %v", cpa.deleted)
	}
	ownership, err := f.s.store.UpstreamAccountOwnership(ctx, aliceAccount)
	if err != nil || !ownership.CredentialRemoved {
		t.Fatalf("released ownership=%+v err=%v", ownership, err)
	}
	if ids := listed(aliceCookie); !slices.Equal(ids, []string{bobAccount}) {
		t.Fatalf("alice after deletion sees %v", ids)
	}
	if count := invitationResponse(t, f.send(t, "GET", "/admin/me/providers", nil, aliceCookie, 200))["account_count"]; count != float64(1) {
		t.Fatalf("account count %v", count)
	}
	// Releasing the account unbound the node: the Owner can now delete it,
	// for example before deleting a disabled member.
	tenantNodes := invitationResponse(t, f.send(t, "GET", "/admin/tenant-providers", nil, f.cookie, 200))["egress_servers"].([]any)
	if len(tenantNodes) != 1 || tenantNodes[0].(map[string]any)["owner_status"] != "active" || tenantNodes[0].(map[string]any)["bound_accounts"] != float64(0) {
		t.Fatalf("tenant nodes before deletion = %v", tenantNodes)
	}
	f.send(t, "DELETE", "/admin/tenant-providers/egress-servers/"+nodeID, map[string]any{}, f.cookie, 200)
	f.send(t, "DELETE", "/admin/tenant-providers/egress-servers/"+nodeID, map[string]any{}, f.cookie, 404)
	if servers := invitationResponse(t, f.send(t, "GET", "/admin/me/egress-servers", nil, aliceCookie, 200))["servers"].([]any); len(servers) != 0 {
		t.Fatalf("alice still has nodes %v", servers)
	}
	var deletedBy string
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT actor_user_id::text FROM audit_events WHERE event_type='egress_server.deleted' AND subject_id=$1`, nodeID).Scan(&deletedBy); err != nil || deletedBy != f.owner.ID {
		t.Fatalf("node deletion audit actor = %q, %v", deletedBy, err)
	}

	// Switching the marketplace off closes the member routes again.
	current := invitationResponse(t, f.send(t, "GET", "/admin/market/settings", nil, f.cookie, 200))
	enable["enabled"], enable["revision"] = false, current["revision"]
	f.send(t, "PUT", "/admin/market/settings", enable, f.cookie, 200)
	f.send(t, "GET", "/admin/me/providers", nil, aliceCookie, 404)
	f.send(t, "PUT", member+bobAccount+"/alias", map[string]any{"alias": "x"}, aliceCookie, 404)
}
