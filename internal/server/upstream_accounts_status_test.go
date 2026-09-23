package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func statusTestRequest(body string) *http.Request {
	r := quotaTestRequestWithBody("0123456789abcdef", body)
	r.Method = http.MethodPut
	r.URL.Path = "/admin/upstream-accounts/0123456789abcdef/status"
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestUpstreamStatusRejectsInvalidRequests(t *testing.T) {
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `true`, `{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":1}`,
		`{"Enabled":true}`, `{"enabled":false,"enabled":true}`, `{"enabled":false,"\u0065nabled":true}`,
		`{"enabled":true,"token":"sensitive-canary"}`, `{"enabled":true}{}`, `{"enabled":true`,
	} {
		t.Run(body, func(t *testing.T) {
			response := httptest.NewRecorder()
			(&Server{}).setUpstreamAccountStatus(response, statusTestRequest(body))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_json") || strings.Contains(response.Body.String(), "sensitive-canary") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	for _, length := range []int64{-1, upstreamStatusRequestBytes + 1} {
		r := statusTestRequest(`{"enabled":true}` + strings.Repeat(" ", upstreamStatusRequestBytes))
		r.ContentLength = length
		response := httptest.NewRecorder()
		(&Server{}).setUpstreamAccountStatus(response, r)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversize status=%d", response.Code)
		}
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		func(r *http.Request) { r.URL.RawQuery = "enabled=false" },
	} {
		r := statusTestRequest(`{"enabled":true}`)
		mutate(r)
		response := httptest.NewRecorder()
		(&Server{}).setUpstreamAccountStatus(response, r)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid protocol status=%d", response.Code)
		}
	}
	for _, id := range []string{"", "../secret", "0123456789ABCDEF", "0123456789abcdef0"} {
		r := statusTestRequest(`{"enabled":true}`)
		r.SetPathValue("id", id)
		response := httptest.NewRecorder()
		(&Server{}).setUpstreamAccountStatus(response, r)
		if response.Code != http.StatusNotFound {
			t.Fatalf("invalid id status=%d", response.Code)
		}
	}
}

func TestUpstreamStatusUsesConfirmationAndAuditsOutcome(t *testing.T) {
	for _, test := range []struct {
		name, body, remoteBody, code, event string
		remoteStatus, wantStatus            int
	}{
		{"disable", `{"enabled":false}`, `{"id":"0123456789abcdef","status":"unavailable","cliproxy_status":"active","gateway_manual_status":"manual_disabled","gateway_quota_status":"available"}`, "ok", "upstream_account.disabled", 200, 200},
		{"enable", `{"enabled":true}`, `{"id":"0123456789abcdef","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available"}`, "ok", "upstream_account.enabled", 200, 200},
		{"persistence failure", `{"enabled":true}`, `{"error":"account_status_persistence_failed"}`, "upstream_account_status_persistence_failed", "upstream_account.enabled", 503, 503},
		{"missing account", `{"enabled":false}`, `{"error":"upstream_account_not_found"}`, "invalid_upstream_account", "upstream_account.disabled", 404, 404},
		{"identity invalid", `{"enabled":true}`, `{"error":"account_status_identity_invalid"}`, "upstream_account_identity_invalid", "upstream_account.enabled", 409, 409},
		{"protocol invalid", `{"enabled":true}`, `{"error":"account_status_request_invalid"}`, "sidecar_account_status_protocol_error", "upstream_account.enabled", 400, 502},
		{"protocol too large", `{"enabled":true}`, `{"error":"account_status_request_too_large"}`, "sidecar_account_status_protocol_error", "upstream_account.enabled", 413, 502},
		{"unrecognized secret", `{"enabled":true}`, `{"error":"Bearer sensitive-canary"}`, "sidecar_request_failed", "upstream_account.enabled", 503, 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, capture := newUpstreamAuditStore(t)
			base, _ := url.Parse("http://sidecar.internal")
			server := &Server{store: repository, upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPut {
					t.Errorf("unexpected refresh during control request: %s", r.Method)
				}
				return &http.Response{StatusCode: test.remoteStatus, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(test.remoteBody))}, nil
			})})}
			response := httptest.NewRecorder()
			server.setUpstreamAccountStatus(response, statusTestRequest(test.body))
			if response.Code != test.wantStatus || strings.Contains(response.Body.String(), "sensitive-canary") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.wantStatus == http.StatusOK {
				if strings.TrimSpace(response.Body.String()) != test.remoteBody {
					t.Fatalf("confirmation=%s", response.Body.String())
				}
			} else if !strings.Contains(response.Body.String(), test.code) {
				t.Fatalf("missing fixed error code: %s", response.Body.String())
			}
			args := capture.snapshot()
			if len(args) != 12 || args[4].Value != test.event {
				t.Fatalf("audit=%+v", args)
			}
			args[4].Value = "upstream_account.quota_queried"
			assertQuotaAudit(t, args, test.wantStatus == http.StatusOK, test.code, test.wantStatus)
		})
	}
}

