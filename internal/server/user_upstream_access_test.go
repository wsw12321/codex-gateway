package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

const userUpstreamAccessTestUser = "00000000-0000-0000-0000-000000000001"

func userUpstreamAccessTestRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/admin/users/"+userUpstreamAccessTestUser+"/upstream-access/codex", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("user_id", userUpstreamAccessTestUser)
	r.SetPathValue("provider", store.UpstreamProviderCodex)
	return r
}

func TestUserUpstreamAccessRejectsInvalidRequests(t *testing.T) {
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `true`,
		`{"mode":"all","account_ids":[]}`, `{"mode":"all","reason":"test"}`,
		`{"mode":"all","account_ids":[],"reason":"test","extra":"secret-canary"}`,
		`{"mode":"all","mode":"selected","account_ids":[],"reason":"test"}`,
		`{"mode":"all","\u006dode":"selected","account_ids":[],"reason":"test"}`,
		`{"Mode":"all","account_ids":[],"reason":"test"}`,
		`{"mode":"all","account_ids":[],"reason":"test"}{}`,
		`{"mode":null,"account_ids":[],"reason":"test"}`,
		`{"mode":"all","account_ids":null,"reason":"test"}`,
		`{"mode":"all","account_ids":[],"reason":null}`,
		`{"mode":"selected","account_ids":"0123456789abcdef","reason":"test"}`,
		`{"mode":"unknown","account_ids":[],"reason":"test"}`,
		`{"mode":"all","account_ids":["0123456789abcdef"],"reason":"test"}`,
		`{"mode":"selected","account_ids":["0123456789abcdef","0123456789abcdef"],"reason":"test"}`,
		`{"mode":"selected","account_ids":["0123456789ABCDEF"],"reason":"test"}`,
		`{"mode":"selected","account_ids":[null],"reason":"test"}`,
		`{"mode":"selected","account_ids":[],"reason":"  "}`,
		`{"mode":"selected","account_ids":[],"reason":"` + strings.Repeat("原", 501) + `"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).setUserUpstreamAccess(w, userUpstreamAccessTestRequest(body))
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "secret-canary") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		func(r *http.Request) { r.URL.RawQuery = "provider=antigravity" },
		func(r *http.Request) { r.SetPathValue("user_id", "invalid") },
		func(r *http.Request) { r.SetPathValue("user_id", "00000000000000000000000000000001") },
		func(r *http.Request) { r.SetPathValue("provider", "unknown") },
		func(r *http.Request) { r.SetPathValue("provider", "Codex") },
	} {
		r := userUpstreamAccessTestRequest(`{"mode":"selected","account_ids":[],"reason":"test"}`)
		mutate(r)
		w := httptest.NewRecorder()
		(&Server{}).setUserUpstreamAccess(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	for _, length := range []int64{-1, userUpstreamAccessRequestBytes + 1} {
		r := userUpstreamAccessTestRequest(strings.Repeat(" ", userUpstreamAccessRequestBytes+1))
		r.ContentLength = length
		w := httptest.NewRecorder()
		(&Server{}).setUserUpstreamAccess(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversize status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestUserUpstreamAccessRoutesProtectSessionsAndWrites(t *testing.T) {
	for _, test := range []struct {
		name, method, origin, site, role, code string
		cookie, verified                       bool
		age                                    time.Duration
		want                                   int
	}{
		{name: "read needs session", method: "GET", code: "session_required", want: 401},
		{name: "member cannot read", method: "GET", cookie: true, role: store.UserRoleMember, code: "owner_required", want: 403},
		{name: "owner reads without verification", method: "GET", cookie: true, role: store.UserRoleOwner, code: "invalid_resource", want: 400},
		{name: "write needs origin", method: "PUT", code: "invalid_origin", want: 403},
		{name: "foreign origin", method: "PUT", origin: "https://evil.example", code: "invalid_origin", want: 403},
		{name: "cross site", method: "PUT", origin: "https://gateway.example", site: "cross-site", code: "cross_site_request", want: 403},
		{name: "write needs session", method: "PUT", origin: "https://gateway.example", code: "session_required", want: 401},
		{name: "member cannot write", method: "PUT", origin: "https://gateway.example", cookie: true, verified: true, role: store.UserRoleMember, code: "owner_required", want: 403},
		{name: "write needs verification", method: "PUT", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, code: "recent_identity_verification_required", want: 403},
		{name: "expired verification", method: "PUT", origin: "https://gateway.example", cookie: true, verified: true, age: 6 * time.Minute, role: store.UserRoleOwner, code: "recent_identity_verification_required", want: 403},
		{name: "verified owner reaches handler", method: "PUT", origin: "https://gateway.example", cookie: true, verified: true, role: store.UserRoleOwner, code: "invalid_json", want: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			var verified driver.Value
			if test.verified {
				verified = now.Add(-test.age)
			}
			db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(query, "FROM sessions") {
					return &upstreamAuditRows{columns: make([]string, 14), values: []driver.Value{"session-1", userUpstreamAccessTestUser, []byte("hash"), []byte("csrf"), nil, nil, now, now, now.Add(time.Hour), now.Add(time.Hour), verified, nil, "", nil}}, nil
				}
				if strings.Contains(query, "FROM users") {
					return &upstreamAuditRows{columns: make([]string, 10), values: []driver.Value{userUpstreamAccessTestUser, "owner", "Owner", []byte("webauthn-id"), test.role, store.StatusActive, now, now, nil, nil}}, nil
				}
				return nil, errors.New("unexpected authentication query")
			}}})
			defer db.Close()
			s := &Server{store: store.New(db), mux: http.NewServeMux(), config: config.Config{RPOrigins: []string{"https://gateway.example"}, ReauthMaxAge: 5 * time.Minute, TokenPepper: []byte(strings.Repeat("p", 32))}}
			s.routes()
			path := "/admin/users/" + userUpstreamAccessTestUser + "/upstream-access/codex"
			if test.method == http.MethodGet {
				path = "/admin/users/invalid/upstream-access"
			}
			r := httptest.NewRequest(test.method, path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Sec-Fetch-Site", test.site)
			if test.cookie {
				token, err := security.GenerateOpaqueToken(security.SessionToken)
				if err != nil {
					t.Fatal(err)
				}
				r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token.Token})
			}
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != test.want || !strings.Contains(w.Body.String(), test.code) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
