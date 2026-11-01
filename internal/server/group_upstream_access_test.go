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

func TestGroupUpstreamAccessRouteRequiresOwnerAndValidTarget(t *testing.T) {
	const groupID = "00000000-0000-0000-0000-000000000002"
	for _, test := range []struct {
		name, path, role, code string
		cookie                 bool
		want                   int
	}{
		{name: "read needs session", path: "/admin/groups/" + groupID + "/upstream-access", code: "session_required", want: 401},
		{name: "member cannot read", path: "/admin/groups/" + groupID + "/upstream-access", cookie: true, role: store.UserRoleMember, code: "owner_required", want: 403},
		{name: "invalid group", path: "/admin/groups/invalid/upstream-access", cookie: true, role: store.UserRoleOwner, code: "group_resource_not_found", want: 404},
		{name: "non-canonical group", path: "/admin/groups/00000000000000000000000000000002/upstream-access", cookie: true, role: store.UserRoleOwner, code: "group_resource_not_found", want: 404},
		{name: "query rejected", path: "/admin/groups/" + groupID + "/upstream-access?provider=codex", cookie: true, role: store.UserRoleOwner, code: "invalid_group_operation", want: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(query, "FROM sessions") {
					return &upstreamAuditRows{columns: make([]string, 14), values: []driver.Value{"session-1", userUpstreamAccessTestUser, []byte("hash"), []byte("csrf"), nil, nil, now, now, now.Add(time.Hour), now.Add(time.Hour), nil, nil, "", nil}}, nil
				}
				if strings.Contains(query, "FROM users") {
					return &upstreamAuditRows{columns: make([]string, 10), values: []driver.Value{userUpstreamAccessTestUser, "owner", "Owner", []byte("webauthn-id"), test.role, store.StatusActive, now, now, nil, nil}}, nil
				}
				return nil, errors.New("unexpected query before group upstream access validation")
			}}})
			defer db.Close()
			s := &Server{store: store.New(db), mux: http.NewServeMux(), config: config.Config{RPOrigins: []string{"https://gateway.example"}, ReauthMaxAge: 5 * time.Minute, TokenPepper: []byte(strings.Repeat("p", 32))}}
			s.routes()
			r := httptest.NewRequest(http.MethodGet, test.path, nil)
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
