package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestInvitationRoutesRequireOwnerOriginAndRecentVerification(t *testing.T) {
	endpoints := []groupHandlerEndpoint{
		{name: "create", method: http.MethodPost, path: "/admin/invitations", body: map[string]any{"kind": "member"}},
		{name: "review", method: http.MethodPost, path: "/admin/invitations/" + groupHandlerTestID + "/review", body: map[string]any{"application_ids": []string{groupHandlerMemberID}, "decision": "approve"}},
		{name: "revoke", method: http.MethodPost, path: "/admin/invitations/" + groupHandlerTestID + "/revoke"},
		{name: "list", method: http.MethodGet, path: "/admin/invitations"},
		{name: "applications", method: http.MethodGet, path: "/admin/invitations/" + groupHandlerTestID + "/applications"},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			type routeCase struct {
				name, role, origin, site, code string
				session                        bool
				age                            time.Duration
				status                         int
			}
			cases := []routeCase{
				{name: "anonymous", origin: "https://gateway.example", code: "session_required", status: 401},
				{name: "member", role: store.UserRoleMember, origin: "https://gateway.example", code: "owner_required", session: true, status: 403},
			}
			if endpoint.method == http.MethodPost {
				cases = append(cases,
					routeCase{name: "cross origin", origin: "https://foreign.example", code: "invalid_origin", status: 403},
					routeCase{name: "cross site", origin: "https://gateway.example", site: "cross-site", code: "cross_site_request", status: 403},
					routeCase{name: "stale verification", role: store.UserRoleOwner, origin: "https://gateway.example", session: true, age: 6 * time.Minute, code: "recent_identity_verification_required", status: 403},
				)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					verified := time.Now().Add(-tc.age)
					s, _ := newBillingSourceTestServer(t, tc.role, &verified)
					r := groupHandlerRequest(t, endpoint)
					r.Header.Set("Origin", tc.origin)
					r.Header.Set("Sec-Fetch-Site", tc.site)
					if tc.session {
						addBillingSourceTestSession(t, r)
					}
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, r)
					if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
						t.Fatalf("status=%d body=%s", w.Code, w.Body)
					}
					if w.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("invitation response may be cached")
					}
				})
			}
		})
	}
}

func TestInvitationPaginationAndPublicMetadata(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=101", "limit=no", "offset=-1", "offset=no"} {
		if _, _, err := invitationPagination(httptest.NewRequest("GET", "/?"+query, nil)); err == nil {
			t.Fatalf("accepted pagination %s", query)
		}
	}
	if limit, offset, err := invitationPagination(httptest.NewRequest("GET", "/", nil)); err != nil || limit != 50 || offset != 0 {
		t.Fatalf("defaults %d %d %v", limit, offset, err)
	}
	now := time.Now().UTC()
	target := "private-user"
	value := store.Invitation{ID: "invite", Kind: store.InvitationRecovery, MaxUses: 1, TokenHash: []byte("secret-hash"), TargetUserID: &target, ExpiresAt: now.Add(time.Hour)}
	raw, err := json.Marshal(publicInvitation(value, now))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"TokenHash", "token_hash", "TargetUserID", "target_user_id", target, "secret-hash"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("metadata leaked %s", secret)
		}
	}
	value.UsedAt = &now
	if result := publicInvitation(value, now); result.Status != "full" || result.UsedCount != 1 {
		t.Fatalf("legacy consumed invitation appears reusable: %+v", result)
	}
	if got := invitationLink("https://gateway.example", "token", store.InvitationGroup); got != "https://gateway.example/join#token=token&kind=group" {
		t.Fatal(got)
	}
}
