//go:build integration

package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestGroupMemberBillingHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	createMember := func(username string) (store.User, string) {
		t.Helper()
		user, err := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: username, DisplayName: username + " private name", Role: store.UserRoleMember})
		if err != nil {
			t.Fatal(err)
		}
		token, err := security.GenerateOpaqueToken(security.SessionToken)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := security.PepperTokenDigest(f.s.config.TokenPepper, token.Digest)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if _, err := f.s.store.CreateSession(ctx, store.CreateSessionParams{UserID: user.ID, TokenHash: digest[:], CSRFSecret: []byte(strings.Repeat("c", 32)), CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		return user, token.Token
	}
	self, memberCookie := createMember("billing-self")
	other, _ := createMember("billing-other")
	groupBody := func() map[string]any {
		return map[string]any{"operation_id": uuid.NewString(), "reason": "Group member HTTP regression", "name": "Member billing", "limit_usd": "20", "period": "day"}
	}
	for _, explicitNull := range []bool{false, true} {
		body := groupBody()
		if explicitNull {
			body["member_limit_usd"] = nil
		}
		created := invitationResponse(t, f.send(t, http.MethodPost, "/admin/groups", body, f.cookie, http.StatusOK))
		if value, exists := created["member_limit_usd"]; !exists || value != nil {
			t.Fatalf("omitted/null create must be unlimited: %+v", created)
		}
	}
	body := groupBody()
	body["member_limit_usd"] = "10"
	created := invitationResponse(t, f.send(t, http.MethodPost, "/admin/groups", body, f.cookie, http.StatusOK))
	groupID, periodID := created["id"].(string), created["period_id"].(string)
	if created["member_limit_usd"] != "10.000000000000" {
		t.Fatalf("created cap: %+v", created)
	}
	groupPath := "/admin/groups/" + groupID
	f.send(t, http.MethodPut, groupPath+"/members", map[string]any{"operation_id": uuid.NewString(), "reason": "Add billing members", "action": "add", "user_ids": []string{self.ID, other.ID}}, f.cookie, http.StatusOK)
	// Seed independent counters with distinct amounts to detect disclosure or
	// incorrectly returning the group's aggregate as this member's own usage.
	if _, err := f.s.store.DB().ExecContext(ctx, `INSERT INTO group_member_usage(period_id,user_id,used_usd) VALUES($1,$2,2),($1,$3,7)`, periodID, self.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.DB().ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=9 WHERE id=$1`, periodID); err != nil {
		t.Fatal(err)
	}
	assertOwnSummary := func(wantRemaining any) {
		t.Helper()
		w := f.send(t, http.MethodGet, "/admin/billing/me", nil, memberCookie, http.StatusOK)
		response := invitationResponse(t, w)
		if response["group_member_used_usd"] != "2.000000000000" || response["group_member_remaining_usd"] != wantRemaining {
			t.Fatalf("own member summary: %s", w.Body)
		}
		group := response["group"].(map[string]any)
		if group["id"] != groupID || group["used_usd"] != "9.000000000000" {
			t.Fatalf("group summary: %+v", group)
		}
		for _, private := range []string{other.ID, other.Username, other.DisplayName, `"members"`, `"member_usage"`, `"user_ids"`} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("own billing exposed another member's detail %q: %s", private, w.Body)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("private billing summary may be cached")
		}
	}
	assertOwnSummary("8.000000000000")
	f.send(t, http.MethodGet, "/admin/billing/users/"+other.ID, nil, memberCookie, http.StatusForbidden)
	f.send(t, http.MethodGet, groupPath, nil, memberCookie, http.StatusForbidden)
	for _, value := range []any{0, true, "", "-1", "0.1234567", "1000000000000000000"} {
		invalid := groupBody()
		invalid["member_limit_usd"] = value
		f.send(t, http.MethodPut, groupPath, invalid, f.cookie, http.StatusBadRequest)
	}
	updated := invitationResponse(t, f.send(t, http.MethodPut, groupPath, groupBody(), f.cookie, http.StatusOK))
	if updated["member_limit_usd"] != "10.000000000000" || updated["period_id"] != periodID || updated["used_usd"] != "9.000000000000" {
		t.Fatalf("omission must preserve cap and counters: %+v", updated)
	}
	assertOwnSummary("8.000000000000")
	for _, tc := range []struct {
		value     any
		cap       any
		remaining any
	}{
		{"1", "1.000000000000", "0.000000000000"},
		{nil, nil, nil},
		{"0", "0.000000000000", "0.000000000000"},
	} {
		edit := groupBody()
		edit["member_limit_usd"] = tc.value
		updated = invitationResponse(t, f.send(t, http.MethodPut, groupPath, edit, f.cookie, http.StatusOK))
		if updated["member_limit_usd"] != tc.cap || updated["period_id"] != periodID || updated["used_usd"] != "9.000000000000" {
			t.Fatalf("member cap edit changed counters: %+v", updated)
		}
		assertOwnSummary(tc.remaining)
	}
	for _, action := range []string{"remove", "add"} {
		f.send(t, http.MethodPut, groupPath+"/members", map[string]any{"operation_id": uuid.NewString(), "reason": "Rejoin billing member", "action": action, "user_ids": []string{self.ID}}, f.cookie, http.StatusOK)
	}
	assertOwnSummary("0.000000000000")
	detail := invitationResponse(t, f.send(t, http.MethodGet, groupPath, nil, f.cookie, http.StatusOK))
	members := detail["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("owner missing member details: %+v", detail)
	}
	wantUsed := map[string]string{self.ID: "2.000000000000", other.ID: "7.000000000000"}
	for _, value := range members {
		member := value.(map[string]any)
		if member["used_usd"] != wantUsed[member["user_id"].(string)] || member["remaining_usd"] != "0.000000000000" {
			t.Fatalf("owner member details: %+v", member)
		}
	}
	ungrouped := invitationResponse(t, f.send(t, http.MethodGet, "/admin/billing/me", nil, f.cookie, http.StatusOK))
	for _, key := range []string{"group_member_used_usd", "group_member_remaining_usd"} {
		if value, exists := ungrouped[key]; !exists || value != nil {
			t.Fatalf("ungrouped summary must expose null %s: %+v", key, ungrouped)
		}
	}
}
