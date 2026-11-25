//go:build integration

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/store"
)

// TestMarketplacePlansHTTPPostgresIntegration covers stage 4 over HTTP: a
// member publishes a plan, the Owner reviews it, another member buys it from
// the marketplace, and each side sees its own view of the sale.
func TestMarketplacePlansHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	alice, _, aliceCookie := profileHTTPMember(t, f, "market-alice")
	bob, _, bobCookie := profileHTTPMember(t, f, "market-bob")
	write := func(actor string) store.BillingWriteParams {
		return store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: actor, Reason: "stage 4 integration", At: time.Now().UTC()}
	}
	planBody := func(price string, active bool) map[string]any {
		return map[string]any{"operation_id": uuid.NewString(), "name": "Alice Pro", "description": "官方账号，每天 10 美元额度",
			"price_usd": price, "tier": "day", "period_days": 1, "allowance_usd": "10", "model_ids": []string{"gpt-test"},
			"min_period_count": 1, "active": active}
	}

	// Marketplace off: none of the routes exist.
	for _, path := range []string{"/admin/market/plans", "/admin/me/plans", "/admin/me/sales"} {
		f.send(t, "GET", path, nil, aliceCookie, 404)
	}
	f.send(t, "POST", "/admin/me/plans", planBody("2", true), aliceCookie, 404)
	f.send(t, "GET", "/admin/market/reviews", nil, f.cookie, 404)
	f.send(t, "GET", "/admin/market/sales", nil, f.cookie, 404)

	f.s.config.EgressControlKey = strings.Repeat("k", 32)
	settings := invitationResponse(t, f.send(t, "GET", "/admin/market/settings", nil, f.cookie, 200))
	invitationResponse(t, f.send(t, "PUT", "/admin/market/settings", map[string]any{"enabled": true, "commission_rate": "0.2",
		"max_accounts_per_user": 4, "max_plans_per_user": 2, "allow_member_suppliers": false, "max_egress_servers_per_user": 1,
		"revision": settings["revision"]}, f.cookie, 200))

	// A member publishes a draft; the Owner's plans live elsewhere.
	f.send(t, "POST", "/admin/me/plans", planBody("2", true), f.cookie, 403)
	bad := planBody("2", true)
	bad["model_ids"] = []string{}
	f.send(t, "POST", "/admin/me/plans", bad, aliceCookie, 400)
	bad = planBody("2", true)
	bad["seller_user_id"] = bob.ID
	f.send(t, "POST", "/admin/me/plans", bad, aliceCookie, 400)
	created := invitationResponse(t, f.send(t, "POST", "/admin/me/plans", planBody("2", true), aliceCookie, 200))
	planID := created["id"].(string)
	if created["review_status"] != "draft" || created["on_sale"] != false || created["seller_user_id"] != alice.ID {
		t.Fatalf("created plan %v", created)
	}
	mine := invitationResponse(t, f.send(t, "GET", "/admin/me/plans", nil, aliceCookie, 200))
	if plans := mine["plans"].([]any); len(plans) != 1 || mine["scopes"].(map[string]any)[planID] == nil {
		t.Fatalf("my plans %v", mine)
	}
	if plans := invitationResponse(t, f.send(t, "GET", "/admin/me/plans", nil, bobCookie, 200))["plans"].([]any); len(plans) != 0 {
		t.Fatalf("bob sees alice's plans: %v", plans)
	}

	// Submission and review.
	version := created["version"].(float64)
	f.send(t, "POST", "/admin/me/plans/"+planID+"/submit", map[string]any{"version": version}, bobCookie, 404)
	submitted := invitationResponse(t, f.send(t, "POST", "/admin/me/plans/"+planID+"/submit", map[string]any{"version": version}, aliceCookie, 200))
	if submitted["review_status"] != "pending" {
		t.Fatalf("submitted %v", submitted)
	}
	f.send(t, "POST", "/admin/me/plans/"+planID+"/submit", map[string]any{"version": version}, aliceCookie, 409)
	for _, path := range []string{"/admin/market/reviews", "/admin/market/sales"} {
		f.send(t, "GET", path, nil, aliceCookie, 403)
	}
	f.send(t, "POST", "/admin/market/reviews/"+planID, map[string]any{"decision": "approve", "note": "", "version": version}, aliceCookie, 403)
	f.send(t, "PUT", "/admin/market/plans/"+planID+"/suspend", map[string]any{"suspended": true, "reason": "x"}, aliceCookie, 403)
	reviews := invitationResponse(t, f.send(t, "GET", "/admin/market/reviews", nil, f.cookie, 200))
	if pending := reviews["pending"].([]any); len(pending) != 1 || pending[0].(map[string]any)["seller_name"] != "Original display name" {
		t.Fatalf("review queue %v", reviews)
	}
	f.send(t, "POST", "/admin/market/reviews/"+planID, map[string]any{"decision": "maybe", "note": "", "version": version}, f.cookie, 400)
	f.send(t, "POST", "/admin/market/reviews/"+planID, map[string]any{"decision": "approve", "note": "", "version": version + 1}, f.cookie, 409)
	approved := invitationResponse(t, f.send(t, "POST", "/admin/market/reviews/"+planID, map[string]any{"decision": "approve", "note": "", "version": version}, f.cookie, 200))
	if approved["review_status"] != "approved" || approved["on_sale"] != true {
		t.Fatalf("approved %v", approved)
	}

	// The marketplace shows it to everyone, marked as Alice's own for her.
	findPlan := func(cookie string) map[string]any {
		t.Helper()
		for _, item := range invitationResponse(t, f.send(t, "GET", "/admin/market/plans", nil, cookie, 200))["plans"].([]any) {
			if plan := item.(map[string]any); plan["id"] == planID {
				return plan
			}
		}
		return nil
	}
	if plan := findPlan(bobCookie); plan == nil || plan["own"] != false || plan["platform"] != false || plan["seller_name"] != "Original display name" || plan["third_party_api"] != false {
		t.Fatalf("market plan for bob %v", plan)
	}
	if plan := findPlan(aliceCookie); plan == nil || plan["own"] != true {
		t.Fatalf("market plan for alice %v", plan)
	}

	// Bob buys it; Alice cannot buy her own.
	if _, err := f.s.store.AdjustUserBalance(ctx, store.AdjustUserBalanceParams{BillingWriteParams: write(f.owner.ID), UserID: bob.ID, USDAmount: "10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.AdjustUserBalance(ctx, store.AdjustUserBalanceParams{BillingWriteParams: write(f.owner.ID), UserID: alice.ID, USDAmount: "10"}); err != nil {
		t.Fatal(err)
	}
	purchase := func(cookie string, status int) map[string]any {
		t.Helper()
		return invitationResponse(t, f.send(t, "POST", "/admin/billing/me/purchases", map[string]any{"operation_id": uuid.NewString(),
			"plan_id": planID, "plan_version": version, "period_count": 1, "acknowledge_third_party": false}, cookie, status))
	}
	if failure := purchase(aliceCookie, 400); failure["error"].(map[string]any)["code"] != "self_purchase" {
		t.Fatalf("self purchase %v", failure)
	}
	bought := purchase(bobCookie, 200)
	if bought["total_usd"] != "2.000000000000" {
		t.Fatalf("purchase %v", bought)
	}

	// Alice sees the sale with an anonymous buyer; the Owner sees everyone.
	sales := f.send(t, "GET", "/admin/me/sales", nil, aliceCookie, 200)
	if strings.Contains(sales.Body.String(), bob.ID) || strings.Contains(sales.Body.String(), "market-bob") {
		t.Fatalf("seller sales reveal the buyer: %s", sales.Body)
	}
	mySales := invitationResponse(t, sales)
	sale := mySales["sales"].([]any)[0].(map[string]any)
	if sale["buyer_number"] != float64(1) || sale["net_usd"] != "1.600000000000" || sale["commission_usd"] != "0.400000000000" ||
		mySales["summary"].(map[string]any)["net_usd"] != "1.600000000000" {
		t.Fatalf("seller sales %v", mySales)
	}
	f.send(t, "GET", "/admin/me/sales?limit=0", nil, aliceCookie, 400)
	all := invitationResponse(t, f.send(t, "GET", "/admin/market/sales", nil, f.cookie, 200))
	if ownerSale := all["sales"].([]any)[0].(map[string]any); ownerSale["buyer_user_id"] != bob.ID || ownerSale["seller_user_id"] != alice.ID {
		t.Fatalf("Owner sales %v", all)
	}
	usage := invitationResponse(t, f.send(t, "GET", "/admin/me/plans/"+planID+"/usage", nil, aliceCookie, 200))
	if usage["subscribers"] != float64(1) {
		t.Fatalf("usage %v", usage)
	}
	f.send(t, "GET", "/admin/me/plans/"+planID+"/usage", nil, bobCookie, 404)

	// Provider scope: the seller's own plan only.
	scopeBody := map[string]any{"mode": "all", "account_ids": []string{}, "revision": 0}
	f.send(t, "PUT", "/admin/me/plans/"+planID+"/provider-scope", scopeBody, bobCookie, 404)
	f.send(t, "PUT", "/admin/me/plans/"+planID+"/provider-scope", scopeBody, aliceCookie, 200)

	// A price change returns the plan to review; listing does not.
	current := invitationResponse(t, f.send(t, "GET", "/admin/me/plans", nil, aliceCookie, 200))["plans"].([]any)[0].(map[string]any)
	edit := planBody("3", true)
	edit["version"] = current["version"]
	edited := invitationResponse(t, f.send(t, "PUT", "/admin/me/plans/"+planID, edit, aliceCookie, 200))
	if edited["review_status"] != "pending" || edited["on_sale"] != false || findPlan(bobCookie) != nil {
		t.Fatalf("edited %v", edited)
	}
	approved = invitationResponse(t, f.send(t, "POST", "/admin/market/reviews/"+planID, map[string]any{"decision": "approve", "note": "", "version": edited["version"]}, f.cookie, 200))
	delisted := invitationResponse(t, f.send(t, "PUT", "/admin/me/plans/"+planID+"/listing", map[string]any{"operation_id": uuid.NewString(),
		"version": approved["version"], "active": false}, aliceCookie, 200))
	if delisted["review_status"] != "approved" || delisted["on_sale"] != false {
		t.Fatalf("delisted %v", delisted)
	}

	// The Owner takes it down; Alice cannot list it again.
	f.send(t, "PUT", "/admin/market/plans/"+planID+"/suspend", map[string]any{"suspended": true, "reason": ""}, f.cookie, 400)
	suspended := invitationResponse(t, f.send(t, "PUT", "/admin/market/plans/"+planID+"/suspend", map[string]any{"suspended": true, "reason": "描述与实际不符"}, f.cookie, 200))
	if suspended["admin_suspended"] != true {
		t.Fatalf("suspended %v", suspended)
	}
	failure := invitationResponse(t, f.send(t, "PUT", "/admin/me/plans/"+planID+"/listing", map[string]any{"operation_id": uuid.NewString(),
		"version": suspended["version"], "active": true}, aliceCookie, 409))
	if failure["error"].(map[string]any)["code"] != "seller_plan_suspended" {
		t.Fatalf("relist suspended %v", failure)
	}

	// Deleting: Bob cannot, Alice can.
	f.send(t, "DELETE", "/admin/me/plans/"+planID, map[string]any{"operation_id": uuid.NewString(), "version": suspended["version"]}, bobCookie, 404)
	f.send(t, "DELETE", "/admin/me/plans/"+planID, map[string]any{"operation_id": uuid.NewString(), "version": suspended["version"]}, aliceCookie, 200)

	// The plan limit applies per seller.
	invitationResponse(t, f.send(t, "POST", "/admin/me/plans", planBody("1", false), aliceCookie, 200))
	invitationResponse(t, f.send(t, "POST", "/admin/me/plans", planBody("1", false), aliceCookie, 200))
	if failure := invitationResponse(t, f.send(t, "POST", "/admin/me/plans", planBody("1", false), aliceCookie, 409)); failure["error"].(map[string]any)["code"] != "seller_plan_limit" {
		t.Fatalf("plan limit %v", failure)
	}

	// The Owner's platform plan list never shows seller plans.
	for _, item := range invitationResponse(t, f.send(t, "GET", "/admin/billing/plans?include_inactive=true", nil, f.cookie, 200))["plans"].([]any) {
		if item.(map[string]any)["name"] == "Alice Pro" {
			t.Fatal("platform plan list shows a seller plan")
		}
	}
}
