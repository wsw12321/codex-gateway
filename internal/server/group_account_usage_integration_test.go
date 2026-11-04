//go:build integration

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestGroupAccountUsageHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	member, _, memberCookie := profileHTTPMember(t, f, "group-usage-member")
	claudeA, claudeB, codex := "aaaabbbbccccdddd", "aaaabbbbcccceeee", "0123456789abcdef"
	now := time.Now().UTC()
	for provider, ids := range map[string][]string{store.UpstreamProviderAnthropic: {claudeA, claudeB}, store.UpstreamProviderCodex: {codex}} {
		snapshots := make([]store.UpstreamAccountSnapshot, 0, len(ids))
		for _, id := range ids {
			snapshots = append(snapshots, store.UpstreamAccountSnapshot{ID: id, MaskedEmail: "u***@example.com", Plan: "pro", Status: store.UpstreamAccountStatusAvailable, LastSyncedAt: now})
		}
		if err := f.s.store.WithUpstreamProvider(provider).SyncUpstreamAccounts(ctx, snapshots, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.store.WithUpstreamProvider(store.UpstreamProviderAnthropic).SetUpstreamAccountAlias(ctx, store.SetUpstreamAccountAliasParams{AccountID: claudeA, Alias: "Claude-A", ActorUserID: f.owner.ID, At: now}); err != nil {
		t.Fatal(err)
	}
	reads := &countingAccountUsageReader{read: func(_, accountID string) (accountUsage, string) {
		used := 42
		return accountUsage{Source: "observed", FiveHour: &accountUsageWindow{UsedPercent: &used}}, ""
	}}
	f.s.accountUsageRead = reads.reader()

	created := invitationResponse(t, f.send(t, "POST", "/admin/groups", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Member usage HTTP regression", "name": "Usage group", "limit_usd": "20", "period": "day",
	}, f.cookie, 200))
	groupID := created["id"].(string)
	if created["account_usage_visible"] != false {
		t.Fatalf("new group exposes usage: %+v", created)
	}
	f.send(t, "PUT", "/admin/groups/"+groupID+"/members", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Add member", "action": "add", "user_ids": []string{member.ID},
	}, f.cookie, 200)
	f.send(t, "PUT", "/admin/groups/"+groupID+"/upstream-accounts/anthropic", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Two Claude accounts", "mode": "selected", "account_ids": []string{claudeB, claudeA},
	}, f.cookie, 200)

	usage := func(cookie string) myAccountUsageResponse {
		t.Helper()
		w := f.send(t, "GET", "/admin/billing/me/account-usage", nil, cookie, 200)
		for _, id := range []string{claudeA, claudeB, codex, "u***@example.com"} {
			if strings.Contains(w.Body.String(), id) {
				t.Fatalf("account usage exposed %q: %s", id, w.Body.String())
			}
		}
		var response myAccountUsageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if got := usage(memberCookie); len(got.Groups) != 0 {
		t.Fatalf("disabled group exposed usage: %+v", got)
	}
	visibility := func(cookie string, enabled bool, status int) {
		t.Helper()
		f.send(t, "PUT", "/admin/groups/"+groupID+"/account-usage-visibility", map[string]any{
			"operation_id": uuid.NewString(), "reason": "Let members see Claude usage", "enabled": enabled,
		}, cookie, status)
	}
	visibility(memberCookie, true, 403)
	visibility(f.cookie, true, 200)
	detail := invitationResponse(t, f.send(t, "GET", "/admin/groups/"+groupID, nil, f.cookie, 200))
	if detail["account_usage_visible"] != true {
		t.Fatalf("group detail=%+v", detail)
	}

	got := usage(memberCookie)
	if len(got.Groups) != 1 || got.Groups[0].GroupID != groupID || got.Groups[0].GroupName != "Usage group" || len(got.Groups[0].Accounts) != 2 {
		t.Fatalf("account usage=%+v", got)
	}
	aliases := []string{got.Groups[0].Accounts[0].Alias, got.Groups[0].Accounts[1].Alias}
	if aliases[0] != "Claude-A" || aliases[1] != "" || got.Groups[0].Accounts[0].Provider != store.UpstreamProviderAnthropic ||
		got.Groups[0].Accounts[0].FiveHour == nil || *got.Groups[0].Accounts[0].FiveHour.UsedPercent != 42 {
		t.Fatalf("accounts=%+v", got.Groups[0].Accounts)
	}
	if reads.count(store.UpstreamProviderCodex, codex) != 0 {
		t.Fatal("an all-accounts Codex pool must not be read")
	}
	if owner := usage(f.cookie); len(owner.Groups) != 0 {
		t.Fatalf("non-member owner saw account usage: %+v", owner)
	}

	visibility(f.cookie, false, 200)
	if got := usage(memberCookie); len(got.Groups) != 0 {
		t.Fatalf("disabled group still exposes usage: %+v", got)
	}
}
