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
	"sync"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestUpstreamAccountOwnerOnlyProtection(t *testing.T) {
	server := &Server{}
	for _, endpoint := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "account list", method: http.MethodGet, path: "/admin/upstream-accounts"},
		{name: "quota query", method: http.MethodPost, path: "/admin/upstream-accounts/0123456789abcdef/quota"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, test := range []struct {
				name       string
				role       string
				wantStatus int
				wantCalled bool
			}{
				{name: "member rejected", role: store.UserRoleMember, wantStatus: http.StatusForbidden},
				{name: "owner accepted", role: store.UserRoleOwner, wantStatus: http.StatusNoContent, wantCalled: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					called := false
					handler := server.ownerOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						called = true
						w.WriteHeader(http.StatusNoContent)
					}))
					request := httptest.NewRequest(endpoint.method, endpoint.path, nil)
					request = request.WithContext(context.WithValue(
						request.Context(), userContextKey, store.User{ID: "user-1", Role: test.role},
					))
					response := httptest.NewRecorder()

					handler.ServeHTTP(response, request)
					if response.Code != test.wantStatus || called != test.wantCalled {
						t.Fatalf("status=%d called=%t", response.Code, called)
					}
					if !test.wantCalled && !strings.Contains(response.Body.String(), "owner_required") {
						t.Fatalf("unexpected response: %s", response.Body.String())
					}
				})
			}
		})
	}
}

func TestUpstreamQuotaRouteRequiresSameOriginBrowserRequest(t *testing.T) {
	server := &Server{
		config: config.Config{RPOrigins: []string{"https://gateway.example"}},
		mux:    http.NewServeMux(),
	}
	server.routes()

	for _, test := range []struct {
		name          string
		origin        string
		secFetchSite  string
		wantStatus    int
		wantErrorCode string
	}{
		{name: "missing origin", wantStatus: http.StatusForbidden, wantErrorCode: "invalid_origin"},
		{name: "foreign origin", origin: "https://evil.example", wantStatus: http.StatusForbidden, wantErrorCode: "invalid_origin"},
		{name: "cross-site fetch", origin: "https://gateway.example", secFetchSite: "cross-site", wantStatus: http.StatusForbidden, wantErrorCode: "cross_site_request"},
		{name: "same origin reaches authentication", origin: "https://gateway.example", secFetchSite: "same-origin", wantStatus: http.StatusUnauthorized, wantErrorCode: "session_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/admin/upstream-accounts/0123456789abcdef/quota", nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.secFetchSite)
			}
			response := httptest.NewRecorder()

			server.mux.ServeHTTP(response, request)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantErrorCode) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("cache-control = %q", got)
			}
		})
	}
}

func TestUpstreamQuotaLimiterEnforcesConcurrencyAndFiveSecondDebounce(t *testing.T) {
	limiter := newUpstreamQuotaLimiter()
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)

	release, retryAfter, accepted := limiter.begin("0123456789abcdef", now)
	if !accepted || retryAfter != 0 || release == nil {
		t.Fatalf("first begin accepted=%t retry_after=%d release_nil=%t", accepted, retryAfter, release == nil)
	}
	if _, retryAfter, accepted = limiter.begin("0123456789abcdef", now.Add(time.Second)); accepted || retryAfter != 1 {
		t.Fatalf("concurrent begin accepted=%t retry_after=%d", accepted, retryAfter)
	}
	otherRelease, _, otherAccepted := limiter.begin("fedcba9876543210", now.Add(time.Second))
	if !otherAccepted {
		t.Fatal("one account blocked a different account")
	}
	otherRelease()
	release()

	if _, retryAfter, accepted = limiter.begin("0123456789abcdef", now.Add(4500*time.Millisecond)); accepted || retryAfter != 1 {
		t.Fatalf("debounced begin accepted=%t retry_after=%d", accepted, retryAfter)
	}
	release, retryAfter, accepted = limiter.begin("0123456789abcdef", now.Add(upstreamQuotaDebounce))
	if !accepted || retryAfter != 0 {
		t.Fatalf("post-debounce begin accepted=%t retry_after=%d", accepted, retryAfter)
	}
	release()
}

