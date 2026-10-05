package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestProfileRouteProtectsSessionOriginAndUsernameVerification(t *testing.T) {
	for _, test := range []struct {
		name, origin, site, body, role, code string
		cookie                               bool
		verification                         time.Duration
		status                               int
	}{
		{name: "anonymous", origin: "https://gateway.example", body: `{"display_name":"Public"}`, code: "session_required", status: 401},
		{name: "foreign origin", origin: "https://foreign.example", body: `{}`, code: "invalid_origin", status: 403},
		{name: "cross site", origin: "https://gateway.example", site: "cross-site", body: `{}`, code: "cross_site_request", status: 403},
		{name: "member username unverified", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"username":"alice"}`, code: "recent_identity_verification_required", status: 403},
		{name: "owner username unverified", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, body: `{"username":"alice"}`, code: "recent_identity_verification_required", status: 403},
		{name: "expired verification", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, verification: -6 * time.Minute, body: `{"username":"alice"}`, code: "recent_identity_verification_required", status: 403},
		{name: "future verification", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, verification: time.Minute, body: `{"username":"alice"}`, code: "recent_identity_verification_required", status: 403},
		{name: "display does not require verification", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"display_name":" "}`, code: "invalid_profile", status: 400},
		{name: "verified username rules", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, verification: -time.Minute, body: `{"username":"0bad"}`, code: "invalid_profile", status: 400},
		{name: "empty update", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, body: `{}`, code: "invalid_profile", status: 400},
		{name: "cannot choose user", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"display_name":"Name","user_id":"another"}`, code: "invalid_json", status: 400},
		{name: "cannot change role", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"display_name":"Name","role":"owner"}`, code: "invalid_json", status: 400},
		{name: "wrong field type", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"display_name":123}`, code: "invalid_json", status: 400},
		{name: "null username", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"username":null,"display_name":"Name"}`, code: "invalid_json", status: 400},
		{name: "null display name", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"display_name":null}`, code: "invalid_json", status: 400},
		{name: "oversize", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, body: `{"display_name":"` + strings.Repeat("a", 4096) + `"}`, code: "request_too_large", status: 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			var verified *time.Time
			if test.verification != 0 {
				value := time.Now().Add(test.verification)
				verified = &value
			}
			s, conn := newBillingSourceTestServer(t, test.role, verified)
			r := billingPlanTestRequest(http.MethodPatch, "/admin/profile", test.body)
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Sec-Fetch-Site", test.site)
			if test.cookie {
				addBillingSourceTestSession(t, r)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) || len(conn.writes) != 0 {
				t.Fatalf("status=%d body=%s writes=%v", w.Code, w.Body, conn.writes)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("profile response may be cached")
			}
		})
	}
}

func TestRecoveryInvitationRejectsAmbiguousAndMalformedTargets(t *testing.T) {
	for _, body := range []string{
		`{"kind":"recovery","target_user_id":"00000000-0000-4000-8000-000000000001","target_username":"self"}`,
		`{"kind":"recovery","target_user_id":"not-a-uuid"}`,
		`{"kind":"recovery","target_user_id":"00000000-0000-4000-8000-000000000001","target_username":null}`,
		`{"kind":"recovery","target_user_id":null,"target_username":"self"}`,
		`{"kind":"recovery"}`,
		`{"kind":"member","target_user_id":"00000000-0000-4000-8000-000000000001"}`,
	} {
		t.Run(body, func(t *testing.T) {
			at := time.Now()
			s, conn := newBillingSourceTestServer(t, store.UserRoleOwner, &at)
			r := billingPlanTestRequest(http.MethodPost, "/admin/invitations", body)
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || len(conn.writes) != 0 {
				t.Fatalf("status=%d body=%s writes=%v", w.Code, w.Body, conn.writes)
			}
		})
	}
}
