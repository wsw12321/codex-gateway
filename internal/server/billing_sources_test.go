package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func billingSourceTestRequest(source, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/admin/billing/me/sources/"+source+"/status", strings.NewReader(body))
	r.SetPathValue("source", source)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://gateway.example")
	return r
}

func TestBillingSourceStatusStrictInput(t *testing.T) {
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `true`, `{"disabled":null}`, `{"disabled":"true"}`, `{"disabled":1}`,
		`{"Disabled":true}`, `{"disabled":false,"disabled":true}`, `{"disabled":false,"\u0064isabled":true}`,
		`{"disabled":true,"user_id":"another-user"}`, `{"disabled":true,"reason":"unneeded"}`,
		`{"disabled":true}{}`, `{"disabled":true`,
	} {
		t.Run(body, func(t *testing.T) {
			response := httptest.NewRecorder()
			(&Server{}).setBillingSourceStatus(response, billingSourceTestRequest("cash", body))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_json") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	for _, source := range []string{"", "Day", "balance", "all", "cash;DROP TABLE users"} {
		r := billingSourceTestRequest("cash", `{"disabled":true}`)
		r.SetPathValue("source", source)
		response := httptest.NewRecorder()
		(&Server{}).setBillingSourceStatus(response, r)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid source %q status=%d", source, response.Code)
		}
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		func(r *http.Request) { r.URL.RawQuery = "user_id=another-user" },
	} {
		r := billingSourceTestRequest("cash", `{"disabled":true}`)
		mutate(r)
		response := httptest.NewRecorder()
		(&Server{}).setBillingSourceStatus(response, r)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid protocol status=%d", response.Code)
		}
	}
	for _, length := range []int64{-1, billingSourceStatusRequestBytes + 1} {
		r := billingSourceTestRequest("cash", `{"disabled":true}`+strings.Repeat(" ", billingSourceStatusRequestBytes))
		r.ContentLength = length
		response := httptest.NewRecorder()
		(&Server{}).setBillingSourceStatus(response, r)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized request status=%d", response.Code)
		}
	}
}

func TestBillingSourceStatusRouteRequiresOriginSessionAndRecentVerification(t *testing.T) {
	for _, test := range []struct {
		name, origin, site, role, code string
		cookie, verified               bool
		age                            time.Duration
		wantStatus                     int
	}{
		{name: "no origin", code: "invalid_origin", wantStatus: 403},
		{name: "foreign origin", origin: "https://evil.example", code: "invalid_origin", wantStatus: 403},
		{name: "cross site", origin: "https://gateway.example", site: "cross-site", code: "cross_site_request", wantStatus: 403},
		{name: "no session", origin: "https://gateway.example", code: "session_required", wantStatus: 401},
		{name: "unverified", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, code: "recent_identity_verification_required", wantStatus: 403},
		{name: "expired verification", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, verified: true, age: 6 * time.Minute, code: "recent_identity_verification_required", wantStatus: 403},
		{name: "verified member", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, verified: true, code: "invalid_json", wantStatus: 400},
		{name: "verified owner", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, verified: true, code: "invalid_json", wantStatus: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			var verified *time.Time
			if test.verified {
				at := time.Now().Add(-test.age)
				verified = &at
			}
			s, conn := newBillingSourceTestServer(t, test.role, verified)
			r := billingSourceTestRequest("cash", `{}`)
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Sec-Fetch-Site", test.site)
			if test.cookie {
				addBillingSourceTestSession(t, r)
			}
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, r)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.code) || len(conn.writes) != 0 {
				t.Fatalf("status=%d body=%s writes=%v", response.Code, response.Body.String(), conn.writes)
			}
		})
	}
}

func TestBillingSourceStatusOnlyUpdatesAuthenticatedUser(t *testing.T) {
	for _, role := range []string{store.UserRoleMember, store.UserRoleOwner} {
		for _, source := range []string{"day", "week", "month", "cash"} {
			for _, disabled := range []bool{true, false} {
				t.Run(role+"/"+source+"/"+map[bool]string{true: "disable", false: "restore"}[disabled], func(t *testing.T) {
					now := time.Now()
					s, conn := newBillingSourceTestServer(t, role, &now)
					body, _ := json.Marshal(map[string]bool{"disabled": disabled})
					// Repeated explicit assignments are accepted without operation IDs.
					for range 2 {
						r := billingSourceTestRequest(source, string(body))
						addBillingSourceTestSession(t, r)
						response := httptest.NewRecorder()
						s.mux.ServeHTTP(response, r)
						var result struct {
							Source   string `json:"source"`
							Disabled bool   `json:"disabled"`
						}
						if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Source != source || result.Disabled != disabled {
							t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
						}
						if response.Header().Get("Cache-Control") != "no-store" {
							t.Fatal("response must not be cached")
						}
					}
					if len(conn.writes) != 4 {
						t.Fatalf("writes=%v", conn.writes)
					}
					for _, write := range conn.writes {
						if strings.Contains(write.query, "UPDATE billing_accounts") {
							if write.args[0].Value != "self-1" || write.args[1].Value != disabled || !strings.Contains(write.query, source+"_source_disabled") {
								t.Fatalf("incorrect update: %#v", write)
							}
						} else if strings.Contains(write.query, "INSERT INTO audit_events") {
							if write.args[1].Value != "self-1" || write.args[2].Value != "session-1" || write.args[6].Value != "self-1" {
								t.Fatalf("incorrect audit actor/subject: %#v", write)
							}
						} else {
							t.Fatalf("unexpected monetary write: %#v", write)
						}
					}
				})
			}
		}
	}
}

