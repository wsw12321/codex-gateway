//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

// Exercise the unchanged callback protocol used by both sidecars. In
// particular, an existing binding must see the new policy on its next
// eligibility check, including when that binding has zero allocation weight.
func TestUserUpstreamRoutingPostgresIntegration(t *testing.T) {
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity} {
		t.Run(provider, func(t *testing.T) {
			f := newInvitationHTTPFixture(t)
			ctx := context.Background()
			f.s.config.SidecarToken = "codex-routing-test"
			f.s.config.AntigravityBridgeToken = "antigravity-routing-test"
			repository := f.s.store.WithUpstreamProvider(provider)
			prefix, token := "/internal/upstream-accounts/", f.s.config.SidecarToken
			if provider == store.UpstreamProviderAntigravity {
				prefix, token = "/internal/antigravity-accounts/", f.s.config.AntigravityBridgeToken
			}
			const first, second, unknown = "1122334455667788", "2233445566778899", "3344556677889900"
			if err := repository.SyncUpstreamAccounts(ctx, []store.UpstreamAccountSnapshot{
				{ID: first, DisplayName: "primary", MaskedEmail: "a***@example.test", Status: "available"},
				{ID: second, DisplayName: "secondary", MaskedEmail: "b***@example.test", Status: "available"},
			}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			setAccess := func(mode string, ids ...string) {
				t.Helper()
				_, err := repository.SetUserUpstreamAccess(ctx, store.SetUserUpstreamAccessParams{
					UserID: f.owner.ID, Mode: mode, AccountIDs: ids,
					ActorUserID: f.owner.ID, Reason: "routing regression",
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			request := func(action string, want int, ids ...string) *httptest.ResponseRecorder {
				t.Helper()
				body, err := json.Marshal(map[string]any{"user_id": f.owner.ID, "account_ids": ids})
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodPost, prefix+action, strings.NewReader(string(body)))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				f.s.Handler().ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s %s: status %d, want %d: %s", provider, action, w.Code, want, w.Body)
				}
				return w
			}
			eligible := func(want []string, ids ...string) {
				t.Helper()
				var response struct {
					Accounts []struct {
						ID string `json:"id"`
					} `json:"accounts"`
				}
				if err := json.Unmarshal(request("eligible", 200, ids...).Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				got := make([]string, 0, len(response.Accounts))
				for _, account := range response.Accounts {
					got = append(got, account.ID)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("eligible %v, want %v", got, want)
				}
			}
			// Missing rules allow existing and newly discovered accounts.
			eligible([]string{first, second, unknown}, first, second, unknown)
			setAccess("selected", first)
			eligible([]string{first}, first, second, unknown)
			if body := request("select", 200, first, second, unknown).Body.String(); !strings.Contains(body, first) {
				t.Fatalf("new assignment escaped selected list: %s", body)
			}
			if _, err := repository.SetUpstreamAccountAllocationWeight(ctx, store.SetUpstreamAccountAllocationWeightParams{
				AccountID: first, Weight: 0, ActorUserID: f.owner.ID,
			}); err != nil {
				t.Fatal(err)
			}
			eligible([]string{first}, first) // existing zero-weight binding remains authorized
			request("select", 429, first)    // no new assignment on a draining account
			setAccess("selected", second)
			eligible([]string{second}, first, second, unknown)
			if body := request("select", 200, first, second).Body.String(); !strings.Contains(body, second) {
				t.Fatalf("revoked binding was not replaced: %s", body)
			}
			setAccess("selected")
			eligible([]string{}, first, second, unknown)
			request("select", 429, first, second, unknown)
			setAccess("all")
			eligible([]string{first, second, unknown}, first, second, unknown)
			// Even with an established binding, database errors cannot fall back
			// to a cached authorization result or a locally preferred account.
			if err := f.s.store.Close(); err != nil {
				t.Fatal(err)
			}
			request("eligible", 503, first, second)
			request("select", 503, first, second)
		})
	}
}
