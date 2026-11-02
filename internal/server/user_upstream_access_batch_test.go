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

func userUpstreamAccessBatchTestRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/admin/upstream-access/users", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestUserUpstreamAccessBatchRejectsInvalidRequests(t *testing.T) {
	const user, account = `"` + userUpstreamAccessTestUser + `"`, `"0123456789abcdef"`
	valid := `{"user_ids":[` + user + `],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"test"}`
	for _, body := range []string{
		``, `{}`, `null`, `[]`,
		`{"user_ids":[` + user + `],"action":"disable","account_ids":[` + account + `],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"test","extra":"secret-canary"}`,
		`{"user_ids":[` + user + `],"action":"disable","action":"enable","account_ids":[` + account + `],"providers":[],"reason":"test"}`,
		`{"User_ids":[` + user + `],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"test"}`,
		valid + `{}`,
		`{"user_ids":null,"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"test"}`,
		`{"user_ids":[],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `,` + user + `],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"test"}`,
		`{"user_ids":["00000000000000000000000000000001"],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"selected","account_ids":[` + account + `],"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"disable","account_ids":null,"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"disable","account_ids":[],"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"enable","account_ids":[` + account + `,` + account + `],"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"enable","account_ids":["0123456789ABCDEF"],"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"enable","account_ids":[` + account + `],"providers":["codex"],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"enable","account_ids":[` + account + `],"providers":null,"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"all","account_ids":[` + account + `],"providers":["codex"],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"all","account_ids":[],"providers":[],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"all","account_ids":[],"providers":["Codex"],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"all","account_ids":[],"providers":["codex","codex"],"reason":"test"}`,
		`{"user_ids":[` + user + `],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"  "}`,
		`{"user_ids":[` + user + `],"action":"disable","account_ids":[` + account + `],"providers":[],"reason":"` + strings.Repeat("原", 501) + `"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).setUserUpstreamAccessBatch(w, userUpstreamAccessBatchTestRequest(body))
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "secret-canary") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		func(r *http.Request) { r.URL.RawQuery = "action=all" },
	} {
		r := userUpstreamAccessBatchTestRequest(valid)
		mutate(r)
		w := httptest.NewRecorder()
		(&Server{}).setUserUpstreamAccessBatch(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	r := userUpstreamAccessBatchTestRequest(strings.Repeat(" ", userUpstreamAccessRequestBytes+1))
	w := httptest.NewRecorder()
	(&Server{}).setUserUpstreamAccessBatch(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d body=%s", w.Code, w.Body.String())
	}
	read := httptest.NewRecorder()
	(&Server{}).userUpstreamAccessBatchJSON(read, httptest.NewRequest(http.MethodGet, "/admin/upstream-access/users?user=1", nil))
	if read.Code != http.StatusBadRequest {
		t.Fatalf("query read status=%d body=%s", read.Code, read.Body.String())
	}
}

func TestUserUpstreamAccessBatchRoutesProtectSessionsAndWrites(t *testing.T) {
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
			path := "/admin/upstream-access/users"
			if test.method == http.MethodGet {
				// A query string is rejected before any account sync or store read.
				path += "?user=1"
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
