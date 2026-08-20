package server

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestResponsesWebSocketNegotiation(t *testing.T) {
	t.Parallel()

	harness := newResponsesWebSocketTestHarness(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer "+harness.apiKey)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()

	harness.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUpgradeRequired)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Type != "invalid_request_error" ||
		body.Error.Code != "responses_websocket_unsupported" {
		t.Fatalf("unexpected error response: %#v", body.Error)
	}
	if got := harness.upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
	if queries := harness.database.queriesSnapshot(); len(queries) != 1 ||
		!strings.Contains(queries[0], "FROM api_keys k") {
		t.Fatalf("database queries = %#v, want only API key authentication", queries)
	}
}

func TestResponsesWebSocketNegotiationRequiresAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		authorization func(*testing.T) string
	}{
		{name: "missing credential"},
		{
			name: "incorrect credential",
			authorization: func(t *testing.T) string {
				t.Helper()
				generated, err := security.GenerateAPIKeyFrom(bytes.NewReader(bytes.Repeat([]byte{0x9a}, security.APIKeyPublicIDBytes+security.APIKeySecretBytes)))
				if err != nil {
					t.Fatalf("generate incorrect API key: %v", err)
				}
				return "Bearer " + generated.Token
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newResponsesWebSocketTestHarness(t)
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			if test.authorization != nil {
				request.Header.Set("Authorization", test.authorization(t))
			}
			recorder := httptest.NewRecorder()

			harness.handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
			}
			var body httpx.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if body.Error.Code != "invalid_api_key" {
				t.Fatalf("error code = %q, want invalid_api_key", body.Error.Code)
			}
			if got := harness.upstreamCalls.Load(); got != 0 {
				t.Fatalf("upstream calls = %d, want 0", got)
			}
		})
	}
}

