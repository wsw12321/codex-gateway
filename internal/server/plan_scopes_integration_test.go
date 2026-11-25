//go:build integration

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// TestPlanScopesAndLayersHTTPPostgresIntegration covers stage 3 over HTTP:
// the Owner's plan provider scope, the member's self-use switch, the billing
// state view, and the internal per-layer eligibility, selection and attempt
// checks the plugin calls.
func TestPlanScopesAndLayersHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	alice, _, aliceCookie := profileHTTPMember(t, f, "scope-alice")
	bob, _, _ := profileHTTPMember(t, f, "scope-bob")
	f.s.config.SidecarToken = "sidecar-test-secret"
	write := func(actor string) store.BillingWriteParams {
		return store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: actor, Reason: "stage 3 integration", At: time.Now().UTC()}
	}
	login := func(owner, account string) {
		t.Helper()
		actor := f.owner.ID
		if owner != "" {
			actor = owner
		}
		registration, err := f.s.store.WithUpstreamOwner(owner).CreateEgressRegistration(ctx, "codex", nil, store.EgressActor{UserID: actor})
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
	plan, err := f.s.store.CreateBillingPlan(ctx, store.CreateBillingPlanParams{BillingWriteParams: write(f.owner.ID), Name: "Scoped",
		Tier: store.BillingTierDay, ModelIDs: []string{"gpt-test"}, PriceUSD: "1", AllowanceUSD: "5", MinPeriodCount: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	scopePath := "/admin/billing/plans/" + plan.ID + "/provider-scope"

	// Marketplace off: none of it exists, and the billing page is unchanged.
	f.send(t, "GET", "/admin/billing/plan-provider-scopes", nil, f.cookie, 404)
	f.send(t, "PUT", scopePath, map[string]any{"mode": "all", "account_ids": []string{}, "revision": 0}, f.cookie, 404)
	f.send(t, "PUT", "/admin/billing/me/self-use", map[string]any{"enabled": false}, aliceCookie, 404)
	if me := invitationResponse(t, f.send(t, "GET", "/admin/billing/me", nil, aliceCookie, 200)); me["self_use_enabled"] != nil {
		t.Fatalf("self-use shown while the marketplace is off: %v", me["self_use_enabled"])
	}

	f.s.config.EgressControlKey = strings.Repeat("k", 32)
	settings := invitationResponse(t, f.send(t, "GET", "/admin/market/settings", nil, f.cookie, 200))
	invitationResponse(t, f.send(t, "PUT", "/admin/market/settings", map[string]any{"enabled": true, "commission_rate": "0",
		"max_accounts_per_user": 4, "max_plans_per_user": 1, "allow_member_suppliers": false, "max_egress_servers_per_user": 1,
		"revision": settings["revision"]}, f.cookie, 200))

	const own, bobs, x, y = "a000000000000001", "b000000000000002", "c000000000000003", "d000000000000004"
	login(alice.ID, own)
	login(bob.ID, bobs)
	login("", x)
	login("", y)

	// The Owner scopes a platform plan; members cannot, and member accounts
	// cannot be listed.
	listing := invitationResponse(t, f.send(t, "GET", "/admin/billing/plan-provider-scopes", nil, f.cookie, 200))
	if scope := listing["scopes"].(map[string]any)[plan.ID].(map[string]any); scope["mode"] != "all" || scope["revision"] != float64(0) {
		t.Fatalf("default scope %v", scope)
	}
	var candidates []string
	for _, item := range listing["candidates"].([]any) {
		candidates = append(candidates, item.(map[string]any)["id"].(string))
	}
	if !slices.Equal(candidates, []string{x, y}) {
		t.Fatalf("scope candidates %v, want the platform accounts", candidates)
	}
	f.send(t, "GET", "/admin/billing/plan-provider-scopes", nil, aliceCookie, 403)
	f.send(t, "PUT", scopePath, map[string]any{"mode": "selected", "account_ids": []string{x}, "revision": 0}, aliceCookie, 403)
	f.send(t, "PUT", scopePath, map[string]any{"mode": "selected", "account_ids": []string{own}, "revision": 0}, f.cookie, 400)
	f.send(t, "PUT", scopePath, map[string]any{"mode": "selected", "account_ids": []string{}, "revision": 0}, f.cookie, 400)
	saved := invitationResponse(t, f.send(t, "PUT", scopePath, map[string]any{"mode": "selected", "account_ids": []string{x}, "revision": 0}, f.cookie, 200))
	if saved["mode"] != "selected" || len(saved["accounts"].([]any)) != 1 {
		t.Fatalf("saved scope %v", saved)
	}
	f.send(t, "PUT", scopePath, map[string]any{"mode": "all", "account_ids": []string{}, "revision": 0}, f.cookie, 409)
	f.send(t, "PUT", "/admin/billing/plans/"+uuid.NewString()+"/provider-scope", map[string]any{"mode": "all", "account_ids": []string{}, "revision": 0}, f.cookie, 404)

	// Alice buys the plan; her billing page shows self-use and the scope.
	if _, err := f.s.store.AdjustUserBalance(ctx, store.AdjustUserBalanceParams{BillingWriteParams: write(f.owner.ID), UserID: alice.ID, USDAmount: "3"}); err != nil {
		t.Fatal(err)
	}
	bought, err := f.s.store.PurchaseBillingPlan(ctx, store.PurchaseBillingPlanParams{BillingWriteParams: write(alice.ID),
		UserID: alice.ID, PlanID: plan.ID, PlanVersion: plan.Version, PeriodCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	subscription := bought.Subscription.ID
	me := invitationResponse(t, f.send(t, "GET", "/admin/billing/me", nil, aliceCookie, 200))
	if me["self_use_enabled"] != true {
		t.Fatalf("self-use %v", me["self_use_enabled"])
	}
	scope := me["subscriptions"].([]any)[0].(map[string]any)["provider_scope"].(map[string]any)
	if scope["restricted"] != true || scope["mode"] != "selected" || scope["account_counts"].(map[string]any)["codex"] != float64(1) {
		t.Fatalf("subscription scope %v", scope)
	}

	// The plugin's per-layer eligibility: each layer by its own rules.
	eligible := func(body string) map[string][]string {
		t.Helper()
		w := httptest.NewRecorder()
		f.s.eligibleUpstreamAccounts(w, allocationTestRequest(body))
		if w.Code != http.StatusOK {
			t.Fatalf("eligible status=%d body=%s", w.Code, w.Body)
		}
		var answer struct {
			Groups []struct {
				GroupID  string `json:"group_id"`
				Accounts []struct {
					ID string `json:"id"`
				} `json:"accounts"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		result := map[string][]string{}
		for _, group := range answer.Groups {
			result[group.GroupID] = []string{}
			for _, account := range group.Accounts {
				result[group.GroupID] = append(result[group.GroupID], account.ID)
			}
		}
		return result
	}
	all := `["` + own + `","` + bobs + `","` + x + `","` + y + `"]`
	got := eligible(`{"user_id":"` + alice.ID + `","groups":[` +
		`{"group_id":"` + store.SelfFundingLayerID + `","account_ids":` + all + `},` +
		`{"group_id":"` + subscription + `","account_ids":` + all + `},` +
		`{"group_id":"` + store.PersonalFundingLayerID + `","account_ids":` + all + `}]}`)
	if !slices.Equal(got[store.SelfFundingLayerID], []string{own}) || !slices.Equal(got[subscription], []string{x}) ||
		!slices.Equal(got[store.PersonalFundingLayerID], []string{x, y}) {
		t.Fatalf("per-layer eligibility %v", got)
	}
	// The list protocol stays platform-only.
	w := httptest.NewRecorder()
	f.s.eligibleUpstreamAccounts(w, allocationTestRequest(`{"user_id":"`+alice.ID+`","account_ids":`+all+`}`))
	if strings.Contains(w.Body.String(), own) || strings.Contains(w.Body.String(), bobs) {
		t.Fatalf("list eligibility admitted a member account: %s", w.Body)
	}
	w = httptest.NewRecorder()
	f.s.selectUpstreamAccount(w, allocationTestRequest(`{"user_id":"`+alice.ID+`","groups":[{"group_id":"`+store.SelfFundingLayerID+`","account_ids":["`+own+`","`+x+`"]}]}`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), own) {
		t.Fatalf("self layer selection status=%d body=%s", w.Code, w.Body)
	}

	// Self-use off: the own account is eligible nowhere.
	f.send(t, "PUT", "/admin/billing/me/self-use", map[string]any{"enabled": false}, f.cookie, 403)
	if off := invitationResponse(t, f.send(t, "PUT", "/admin/billing/me/self-use", map[string]any{"enabled": false}, aliceCookie, 200)); off["self_use_enabled"] != false {
		t.Fatalf("self-use %v", off)
	}
	if got := eligible(`{"user_id":"` + alice.ID + `","groups":[{"group_id":"` + store.SelfFundingLayerID + `","account_ids":["` + own + `"]}]}`); len(got[store.SelfFundingLayerID]) != 0 {
		t.Fatalf("own account eligible with self-use off: %v", got)
	}
	f.send(t, "PUT", "/admin/billing/me/self-use", map[string]any{"enabled": true}, aliceCookie, 200)

	// /attempt: a member's account is accepted only in the layer that lists
	// it and pays for it now.
	f.s.allocations = gatewayproxy.NewAllocationLedger()
	self := gatewayproxy.AllocationGroup{GroupID: store.SelfFundingLayerID, AccountIDs: []string{own}, Tenant: true}
	personal := gatewayproxy.AllocationGroup{GroupID: gatewayproxy.PersonalLayerID, AccountIDs: []string{}, AllAccounts: true}
	attempt := func(layers []gatewayproxy.AllocationGroup, account string) int {
		t.Helper()
		status := 0
		transport := attemptRoundTrip(func(r *http.Request) (*http.Response, error) {
			reply := func(body string) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/layer-select"):
				return reply(`{"protocol":"layer_select_v1"}`)
			case strings.HasSuffix(r.URL.Path, "/funding-layers"):
				return reply(`{"protocol":"group_upstream_account_layers_v1"}`)
			case strings.HasSuffix(r.URL.Path, "/capabilities"):
				return reply(`{"protocol":"upstream_account_access_v1","group_upstream_account_scope":"group_upstream_account_scope_v1"}`)
			}
			body := attemptBody(map[string]string{"request_token": r.Header.Get("X-Codex-Gateway-Request"), "user_id": alice.ID, "account_id": account})
			w := httptest.NewRecorder()
			f.s.recordUpstreamAttempt(w, attemptRequest(groupAllocationTestPath(store.UpstreamProviderCodex, "attempt"), body))
			status = w.Code
			return reply(`{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}`)
		})
		base, _ := url.Parse("http://compat.invalid")
		client := withManagement(gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}))
		client.UseAllocationLedger(f.s.allocations)
		incoming := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
		if _, failure := client.ForwardWithOptions(ctx, httptest.NewRecorder(), incoming, "/v1/responses", gatewayproxy.ForwardOptions{UserID: alice.ID, Groups: layers}); failure != nil {
			t.Fatalf("dispatch failed: %+v", failure)
		}
		return status
	}
	if status := attempt([]gatewayproxy.AllocationGroup{self, personal}, own); status != http.StatusNoContent {
		t.Fatalf("own account in the self layer = %d", status)
	}
	if status := attempt([]gatewayproxy.AllocationGroup{self, personal}, x); status != http.StatusNoContent {
		t.Fatalf("platform account under the personal layer = %d", status)
	}
	// Bob's account is in no layer; the personal layer admits platform
	// accounts only.
	if status := attempt([]gatewayproxy.AllocationGroup{self, personal}, bobs); status != http.StatusForbidden {
		t.Fatalf("another member's account = %d", status)
	}
	// Alice's own account smuggled into a layer it does not pay through.
	smuggled := gatewayproxy.AllocationGroup{GroupID: subscription, AccountIDs: []string{own, x}}
	if status := attempt([]gatewayproxy.AllocationGroup{smuggled}, own); status != http.StatusForbidden {
		t.Fatalf("own account in a plan layer = %d", status)
	}
	if status := attempt([]gatewayproxy.AllocationGroup{smuggled}, x); status != http.StatusNoContent {
		t.Fatalf("plan account in its layer = %d", status)
	}
}