func TestUpstreamQuotaLocalRateLimitIsAudited(t *testing.T) {
	repository, capture := newUpstreamAuditStore(t)
	limiter := newUpstreamQuotaLimiter()
	release, _, accepted := limiter.begin("0123456789abcdef", time.Now().UTC())
	if !accepted {
		t.Fatal("failed to prime quota limiter")
	}
	defer release()

	server := &Server{store: repository, quotas: limiter}
	response := httptest.NewRecorder()
	server.upstreamAccountQuota(response, quotaTestRequest("0123456789abcdef"))
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "upstream_quota_query_rate_limited") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	assertQuotaAudit(t, capture.snapshot(), false, "upstream_quota_query_rate_limited", http.StatusTooManyRequests)
}

func TestParseUpstreamAccountQuery(t *testing.T) {
	now := time.Date(2026, time.August, 24, 20, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))

	query, err := parseUpstreamAccountQuery(now, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if query.All || query.From.Format(time.RFC3339) != "2026-08-01T00:00:00Z" || query.Until.Format(time.RFC3339) != "2026-08-24T12:30:00Z" {
		t.Fatalf("default query = %+v", query)
	}

	query, err = parseUpstreamAccountQuery(now, url.Values{
		"from": {"2026-08-01"}, "until": {"2026-08-10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if query.From.Format(time.RFC3339) != "2026-08-01T00:00:00Z" || query.Until.Format(time.RFC3339) != "2026-08-11T00:00:00Z" {
		t.Fatalf("bounded query = %+v", query)
	}

	query, err = parseUpstreamAccountQuery(now, url.Values{"all": {"true"}})
	if err != nil || !query.All {
		t.Fatalf("all-history query=%+v err=%v", query, err)
	}

	invalid := map[string]url.Values{
		"unsupported model": {"model": {"gpt-5"}},
		"unknown filter":    {"account": {"0123456789abcdef"}},
		"duplicate from":    {"from": {"2026-08-01", "2026-08-02"}},
		"reversed":          {"from": {"2026-08-10"}, "until": {"2026-08-01"}},
		"all and date":      {"all": {"true"}, "from": {"2026-08-01"}},
		"over ninety days":  {"from": {"2026-01-01"}, "until": {"2026-08-01"}},
		"expired detail":    {"from": {"2026-05-01"}, "until": {"2026-05-15"}},
	}
	for name, values := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := parseUpstreamAccountQuery(now, values); err == nil {
				t.Fatalf("accepted invalid values: %v", values)
			}
		})
	}
	if _, err := parseUpstreamAccountQuery(now, url.Values{
		"from": {"2026-05-01"}, "until": {"2026-05-15"},
	}); !errors.Is(err, errUpstreamAccountRangeExpired) {
		t.Fatalf("expired detail error = %v", err)
	}
}