func TestUpstreamStatusSuccessSurvivesAuditDatabaseFailure(t *testing.T) {
	db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		return nil, errors.New("database unavailable sensitive-canary")
	}}})
	defer db.Close()
	base, _ := url.Parse("http://sidecar.internal")
	server := &Server{store: store.New(db), upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"0123456789abcdef","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available"}`))}, nil
	})})}
	response := httptest.NewRecorder()
	server.setUpstreamAccountStatus(response, statusTestRequest(`{"enabled":true}`))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"status":"available"`) {
		t.Fatalf("confirmation lost after audit failure: %d %s", response.Code, response.Body.String())
	}
}

func TestUpstreamStatusRejectsLegacyConfirmationWithoutSourceStates(t *testing.T) {
	repository, _ := newUpstreamAuditStore(t)
	base, _ := url.Parse("http://sidecar.internal")
	server := &Server{store: repository, upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"0123456789abcdef","status":"available"}`))}, nil
	})})}
	response := httptest.NewRecorder()
	server.setUpstreamAccountStatus(response, statusTestRequest(`{"enabled":true}`))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "sidecar_account_status_protocol_error") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUpstreamStatusRouteProtectsOriginRoleAndRecentVerification(t *testing.T) {
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
		{name: "member", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, verified: true, code: "owner_required", wantStatus: 403},
		{name: "unverified", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, code: "recent_identity_verification_required", wantStatus: 403},
		{name: "expired verification", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, verified: true, age: 6 * time.Minute, code: "recent_identity_verification_required", wantStatus: 403},
		{name: "verified owner reaches handler", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, verified: true, code: "invalid_json", wantStatus: 400},
	} {
		for _, control := range []string{"status", "allocation-weight", "access"} {
			t.Run(test.name+"/"+control, func(t *testing.T) {
				now := time.Now().UTC()
				var verified driver.Value
				if test.verified {
					verified = now.Add(-test.age)
				}
				db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
					if strings.Contains(query, "FROM sessions") {
						return &upstreamAuditRows{columns: make([]string, 13), values: []driver.Value{"session-1", "owner-1", []byte("hash"), []byte("csrf"), nil, nil, now, now, now.Add(time.Hour), now.Add(time.Hour), verified, nil, ""}}, nil
					}
					if strings.Contains(query, "FROM users") {
						return &upstreamAuditRows{columns: make([]string, 10), values: []driver.Value{"owner-1", "owner", "Owner", []byte("webauthn-id"), test.role, store.StatusActive, now, now, nil, nil}}, nil
					}
					return nil, errors.New("unexpected authentication query")
				}}})
				defer db.Close()
				server := &Server{store: store.New(db), mux: http.NewServeMux(), config: config.Config{RPOrigins: []string{"https://gateway.example"}, ReauthMaxAge: 5 * time.Minute, TokenPepper: []byte(strings.Repeat("p", 32))}}
				server.routes()
				r := statusTestRequest(`{}`)
				r.URL.Path = "/admin/upstream-accounts/0123456789abcdef/" + control
				r.Header.Set("Origin", test.origin)
				r.Header.Set("Sec-Fetch-Site", test.site)
				if test.cookie {
					token, err := security.GenerateOpaqueToken(security.SessionToken)
					if err != nil {
						t.Fatal(err)
					}
					r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token.Token})
				}
				response := httptest.NewRecorder()
				server.mux.ServeHTTP(response, r)
				if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.code) || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestUpstreamAccountsManageabilityAndStatusSerialization(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "historical only", true: "live account"}[live], func(t *testing.T) {
			summaryStarted, releaseSummary := make(chan struct{}), make(chan struct{})
			capture := &upstreamAuditCapture{}
			db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(query, "a.access_mode") || strings.Contains(query, "WITH account_costs AS") {
					return (upstreamSummaryConn{}).QueryContext(ctx, query, args)
				}
				if strings.Contains(query, "WITH usage_source AS") {
					close(summaryStarted)
					<-releaseSummary
					return (upstreamSummaryConn{}).QueryContext(ctx, query, args)
				}
				return (&upstreamAuditConn{capture: capture}).QueryContext(ctx, query, args)
			}}})
			defer db.Close()
			var controls atomic.Int32
			base, _ := url.Parse("http://sidecar.internal")
			server := &Server{store: store.New(db), upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"accounts":[]}`
				if r.Method == http.MethodPut {
					controls.Add(1)
					body = `{"id":"0123456789abcdef","status":"unavailable","cliproxy_status":"active","gateway_manual_status":"manual_disabled","gateway_quota_status":"available"}`
				} else if live {
					body = `{"accounts":[{"id":"0123456789abcdef","masked_email":"u***@example.com","plan":"plus","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available","last_synced_at":"2026-09-01T00:00:00Z"}]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})})}
			listResponse, statusResponse := httptest.NewRecorder(), httptest.NewRecorder()
			listDone, statusDone := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(listDone)
				server.upstreamAccountsJSON(listResponse, httptest.NewRequest(http.MethodGet, "/admin/upstream-accounts", nil))
			}()
			<-summaryStarted
			go func() {
				defer close(statusDone)
				server.setUpstreamAccountStatus(statusResponse, statusTestRequest(`{"enabled":false}`))
			}()
			select {
			case <-statusDone:
				t.Error("status completed before prior list summary")
			case <-time.After(25 * time.Millisecond):
			}
			if controls.Load() != 0 {
				t.Error("control reached sidecar before prior list summary completed")
			}
			close(releaseSummary)
			<-listDone
			<-statusDone
			var list upstreamAccountsResponse
			if json.Unmarshal(listResponse.Body.Bytes(), &list) != nil || len(list.Accounts) != 1 || list.Accounts[0].CanManage != live ||
				(live && (list.Accounts[0].Status != "available" || list.Accounts[0].CliproxyStatus != "active" ||
					list.Accounts[0].GatewayManualStatus != "enabled" || list.Accounts[0].GatewayQuotaStatus != "available")) ||
				(!live && (list.Accounts[0].Status != "unknown" || list.Accounts[0].CliproxyStatus != "unknown")) ||
				statusResponse.Code != 200 || controls.Load() != 1 {
				t.Fatalf("list=%s status=%s controls=%d", listResponse.Body.String(), statusResponse.Body.String(), controls.Load())
			}
		})
	}
}

func TestUpstreamStatusSerializesConcurrentOperations(t *testing.T) {
	repository, _ := newUpstreamAuditStore(t)
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	base, _ := url.Parse("http://sidecar.internal")
	server := &Server{store: repository, upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: quotaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		body, _ := io.ReadAll(r.Body)
		status := "unavailable"
		manualStatus := "manual_disabled"
		if string(body) == `{"enabled":true}` {
			status = "available"
			manualStatus = "enabled"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"0123456789abcdef","status":"` + status + `","cliproxy_status":"active","gateway_manual_status":"` + manualStatus + `","gateway_quota_status":"available"}`))}, nil
	})})}
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	firstResponse, secondResponse := httptest.NewRecorder(), httptest.NewRecorder()
	go func() {
		defer close(firstDone)
		server.setUpstreamAccountStatus(firstResponse, statusTestRequest(`{"enabled":false}`))
	}()
	<-firstStarted
	go func() {
		defer close(secondDone)
		server.setUpstreamAccountStatus(secondResponse, statusTestRequest(`{"enabled":true}`))
	}()
	select {
	case <-secondDone:
		t.Error("second control completed while first was still in flight")
	case <-time.After(25 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Error("concurrent account controls reached sidecar")
	}
	close(releaseFirst)
	<-firstDone
	<-secondDone
	if firstResponse.Code != 200 || secondResponse.Code != 200 || calls.Load() != 2 {
		t.Fatalf("first=%s second=%s calls=%d", firstResponse.Body.String(), secondResponse.Body.String(), calls.Load())
	}
}

type statusTestConnector struct{ conn *statusTestConn }

func (c statusTestConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (statusTestConnector) Driver() driver.Driver                          { return upstreamAuditDriver{} }

type statusTestConn struct {
	query func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
}

func (*statusTestConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (*statusTestConn) Close() error                        { return nil }
func (*statusTestConn) Begin() (driver.Tx, error)           { return statusTestTx{}, nil }
func (*statusTestConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (c *statusTestConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.query(ctx, query, args)
}

type statusTestTx struct{}

func (statusTestTx) Commit() error   { return nil }
func (statusTestTx) Rollback() error { return nil }