func TestResponsesRejectsUnknownModelBeforeServiceTier(t *testing.T) {
	t.Parallel()

	pricing, err := config.ParseUsagePricing(`{
		"schema_version":2,
		"catalog_as_of":"2026-08-20",
		"fx_as_of":"2026-08-20",
		"usd_cny_rate":"7.2",
		"fallback_policy":{
			"unknown_service_tier":"max_published",
			"missing_price_combination":"max_published",
			"missing_cache_write_tokens":"all_uncached_as_write"
		},
		"models":{"gpt-known":{
			"cache_write_mode":"included_in_input",
			"max_input_tokens":272000,
			"long_context_threshold_tokens":272000,
			"service_tiers":{"standard":{"short":{
				"input_usd_per_million":"1",
				"cached_input_usd_per_million":"0.1",
				"output_usd_per_million":"2"
			}}}
		}}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	lateUltrafast := `{"model":"gpt-known","input":"` + strings.Repeat("x", maxModelPrefix+1024) + `","service_tier":"ultrafast"}`

	for _, test := range []struct {
		name     string
		body     string
		wantCode string
	}{
		{
			name:     "unknown model takes precedence over otherwise valid tier",
			body:     `{"model":"gpt-unknown","service_tier":"priority","input":[]}`,
			wantCode: "model_pricing_not_found",
		},
		{
			name:     "unknown model also takes precedence over ultrafast",
			body:     `{"model":"gpt-unknown","service_tier":"ultrafast","input":[]}`,
			wantCode: "model_pricing_not_found",
		},
		{
			name:     "ultrafast is rejected for known model",
			body:     `{"model":"gpt-known","service_tier":"ultrafast","input":[]}`,
			wantCode: "service_tier_not_supported",
		},
		{
			name:     "ultrafast after former routing prefix is rejected",
			body:     lateUltrafast,
			wantCode: "service_tier_not_supported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newResponsesWebSocketTestHarness(t)
			harness.server.config.BodyLimit = 64 << 20
			harness.server.config.UsagePricing = pricing
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+harness.apiKey)
			recorder := httptest.NewRecorder()

			harness.handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			var body httpx.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if body.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", body.Error.Code, test.wantCode)
			}
			if got := harness.upstreamCalls.Load(); got != 0 {
				t.Fatalf("upstream calls = %d, want 0", got)
			}
			if queries := harness.database.queriesSnapshot(); len(queries) != 1 ||
				!strings.Contains(queries[0], "FROM api_keys k") {
				t.Fatalf("database queries = %#v, want only API key authentication", queries)
			}
		})
	}
}

type responsesWebSocketTestHarness struct {
	handler       http.Handler
	server        *Server
	apiKey        string
	database      *responsesWebSocketTestDatabase
	upstreamCalls *atomic.Int64
}

func newResponsesWebSocketTestHarness(t *testing.T) responsesWebSocketTestHarness {
	t.Helper()

	keyMaterial := bytes.Repeat([]byte{0x3c}, security.APIKeyPublicIDBytes+security.APIKeySecretBytes)
	generated, err := security.GenerateAPIKeyFrom(bytes.NewReader(keyMaterial))
	if err != nil {
		t.Fatalf("generate API key: %v", err)
	}
	pepper := bytes.Repeat([]byte{0x6d}, security.MinimumPepperBytes)
	digest, err := security.HashAPIKey(pepper, generated.Token)
	if err != nil {
		t.Fatalf("hash API key: %v", err)
	}
	database := &responsesWebSocketTestDatabase{
		publicID: generated.PublicID,
		keyHash:  append([]byte(nil), digest[:]...),
	}
	db := sql.OpenDB(responsesWebSocketTestConnector{database: database})
	t.Cleanup(func() { _ = db.Close() })

	upstreamCalls := &atomic.Int64{}
	upstreamURL, err := url.Parse("https://upstream.invalid")
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	server := &Server{
		config: config.Config{KeyPepper: pepper},
		store:  store.New(db),
		upstream: gatewayproxy.NewWithHTTPClient(upstreamURL, "internal-test-token", &http.Client{
			Transport: responsesWebSocketTestRoundTripper{calls: upstreamCalls},
		}),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		mux:      http.NewServeMux(),
		attempts: newAttemptLimiter(),
	}
	server.routes()
	return responsesWebSocketTestHarness{
		handler:       server.Handler(),
		server:        server,
		apiKey:        generated.Token,
		database:      database,
		upstreamCalls: upstreamCalls,
	}
}

type responsesWebSocketTestRoundTripper struct {
	calls *atomic.Int64
}

func (t responsesWebSocketTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected upstream call")
}

type responsesWebSocketTestDatabase struct {
	mu       sync.Mutex
	queries  []string
	publicID string
	keyHash  []byte
}

func (d *responsesWebSocketTestDatabase) recordQuery(query string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, query)
}

func (d *responsesWebSocketTestDatabase) queriesSnapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.queries...)
}

type responsesWebSocketTestConnector struct {
	database *responsesWebSocketTestDatabase
}

func (c responsesWebSocketTestConnector) Connect(context.Context) (driver.Conn, error) {
	return responsesWebSocketTestConn{database: c.database}, nil
}

func (c responsesWebSocketTestConnector) Driver() driver.Driver {
	return responsesWebSocketTestDriver{database: c.database}
}

type responsesWebSocketTestDriver struct {
	database *responsesWebSocketTestDatabase
}

func (d responsesWebSocketTestDriver) Open(string) (driver.Conn, error) {
	return responsesWebSocketTestConn{database: d.database}, nil
}

type responsesWebSocketTestConn struct {
	database *responsesWebSocketTestDatabase
}

func (c responsesWebSocketTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared statement")
}

func (c responsesWebSocketTestConn) Close() error { return nil }

func (c responsesWebSocketTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}

func (c responsesWebSocketTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.database.recordQuery(query)
	if !strings.Contains(query, "FROM api_keys k") || len(args) != 1 || args[0].Value != c.database.publicID {
		return &responsesWebSocketTestRows{}, nil
	}
	now := time.Now().UTC()
	return &responsesWebSocketTestRows{
		columns: make([]string, 23),
		values: [][]driver.Value{{
			"key-id", c.database.publicID, "cgk_v1_test_", c.database.keyHash,
			"user-id", "device-id", nil, "Codex CLI", store.StatusActive,
			[]byte("[]"), nil, nil, nil, nil, now, now.Add(time.Hour), nil, nil,
			"", nil, store.StatusActive, store.StatusActive, nil,
		}},
	}, nil
}

type responsesWebSocketTestRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *responsesWebSocketTestRows) Columns() []string { return r.columns }
func (r *responsesWebSocketTestRows) Close() error      { return nil }

func (r *responsesWebSocketTestRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}

func TestWriteModelPricingNotFound(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	writeModelPricingNotFound(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "model_pricing_not_found" ||
		body.Error.Message != "模型未记录计费价格，请联系管理员" {
		t.Fatalf("unexpected error response: %#v", body.Error)
	}
}

func TestWriteInsufficientQuota(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		retryAfter time.Duration
		wantHeader string
	}{
		{name: "without renewal", retryAfter: 0},
		{name: "round renewal up", retryAfter: 1500 * time.Millisecond, wantHeader: "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			writeInsufficientQuota(recorder, request, test.retryAfter)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
			}
			if got := recorder.Header().Get("Retry-After"); got != test.wantHeader {
				t.Fatalf("Retry-After = %q, want %q", got, test.wantHeader)
			}
			var body httpx.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if body.Error.Type != "insufficient_quota" || body.Error.Code != "insufficient_quota" ||
				body.Error.Message != "可用额度不足" {
				t.Fatalf("unexpected error response: %#v", body.Error)
			}
		})
	}
}

func TestRetryUsageCompletionRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	transient := errors.New("temporary database failure")
	attempts := 0
	err := retryUsageCompletion(context.Background(), 3, 0, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return transient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryUsageCompletion: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryUsageCompletionStopsOnDurableErrors(t *testing.T) {
	t.Parallel()

	for _, durable := range []error{store.ErrInvalid, store.ErrConflict, store.ErrNotFound} {
		attempts := 0
		err := retryUsageCompletion(context.Background(), 3, 0, func(context.Context) error {
			attempts++
			return durable
		})
		if !errors.Is(err, durable) {
			t.Fatalf("error = %v, want %v", err, durable)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d for %v, want 1", attempts, durable)
		}
	}
}
