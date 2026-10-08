//go:build integration

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestUserUpstreamAccessHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	member, _, memberCookie := profileHTTPMember(t, f, "upstream-access-member")
	path := "/admin/users/" + member.ID + "/upstream-access"
	const codexID = "0123456789abcdef"
	const agyID = "fedcba9876543210"
	const anthropicID = "aaaabbbbccccdddd"
	const unavailableID = "aaaaaaaaaaaaaaaa"
	base, _ := url.Parse("http://sidecar.internal")
	remoteCalls := make(map[string]int)
	remoteFailed := false
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		id := codexID
		if provider == store.UpstreamProviderAntigravity {
			id = agyID
		}
		if provider == store.UpstreamProviderAnthropic {
			id = anthropicID
		}
		client := gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			remoteCalls[provider]++
			if remoteFailed {
				return nil, errors.New("sidecar unavailable secret-canary")
			}
			body := `{"accounts":[{"id":"` + id + `","display_name":"` + provider + `-account","masked_email":"u***@example.com","plan":"plus","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available","last_synced_at":"2026-10-06T00:00:00Z"}]}`
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		if provider == store.UpstreamProviderCodex {
			f.s.upstream = client
		} else if provider == store.UpstreamProviderAnthropic {
			f.s.anthropic = client
		} else {
			f.s.antigravity = client
		}
	}
	read := func() userUpstreamAccessResponse {
		t.Helper()
		w := f.send(t, http.MethodGet, path, nil, f.cookie, http.StatusOK)
		var response userUpstreamAccessResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.UserID != member.ID || len(response.Providers) != 3 || strings.Contains(w.Body.String(), "secret-canary") {
			t.Fatalf("invalid read response: %s", w.Body)
		}
		return response
	}
	write := func(provider, mode string, ids []string, want int) store.UserUpstreamAccess {
		t.Helper()
		w := f.send(t, http.MethodPut, path+"/"+provider, map[string]any{"mode": mode, "account_ids": ids, "reason": "Update upstream permission"}, f.cookie, want)
		var access store.UserUpstreamAccess
		if want == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &access); err != nil {
				t.Fatal(err)
			}
			if access.UserID != member.ID || access.Provider != provider || access.Mode != mode || !reflect.DeepEqual(access.AccountIDs, ids) {
				t.Fatalf("invalid write response: %s", w.Body)
			}
		}
		return access
	}
	initial := read()
	for i, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		entry := initial.Providers[i]
		if entry.Provider != provider || entry.Mode != "all" || entry.AccountIDs == nil || len(entry.AccountIDs) != 0 || entry.SyncWarning != "" || len(entry.Accounts) != 1 || entry.Accounts[0].EmailMasked != "u***@example.com" || entry.Accounts[0].DisplayName != provider+"-account" || entry.Accounts[0].Status != store.UpstreamAccountStatusAvailable {
			t.Fatalf("default %s entry=%+v", provider, entry)
		}
	}
	if remoteCalls[store.UpstreamProviderCodex] != 1 || remoteCalls[store.UpstreamProviderAntigravity] != 1 {
		t.Fatalf("metadata sync calls=%v", remoteCalls)
	}

	// Provider mismatch and unknown account validation must leave both defaults intact.
	write(store.UpstreamProviderCodex, "selected", []string{agyID}, http.StatusBadRequest)
	write(store.UpstreamProviderAntigravity, "selected", []string{codexID}, http.StatusBadRequest)
	write(store.UpstreamProviderCodex, "selected", []string{unavailableID}, http.StatusBadRequest)
	f.send(t, http.MethodPut, path+"/codex", map[string]any{"mode": "selected", "account_ids": []string{}, "reason": "Member forbidden"}, memberCookie, http.StatusForbidden)
	f.send(t, http.MethodGet, path, nil, memberCookie, http.StatusForbidden)

	write(store.UpstreamProviderCodex, "selected", []string{codexID}, http.StatusOK)
	write(store.UpstreamProviderAntigravity, "selected", []string{}, http.StatusOK)
	configured := read()
	if configured.Providers[0].Mode != "selected" || !reflect.DeepEqual(configured.Providers[0].AccountIDs, []string{codexID}) || configured.Providers[1].Mode != "selected" || len(configured.Providers[1].AccountIDs) != 0 {
		t.Fatalf("independent provider settings=%+v", configured)
	}
	// A local registered account remains selectable after a sidecar removes it.
	if err := f.s.store.EnsureUpstreamAccount(ctx, unavailableID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	write(store.UpstreamProviderCodex, "selected", []string{unavailableID}, http.StatusOK)
	withUnavailable := read()
	if len(withUnavailable.Providers[0].Accounts) != 2 || !reflect.DeepEqual(withUnavailable.Providers[0].AccountIDs, []string{unavailableID}) {
		t.Fatalf("offline account dropped from settings/options: %+v", withUnavailable)
	}

	remoteFailed = true
	fallback := read()
	for _, entry := range fallback.Providers {
		if entry.SyncWarning != "upstream_account_sync_unavailable" || len(entry.Accounts) == 0 {
			t.Fatalf("missing local fallback: %+v", entry)
		}
	}
	if !reflect.DeepEqual(fallback.Providers[0].AccountIDs, []string{unavailableID}) || fallback.Providers[1].Mode != "selected" {
		t.Fatalf("sync failure changed settings: %+v", fallback)
	}
	f.s.antigravity = nil
	if got := read().Providers[1]; got.SyncWarning == "" || len(got.Accounts) != 1 {
		t.Fatalf("unconfigured provider lost local snapshot: %+v", got)
	}
	write(store.UpstreamProviderCodex, "all", []string{}, http.StatusOK)
	restored := read()
	if restored.Providers[0].Mode != "all" || restored.Providers[1].Mode != "selected" {
		t.Fatalf("restore all changed other provider: %+v", restored)
	}
	// A failed metadata write also retains the previous durable snapshot.
	remoteFailed = false
	if _, err := f.s.store.DB().ExecContext(ctx, `CREATE FUNCTION reject_access_metadata_sync() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'metadata sync failed'; END $$; CREATE TRIGGER reject_access_metadata_sync BEFORE INSERT OR UPDATE ON upstream_accounts FOR EACH ROW EXECUTE FUNCTION reject_access_metadata_sync()`); err != nil {
		t.Fatal(err)
	}
	if got := read().Providers[0]; got.SyncWarning == "" || len(got.Accounts) != 2 || got.Mode != "all" {
		t.Fatalf("metadata write failure lost snapshot: %+v", got)
	}
	if _, err := f.s.store.DB().ExecContext(ctx, `DROP TRIGGER reject_access_metadata_sync ON upstream_accounts; DROP FUNCTION reject_access_metadata_sync()`); err != nil {
		t.Fatal(err)
	}
	// Nonexistent and pending targets cannot acquire rules.
	f.send(t, http.MethodGet, "/admin/users/"+uuid.NewString()+"/upstream-access", nil, f.cookie, http.StatusNotFound)
	if _, err := f.s.store.DB().ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	f.send(t, http.MethodGet, path, nil, f.cookie, http.StatusConflict)
	write(store.UpstreamProviderCodex, "all", []string{}, http.StatusConflict)
}