func TestBillingSourceStatusCannotTargetAnotherUser(t *testing.T) {
	now := time.Now()
	s, conn := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
	for _, path := range []string{
		"/admin/billing/users/other-user/sources/cash/status",
		"/admin/billing/me/sources/cash/status?user_id=other-user",
	} {
		r := billingSourceTestRequest("cash", `{"disabled":true}`)
		urlRequest := httptest.NewRequest(http.MethodPut, path, nil)
		r.URL = urlRequest.URL
		addBillingSourceTestSession(t, r)
		response := httptest.NewRecorder()
		s.mux.ServeHTTP(response, r)
		if response.Code < 400 || len(conn.writes) != 0 {
			t.Fatalf("target override accepted: status=%d writes=%v", response.Code, conn.writes)
		}
	}
}

func TestBillingSourceStatusDoesNotAcknowledgeFailedSave(t *testing.T) {
	now := time.Now()
	s, conn := newBillingSourceTestServer(t, store.UserRoleMember, &now)
	conn.writeError = errors.New("storage unavailable")
	r := billingSourceTestRequest("cash", `{"disabled":true}`)
	addBillingSourceTestSession(t, r)
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, r)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"internal_error"`) || strings.Contains(response.Body.String(), "storage unavailable") {
		t.Fatalf("failed save response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBillingStateResponseIncludesAllSourcePreferences(t *testing.T) {
	for _, disabled := range []store.BillingSourceDisabled{{}, {Day: true, Week: true, Month: true, Cash: true}} {
		encoded, err := json.Marshal(billingStateResponse(store.BillingState{SourceDisabled: disabled}, 10, 0))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			SourceDisabled map[string]bool `json:"source_disabled"`
		}
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"day": disabled.Day, "week": disabled.Week, "month": disabled.Month, "cash": disabled.Cash}
		if !reflect.DeepEqual(result.SourceDisabled, want) {
			t.Fatalf("source_disabled=%v, want %v", result.SourceDisabled, want)
		}
	}
}

func addBillingSourceTestSession(t *testing.T, r *http.Request) {
	t.Helper()
	token, err := security.GenerateOpaqueToken(security.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token.Token})
}

type billingSourceTestWrite struct {
	query string
	args  []driver.NamedValue
}

type billingSourceTestConn struct {
	*statusTestConn
	writes     []billingSourceTestWrite
	writeError error
}

func (c *billingSourceTestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.writes = append(c.writes, billingSourceTestWrite{query, append([]driver.NamedValue(nil), args...)})
	if c.writeError != nil {
		return nil, c.writeError
	}
	return driver.RowsAffected(1), nil
}

type billingSourceTestConnector struct{ conn *billingSourceTestConn }

func (c billingSourceTestConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (billingSourceTestConnector) Driver() driver.Driver                          { return upstreamAuditDriver{} }

func newBillingSourceTestServer(t *testing.T, role string, verified *time.Time) (*Server, *billingSourceTestConn) {
	t.Helper()
	now := time.Now().UTC()
	var verification driver.Value
	if verified != nil {
		verification = *verified
	}
	conn := &billingSourceTestConn{statusTestConn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
		switch {
		case strings.Contains(query, "FROM sessions"):
			return &upstreamAuditRows{columns: make([]string, 13), values: []driver.Value{"session-1", "self-1", []byte("hash"), []byte("csrf"), nil, nil, now, now, now.Add(time.Hour), now.Add(time.Hour), verification, nil, ""}}, nil
		case strings.Contains(query, "FROM users"):
			return &upstreamAuditRows{columns: make([]string, 10), values: []driver.Value{"self-1", "self", "Self", []byte("webauthn-id"), role, store.StatusActive, now, now, nil, nil}}, nil
		case strings.Contains(query, "FROM billing_accounts"):
			if len(args) != 1 || args[0].Value != "self-1" {
				return nil, errors.New("only session user may be targeted")
			}
			return &upstreamAuditRows{columns: []string{"disabled"}, values: []driver.Value{false}}, nil
		default:
			return nil, errors.New("unexpected billing preference query")
		}
	}}}
	db := sql.OpenDB(billingSourceTestConnector{conn: conn})
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{
		store: store.New(db), mux: http.NewServeMux(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: config.Config{RPOrigins: []string{"https://gateway.example"}, ReauthMaxAge: 5 * time.Minute, TokenPepper: []byte(strings.Repeat("p", 32))},
	}
	s.routes()
	return s, conn
}
