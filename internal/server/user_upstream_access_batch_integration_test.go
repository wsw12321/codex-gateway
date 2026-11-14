//go:build integration

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestUserUpstreamAccessBatchHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	member, _, memberCookie := profileHTTPMember(t, f, "batch-access-member")
	f.s.config.SidecarToken = "batch-routing-test"
	const path = "/admin/upstream-access/users"
	const codexFirst, codexSecond, agyID, anthropicID = "1122334455667788", "2233445566778899", "fedcba9876543210", "aaaabbbbccccdddd"
	base, _ := url.Parse("http://sidecar.internal")
	remoteFailed := false
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		accounts := map[string][]string{
			store.UpstreamProviderCodex:       {codexFirst, codexSecond},
			store.UpstreamProviderAntigravity: {agyID},
			store.UpstreamProviderAnthropic:   {anthropicID},
		}[provider]
		client := withManagement(gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			if remoteFailed {
				return nil, errors.New("sidecar unavailable secret-canary")
			}
			entries := make([]string, 0, len(accounts))
			for _, id := range accounts {
				entries = append(entries, `{"id":"`+id+`","display_name":"`+provider+`-`+id[:4]+`","masked_email":"u***@example.com","plan":"plus","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available","last_synced_at":"2026-11-02T00:00:00Z"}`)
			}
			body := `{"accounts":[` + strings.Join(entries, ",") + `]}`
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}))
		switch provider {
		case store.UpstreamProviderCodex:
			f.s.upstream = client
		case store.UpstreamProviderAntigravity:
			f.s.antigravity = client
		default:
			f.s.anthropic = client
		}
	}
	read := func() userUpstreamAccessBatchResponse {
		t.Helper()
		w := f.send(t, http.MethodGet, path, nil, f.cookie, http.StatusOK)
		var response userUpstreamAccessBatchResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Providers) != 3 || response.Rules == nil || strings.Contains(w.Body.String(), "secret-canary") {
			t.Fatalf("invalid batch read: %s", w.Body)
		}
		return response
	}
	write := func(body map[string]any, want int) store.UserUpstreamAccessBatchResult {
		t.Helper()
		body["reason"] = "Batch upstream permission"
		w := f.send(t, http.MethodPut, path, body, f.cookie, want)
		var result store.UserUpstreamAccessBatchResult
		if want == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	eligible := func(userID string, want ...string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"user_id": userID, "account_ids": []string{codexFirst, codexSecond}})
		r := httptest.NewRequest(http.MethodPost, "/internal/upstream-accounts/eligible", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+f.s.config.SidecarToken)
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, r)
		var response struct {
			Accounts []struct {
				ID string `json:"id"`
			} `json:"accounts"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("eligible status=%d body=%s", w.Code, w.Body)
		}
		got := []string{}
		for _, account := range response.Accounts {
			got = append(got, account.ID)
		}
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("eligible = %v; want %v", got, want)
		}
	}

	initial := read()
	for i, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		entry := initial.Providers[i]
		if entry.Provider != provider || entry.SyncWarning != "" || len(entry.Accounts) == 0 || entry.Accounts[0].EmailMasked != "u***@example.com" {
			t.Fatalf("%s options = %+v", provider, entry)
		}
	}
	if len(initial.Rules) != 0 || len(initial.Providers[0].Accounts) != 2 {
		t.Fatalf("initial batch read = %+v", initial)
	}
	users := []string{f.owner.ID, member.ID}
	eligible(f.owner.ID, codexFirst, codexSecond)

	// A cross-provider batch disables both accounts for both users in one write.
	result := write(map[string]any{"user_ids": users, "action": "disable", "account_ids": []string{codexFirst, agyID}, "providers": []string{}}, http.StatusOK)
	if result.TargetCount != 4 || result.ChangedCount != 4 || result.ConvertedCount != 4 || len(result.Access) != 4 {
		t.Fatalf("disable result = %+v", result)
	}
	eligible(f.owner.ID, codexSecond)
	rules := read().Rules
	if len(rules) != 4 {
		t.Fatalf("rules after disable = %+v", rules)
	}
	for _, rule := range rules {
		want := []string{codexSecond}
		if rule.Provider == store.UpstreamProviderAntigravity {
			want = []string{}
		}
		if rule.Mode != "selected" || !reflect.DeepEqual(rule.AccountIDs, want) {
			t.Fatalf("rule after disable = %+v", rule)
		}
	}
	result = write(map[string]any{"user_ids": users, "action": "enable", "account_ids": []string{codexFirst}, "providers": []string{}}, http.StatusOK)
	if result.ChangedCount != 2 || result.ConvertedCount != 0 {
		t.Fatalf("enable result = %+v", result)
	}
	eligible(f.owner.ID, codexFirst, codexSecond)

	// Rejected batches change nothing.
	write(map[string]any{"user_ids": users, "action": "enable", "account_ids": []string{"ffffffffffffffff"}, "providers": []string{}}, http.StatusBadRequest)
	f.send(t, http.MethodPut, path, map[string]any{"user_ids": users, "action": "all", "account_ids": []string{}, "providers": []string{"codex"}, "reason": "Member forbidden"}, memberCookie, http.StatusForbidden)
	f.send(t, http.MethodGet, path, nil, memberCookie, http.StatusForbidden)
	if _, err := f.s.store.DB().ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	write(map[string]any{"user_ids": users, "action": "all", "account_ids": []string{}, "providers": []string{"codex"}}, http.StatusConflict)
	pendingRead := read()
	for _, rule := range pendingRead.Rules {
		if rule.UserID == member.ID {
			t.Fatalf("pending user rule listed: %+v", rule)
		}
	}
	if len(pendingRead.Rules) != 2 {
		t.Fatalf("owner rules = %+v", pendingRead.Rules)
	}

	// A sync failure falls back to the local snapshot and still allows writes.
	remoteFailed = true
	fallback := read()
	for _, entry := range fallback.Providers {
		if entry.SyncWarning != "upstream_account_sync_unavailable" || len(entry.Accounts) == 0 {
			t.Fatalf("missing local fallback: %+v", entry)
		}
	}
	result = write(map[string]any{"user_ids": []string{f.owner.ID}, "action": "all", "account_ids": []string{}, "providers": []string{"codex", "antigravity"}}, http.StatusOK)
	if result.ChangedCount != 2 {
		t.Fatalf("all result = %+v", result)
	}
	if rules := read().Rules; len(rules) != 2 || rules[0].Mode != "all" || rules[1].Mode != "all" {
		t.Fatalf("restored rules = %+v", rules)
	}
}