func TestUpstreamAccountExpiredBoundedRangeReturnsActionableError(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/admin/upstream-accounts?from=2020-01-01&until=2020-01-02", nil)
	response := httptest.NewRecorder()
	(&Server{}).upstreamAccountsJSON(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "upstream_account_detail_expired") ||
		!strings.Contains(response.Body.String(), "全部历史") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUpstreamAccountsResponseOmitsEmptyUnattributedGroup(t *testing.T) {
	raw, err := json.Marshal(upstreamAccountsResponse{Accounts: []upstreamAccountDTO{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "unattributed") {
		t.Fatalf("empty unattributed group was serialized: %s", raw)
	}
}

func TestUpstreamAccountsFallsBackToDurableLocalSummary(t *testing.T) {
	db := sql.OpenDB(upstreamSummaryConnector{})
	t.Cleanup(func() { _ = db.Close() })
	baseURL, _ := url.Parse("http://sidecar.internal")
	server := &Server{
		store: store.New(db),
		upstream: gatewayproxy.NewWithHTTPClient(baseURL, "sidecar-secret", &http.Client{
			Transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("sidecar offline")
			}),
		}),
	}
	response := httptest.NewRecorder()
	server.upstreamAccountsJSON(response, httptest.NewRequest(http.MethodGet, "/admin/upstream-accounts", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		SyncWarning string               `json:"sync_warning"`
		Accounts    []upstreamAccountDTO `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SyncWarning != "upstream_account_sync_unavailable" || len(payload.Accounts) != 1 ||
		payload.Accounts[0].ID != "0123456789abcdef" || payload.Accounts[0].RequestCount != 3 {
		t.Fatalf("fallback payload = %+v", payload)
	}
}

func TestUpstreamQuotaMapsReauthenticationAndTimeouts(t *testing.T) {
	baseURL, _ := url.Parse("http://sidecar.internal")
	for _, test := range []struct {
		name       string
		transport  http.RoundTripper
		wantStatus int
		wantCode   string
	}{
		{name: "reauthentication", transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"secret"}`))}, nil
		}), wantStatus: http.StatusServiceUnavailable, wantCode: "upstream_reauthentication_required"},
		{name: "timeout", transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		}), wantStatus: http.StatusGatewayTimeout, wantCode: "sidecar_timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, capture := newUpstreamAuditStore(t)
			server := &Server{
				store:    repository,
				upstream: gatewayproxy.NewWithHTTPClient(baseURL, "sidecar-secret", &http.Client{Transport: test.transport}),
				quotas:   newUpstreamQuotaLimiter(),
			}
			response := httptest.NewRecorder()
			server.upstreamAccountQuota(response, quotaTestRequest("0123456789abcdef"))
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantCode) || strings.Contains(response.Body.String(), "secret") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			assertQuotaAudit(t, capture.snapshot(), false, test.wantCode, test.wantStatus)
		})
	}
}

type quotaRoundTripFunc func(*http.Request) (*http.Response, error)

func (function quotaRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type upstreamSummaryConnector struct{}

func (upstreamSummaryConnector) Connect(context.Context) (driver.Conn, error) {
	return upstreamSummaryConn{}, nil
}

func (upstreamSummaryConnector) Driver() driver.Driver { return upstreamSummaryDriver{} }

type upstreamSummaryDriver struct{}

func (upstreamSummaryDriver) Open(string) (driver.Conn, error) { return upstreamSummaryConn{}, nil }

type upstreamSummaryConn struct{}

func (upstreamSummaryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is unsupported")
}

func (upstreamSummaryConn) Close() error { return nil }

func (upstreamSummaryConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are unsupported")
}

func (upstreamSummaryConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "WITH usage_source AS") {
		return nil, errors.New("unexpected summary query")
	}
	return &upstreamAuditRows{
		columns: []string{
			"upstream_account_id", "masked_email", "plan", "status", "last_synced_at",
			"request_count", "error_count", "input_tokens", "cached_input_tokens",
			"cache_write_tokens", "output_tokens", "reasoning_tokens", "equivalent_cost_usd",
		},
		values: []driver.Value{
			"0123456789abcdef", "u***@example.com", "plus", "available",
			time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC),
			int64(3), int64(1), int64(100), int64(20), int64(10), int64(30), int64(5), "0.125",
		},
	}, nil
}

