//go:build integration

package server

import (
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

func TestGroupUpstreamAccessHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	_, _, memberCookie := profileHTTPMember(t, f, "group-upstream-access-member")
	created := invitationResponse(t, f.send(t, http.MethodPost, "/admin/groups", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Group upstream access HTTP regression",
		"name": "Account pool", "limit_usd": "20", "period": "day",
		"starts_at": time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339),
	}, f.cookie, http.StatusOK))
	groupID := created["id"].(string)
	path := "/admin/groups/" + groupID + "/upstream-access"
	providers := []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic}
	accountIDs := map[string]string{
		store.UpstreamProviderCodex: "0123456789abcdef", store.UpstreamProviderAntigravity: "fedcba9876543210", store.UpstreamProviderAnthropic: "aaaabbbbccccdddd",
	}
	base, _ := url.Parse("http://sidecar.internal")
	remoteFailed := false
	for _, provider := range providers {
		id := accountIDs[provider]
		client := withManagement(gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			if remoteFailed {
				return nil, errors.New("sidecar unavailable secret-canary")
			}
			body := `{"accounts":[{"id":"` + id + `","display_name":"` + provider + `-account","masked_email":"u***@example.com","plan":"plus","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available","last_synced_at":"2026-10-06T00:00:00Z"}]}`
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
	read := func() groupUpstreamAccessResponse {
		t.Helper()
		w := f.send(t, http.MethodGet, path, nil, f.cookie, http.StatusOK)
		var response groupUpstreamAccessResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.GroupID != groupID || len(response.Providers) != len(providers) || strings.Contains(w.Body.String(), "secret-canary") {
			t.Fatalf("invalid read response: %s", w.Body)
		}
		for i, provider := range providers {
			if response.Providers[i].Provider != provider || response.Providers[i].AccountIDs == nil {
				t.Fatalf("provider %d=%+v", i, response.Providers[i])
			}
		}
		return response
	}
	write := func(provider, mode string, ids []string, want int) {
		t.Helper()
		f.send(t, http.MethodPut, "/admin/groups/"+groupID+"/upstream-accounts/"+provider, map[string]any{
			"operation_id": uuid.NewString(), "reason": "Update group account pool", "mode": mode, "account_ids": ids,
		}, f.cookie, want)
	}

	initial := read()
	for i, provider := range providers {
		entry := initial.Providers[i]
		if entry.Mode != "all" || len(entry.AccountIDs) != 0 || entry.SyncWarning != "" || len(entry.Accounts) != 1 ||
			entry.Accounts[0].ID != accountIDs[provider] || entry.Accounts[0].DisplayName != provider+"-account" ||
			entry.Accounts[0].EmailMasked != "u***@example.com" || entry.Accounts[0].Status != store.UpstreamAccountStatusAvailable {
			t.Fatalf("default %s entry=%+v", provider, entry)
		}
	}
	f.send(t, http.MethodGet, path, nil, memberCookie, http.StatusForbidden)
	f.send(t, http.MethodGet, path+"?provider=codex", nil, f.cookie, http.StatusBadRequest)
	f.send(t, http.MethodGet, "/admin/groups/"+uuid.NewString()+"/upstream-access", nil, f.cookie, http.StatusNotFound)

	// Another provider's account cannot join a pool; the default stays intact.
	write(store.UpstreamProviderCodex, "selected", []string{accountIDs[store.UpstreamProviderAntigravity]}, http.StatusNotFound)
	write(store.UpstreamProviderCodex, "selected", []string{accountIDs[store.UpstreamProviderCodex]}, http.StatusOK)
	write(store.UpstreamProviderAnthropic, "selected", []string{}, http.StatusOK)
	configured := read()
	if configured.Providers[0].Mode != "selected" || !reflect.DeepEqual(configured.Providers[0].AccountIDs, []string{accountIDs[store.UpstreamProviderCodex]}) ||
		configured.Providers[1].Mode != "all" || configured.Providers[2].Mode != "selected" || len(configured.Providers[2].AccountIDs) != 0 {
		t.Fatalf("independent provider pools=%+v", configured)
	}

	// A sidecar outage still returns the durable local snapshot and settings.
	remoteFailed = true
	fallback := read()
	for _, entry := range fallback.Providers {
		if entry.SyncWarning != "upstream_account_sync_unavailable" || len(entry.Accounts) != 1 {
			t.Fatalf("missing local fallback: %+v", entry)
		}
	}
	if !reflect.DeepEqual(fallback.Providers[0].AccountIDs, configured.Providers[0].AccountIDs) || fallback.Providers[2].Mode != "selected" {
		t.Fatalf("sync failure changed pools: %+v", fallback)
	}
}
