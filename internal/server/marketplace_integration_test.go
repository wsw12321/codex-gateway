//go:build integration

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestMarketplaceSettingsHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	_, _, memberCookie := profileHTTPMember(t, f, "marketplace-member")
	valid := func(revision float64) map[string]any {
		return map[string]any{"enabled": true, "commission_rate": "0.1", "max_accounts_per_user": 5,
			"max_plans_per_user": 3, "allow_member_suppliers": false, "max_egress_servers_per_user": 4, "revision": revision}
	}
	f.send(t, "GET", "/admin/market/settings", nil, memberCookie, 403)
	f.send(t, "PUT", "/admin/market/settings", valid(1), memberCookie, 403)

	settings := invitationResponse(t, f.send(t, "GET", "/admin/market/settings", nil, f.cookie, 200))
	revision, _ := settings["revision"].(float64)
	if settings["enabled"] != false || settings["commission_rate"] != "0" || revision < 1 {
		t.Fatalf("default settings %v", settings)
	}
	// Restore the shared singleton for later tests in this database.
	t.Cleanup(func() {
		current, err := f.s.store.MarketplaceSettings(context.Background())
		if err != nil {
			t.Errorf("read settings: %v", err)
			return
		}
		if _, err := f.s.store.SetMarketplaceSettings(context.Background(), store.SetMarketplaceSettingsParams{
			CommissionRate: "0", MaxAccountsPerUser: 20, MaxPlansPerUser: 20, MaxEgressServersPerUser: 5,
			ExpectedRevision: current.Revision, ActorUserID: f.owner.ID,
		}); err != nil {
			t.Errorf("restore settings: %v", err)
		}
	})

	f.send(t, "PUT", "/admin/market/settings", valid(revision+1), f.cookie, 409)
	// Enabling needs account egress: member logins commit through it.
	f.send(t, "PUT", "/admin/market/settings", valid(revision), f.cookie, 409)
	f.s.config.EgressControlKey = strings.Repeat("k", 32)
	for _, bad := range []map[string]any{
		{"enabled": true, "revision": revision},
		{"enabled": "yes", "commission_rate": "0.1", "max_accounts_per_user": 5, "max_plans_per_user": 3, "allow_member_suppliers": false, "revision": revision},
		{"enabled": true, "commission_rate": "1.5", "max_accounts_per_user": 5, "max_plans_per_user": 3, "allow_member_suppliers": false, "revision": revision},
		{"enabled": true, "commission_rate": "0.1", "max_accounts_per_user": 257, "max_plans_per_user": 3, "allow_member_suppliers": false, "revision": revision},
		{"enabled": true, "commission_rate": "0.1", "max_accounts_per_user": 5, "max_plans_per_user": 3, "allow_member_suppliers": false, "max_egress_servers_per_user": 4, "revision": revision, "extra": 1},
		{"enabled": true, "commission_rate": "0.1", "max_accounts_per_user": 5, "max_plans_per_user": 3, "allow_member_suppliers": false, "max_egress_servers_per_user": 33, "revision": revision},
		{"enabled": true, "commission_rate": "0.1", "max_accounts_per_user": 5, "max_plans_per_user": 3, "allow_member_suppliers": false, "revision": revision},
	} {
		f.send(t, "PUT", "/admin/market/settings", bad, f.cookie, 400)
	}
	updated := invitationResponse(t, f.send(t, "PUT", "/admin/market/settings", valid(revision), f.cookie, 200))
	if updated["enabled"] != true || updated["commission_rate"] != "0.1" || updated["max_accounts_per_user"] != float64(5) ||
		updated["max_plans_per_user"] != float64(3) || updated["max_egress_servers_per_user"] != float64(4) || updated["revision"] != revision+1 {
		t.Fatalf("updated settings %v", updated)
	}
}