func TestUpstreamQuotaErrorAndAuditAreSanitized(t *testing.T) {
	const sensitive = "Bearer oauth-secret access_token=raw"
	upstreamCalls := 0
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != "/internal/upstream-accounts/0123456789abcdef/quota" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"access_token":"`+sensitive+`"}`)
	}))
	defer sidecar.Close()

	repository, capture := newUpstreamAuditStore(t)
	baseURL, _ := url.Parse(sidecar.URL)
	server := &Server{
		store: repository, upstream: gatewayproxy.NewWithHTTPClient(baseURL, "sidecar-secret", sidecar.Client()),
		quotas: newUpstreamQuotaLimiter(),
	}
	request := quotaTestRequest("0123456789abcdef")
	response := httptest.NewRecorder()
	server.upstreamAccountQuota(response, request)

	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "7" {
		t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	if upstreamCalls != 1 || !strings.Contains(response.Body.String(), "upstream_quota_rate_limited") {
		t.Fatalf("calls=%d body=%s", upstreamCalls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), sensitive) || strings.Contains(response.Body.String(), "access_token") {
		t.Fatalf("sensitive upstream response leaked: %s", response.Body.String())
	}

	assertQuotaAudit(t, capture.snapshot(), false, "upstream_quota_rate_limited", http.StatusTooManyRequests)
}

func TestUpstreamQuotaResponseAndSuccessAuditContainOnlyNormalizedFields(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"queried_at": now, "plan": "plus",
			"five_hour": map[string]any{"used_ratio": 0.4, "remaining_ratio": 0.6, "reset_at": now.Add(time.Hour)},
			"seven_day": map[string]any{"used_ratio": 0.25, "remaining_ratio": 0.75, "reset_at": now.Add(7 * 24 * time.Hour)},
			"additional_windows": []map[string]any{{
				"name": "codex_other", "used_ratio": 0.1, "remaining_ratio": 0.9,
				"reset_at": now.Add(30 * time.Minute), "window_seconds": 1800,
			}},
		})
	}))
	defer sidecar.Close()

	repository, capture := newUpstreamAuditStore(t)
	baseURL, _ := url.Parse(sidecar.URL)
	server := &Server{
		store: repository, upstream: gatewayproxy.NewWithHTTPClient(baseURL, "sidecar-secret", sidecar.Client()),
		quotas: newUpstreamQuotaLimiter(),
	}
	response := httptest.NewRecorder()
	server.upstreamAccountQuota(response, quotaTestRequest("0123456789abcdef"))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, payload, "queried_at", "plan", "five_hour", "seven_day", "additional_windows")
	assertExactJSONKeys(t, payload["five_hour"].(map[string]any), "used_ratio", "remaining_ratio", "resets_at")
	additional := payload["additional_windows"].([]any)
	if len(additional) != 1 {
		t.Fatalf("additional windows = %#v", additional)
	}
	if additional[0].(map[string]any)["name"] != "additional_1" {
		t.Fatalf("additional window name was not gateway-generated: %#v", additional[0])
	}
	assertExactJSONKeys(t, additional[0].(map[string]any), "name", "used_ratio", "remaining_ratio", "resets_at", "window_seconds")
	for _, forbidden := range []string{"token", "email", "raw", "authorization"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("forbidden field %q in quota DTO: %s", forbidden, response.Body.String())
		}
	}
	assertQuotaAudit(t, capture.snapshot(), true, "ok", http.StatusOK)
}

func TestUpstreamManagementErrorRejectsDataDerivedCodesAndRetryValues(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/admin/upstream-accounts/0123456789abcdef/quota", nil)
	response := httptest.NewRecorder()
	err := &gatewayproxy.InternalAPIError{
		StatusCode: http.StatusBadGateway, Code: "access_token", RetryAfter: 86400,
		Cause: errors.New("Bearer should-never-appear"),
	}
	writeUpstreamManagementError(response, request, err, "无法查询官方额度")

	if response.Code != http.StatusBadGateway || response.Header().Get("Retry-After") != "" {
		t.Fatalf("status=%d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}
	if body := response.Body.String(); !strings.Contains(body, "sidecar_request_failed") ||
		strings.Contains(body, "access_token") || strings.Contains(body, "Bearer") {
		t.Fatalf("unsafe error response: %s", body)
	}
	code, status := upstreamManagementErrorDetails(err)
	if code != "sidecar_request_failed" || status != http.StatusBadGateway {
		t.Fatalf("audit details code=%q status=%d", code, status)
	}
}

func quotaTestRequest(accountID string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/admin/upstream-accounts/"+accountID+"/quota", nil)
	request.SetPathValue("id", accountID)
	ctx := context.WithValue(request.Context(), userContextKey, store.User{ID: "owner-1", Role: store.UserRoleOwner})
	ctx = context.WithValue(ctx, sessionContextKey, store.Session{ID: "session-1", UserID: "owner-1"})
	return request.WithContext(ctx)
}

func assertExactJSONKeys(t *testing.T, value map[string]any, keys ...string) {
	t.Helper()
	want := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		want[key] = struct{}{}
	}
	if len(value) != len(want) {
		t.Fatalf("keys=%v want=%v", value, keys)
	}
	for key := range value {
		if _, ok := want[key]; !ok {
			t.Fatalf("unexpected key %q in %v", key, value)
		}
	}
}

type upstreamAuditCapture struct {
	mu   sync.Mutex
	args []driver.NamedValue
}

func (c *upstreamAuditCapture) record(args []driver.NamedValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.args = append([]driver.NamedValue(nil), args...)
}

func (c *upstreamAuditCapture) snapshot() []driver.NamedValue {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]driver.NamedValue(nil), c.args...)
}

func newUpstreamAuditStore(t *testing.T) (*store.Store, *upstreamAuditCapture) {
	t.Helper()
	capture := &upstreamAuditCapture{}
	db := sql.OpenDB(upstreamAuditConnector{capture: capture})
	t.Cleanup(func() { _ = db.Close() })
	return store.New(db), capture
}

type upstreamAuditConnector struct{ capture *upstreamAuditCapture }

func (c upstreamAuditConnector) Connect(context.Context) (driver.Conn, error) {
	return &upstreamAuditConn{capture: c.capture}, nil
}

func (upstreamAuditConnector) Driver() driver.Driver { return upstreamAuditDriver{} }

type upstreamAuditDriver struct{}

func (upstreamAuditDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type upstreamAuditConn struct{ capture *upstreamAuditCapture }

func (*upstreamAuditConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is unsupported")
}

func (*upstreamAuditConn) Close() error { return nil }

func (*upstreamAuditConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are unsupported")
}

func (c *upstreamAuditConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	c.capture.record(args)
	if len(args) != 12 {
		return nil, errors.New("unexpected audit argument count")
	}
	return &upstreamAuditRows{
		columns: []string{
			"id", "occurred_at", "actor_user_id", "actor_session_id", "actor_api_key_id",
			"event_type", "severity", "success", "source_ip", "subject_type", "subject_id",
			"request_id", "metadata",
		},
		values: []driver.Value{
			int64(1), args[0].Value, args[1].Value, args[2].Value, args[3].Value,
			args[4].Value, args[5].Value, args[6].Value, args[7].Value, args[8].Value,
			args[9].Value, args[10].Value, args[11].Value,
		},
	}, nil
}

type upstreamAuditRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *upstreamAuditRows) Columns() []string { return r.columns }

func (*upstreamAuditRows) Close() error { return nil }

func (r *upstreamAuditRows) Next(destination []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(destination, r.values)
	r.done = true
	return nil
}

func assertQuotaAudit(t *testing.T, args []driver.NamedValue, success bool, code string, status int) {
	t.Helper()
	if len(args) != 12 {
		t.Fatalf("audit args = %#v", args)
	}
	if args[1].Value != "owner-1" || args[2].Value != "session-1" ||
		args[4].Value != "upstream_account.quota_queried" || args[6].Value != success ||
		args[8].Value != "upstream_account" || args[9].Value != "0123456789abcdef" {
		t.Fatalf("audit identity fields = %#v", args)
	}
	metadataBytes, ok := args[11].Value.([]byte)
	if !ok {
		t.Fatalf("audit metadata type = %T", args[11].Value)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, metadata, "duration_ms", "result_code", "http_status")
	if metadata["result_code"] != code || metadata["http_status"] != float64(status) {
		t.Fatalf("audit metadata = %#v", metadata)
	}
	encoded := strings.ToLower(string(metadataBytes))
	for _, forbidden := range []string{"token", "email", "authorization", "response", "bearer"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("audit metadata contains %q: %s", forbidden, metadataBytes)
		}
	}
}
