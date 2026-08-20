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

func TestAPIKeyAuthenticationLifecycleStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configure  func(*responsesWebSocketTestDatabase)
		wantStatus int
		wantCode   string
	}{
		{
			name: "disabled key is identified without accepting requests",
			configure: func(database *responsesWebSocketTestDatabase) {
				database.status = store.StatusDisabled
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "key_disabled",
		},
		{
			name: "expired key is invalid",
			configure: func(database *responsesWebSocketTestDatabase) {
				database.expiresAt = time.Now().UTC().Add(-time.Minute)
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_api_key",
		},
		{
			name: "deleted key is unknown",
			configure: func(database *responsesWebSocketTestDatabase) {
				database.exists = false
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_api_key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newResponsesWebSocketTestHarness(t)
			test.configure(harness.database)
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			request.Header.Set("Authorization", "Bearer "+harness.apiKey)
			recorder := httptest.NewRecorder()

			harness.handler.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
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
		})
	}
}

func TestAPIKeyLifecycleRoutesRequireSession(t *testing.T) {
	t.Parallel()

	server := &Server{
		config:   config.Config{RPOrigins: []string{"https://gateway.example"}},
		mux:      http.NewServeMux(),
		attempts: newAttemptLimiter(),
	}
	server.routes()
	for _, route := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: "/admin/api-keys/key-id/reveal", body: "{}"},
		{method: http.MethodPut, path: "/admin/api-keys/key-id/status", body: `{"status":"disabled"}`},
		{method: http.MethodDelete, path: "/admin/api-keys/key-id"},
	} {
		request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		request.Header.Set("Origin", "https://gateway.example")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		recorder := httptest.NewRecorder()

		server.mux.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want %d", route.method, route.path, recorder.Code, http.StatusUnauthorized)
			continue
		}
		var body httpx.ErrorBody
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s decode error response: %v", route.method, route.path, err)
		} else if body.Error.Code != "session_required" {
			t.Errorf("%s %s error code = %q, want session_required", route.method, route.path, body.Error.Code)
		}
	}
}

func TestRevealAPIKeyValidatesStoredSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configure  func(*responsesWebSocketTestHarness)
		wantStatus int
		wantCode   string
		wantSecret bool
	}{
		{name: "valid encrypted secret", wantStatus: http.StatusOK, wantSecret: true},
		{
			name: "legacy key has no encrypted secret",
			configure: func(harness *responsesWebSocketTestHarness) {
				harness.database.secretCiphertext = nil
			},
			wantStatus: http.StatusConflict,
			wantCode:   "api_key_secret_unavailable",
		},
		{
			name: "tampered ciphertext fails closed",
			configure: func(harness *responsesWebSocketTestHarness) {
				tampered := append([]byte(nil), harness.database.secretCiphertext...)
				tampered[len(tampered)-1] ^= 0xff
				harness.database.secretCiphertext = tampered
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
		{
			name: "stored digest mismatch fails closed",
			configure: func(harness *responsesWebSocketTestHarness) {
				harness.database.keyHash = bytes.Repeat([]byte{0x7f}, len(harness.database.keyHash))
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newResponsesWebSocketTestHarness(t)
			if test.configure != nil {
				test.configure(&harness)
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/api-keys/key-id/reveal", strings.NewReader("{}"))
			request.SetPathValue("id", harness.database.keyID)
			request = request.WithContext(context.WithValue(
				request.Context(), userContextKey, store.User{ID: harness.database.userID},
			))
			recorder := httptest.NewRecorder()

			harness.server.revealAPIKey(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if test.wantSecret {
				var body map[string]string
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode reveal response: %v", err)
				}
				if body["api_key"] != harness.apiKey {
					t.Fatal("reveal response did not return the verified API key")
				}
				return
			}
			if strings.Contains(recorder.Body.String(), harness.apiKey) {
				t.Fatal("failed reveal leaked plaintext API key")
			}
			var body httpx.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if body.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", body.Error.Code, test.wantCode)
			}
		})
	}
}

func TestSetAPIKeyStatusRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPut, "/admin/api-keys/key-id/status", strings.NewReader(`{"status":"revoked"}`))
	request.SetPathValue("id", "key-id")
	recorder := httptest.NewRecorder()

	(&Server{}).setAPIKeyStatus(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Code != "invalid_api_key_status" {
		t.Fatalf("error code = %q, want invalid_api_key_status", body.Error.Code)
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
	encryptionKey := bytes.Repeat([]byte{0x42}, security.APIKeyEncryptionKeyBytes)
	ciphertext, err := security.EncryptAPIKeySecret(
		encryptionKey, "user-id", generated.PublicID, generated.Token,
	)
	if err != nil {
		t.Fatalf("encrypt API key: %v", err)
	}
	database := &responsesWebSocketTestDatabase{
		keyID:            "key-id",
		userID:           "user-id",
		publicID:         generated.PublicID,
		keyPrefix:        generated.Prefix,
		keyHash:          append([]byte(nil), digest[:]...),
		secretCiphertext: append([]byte(nil), ciphertext...),
		status:           store.StatusActive,
		expiresAt:        time.Now().UTC().Add(time.Hour),
		exists:           true,
	}
	db := sql.OpenDB(responsesWebSocketTestConnector{database: database})
	t.Cleanup(func() { _ = db.Close() })

	upstreamCalls := &atomic.Int64{}
	upstreamURL, err := url.Parse("https://upstream.invalid")
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	server := &Server{
		config: config.Config{KeyPepper: pepper, APIKeyEncryptionKey: encryptionKey},
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
	mu               sync.Mutex
	queries          []string
	keyID            string
	userID           string
	publicID         string
	keyPrefix        string
	keyHash          []byte
	secretCiphertext []byte
	status           string
	expiresAt        time.Time
	exists           bool
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
	if strings.Contains(query, "SELECT id, user_id, public_id, key_prefix, key_hash, secret_ciphertext") {
		if len(args) != 2 || args[0].Value != c.database.keyID ||
			args[1].Value != c.database.userID || !c.database.exists {
			return &responsesWebSocketTestRows{columns: make([]string, 6)}, nil
		}
		return &responsesWebSocketTestRows{
			columns: make([]string, 6),
			values: [][]driver.Value{{
				c.database.keyID, c.database.userID, c.database.publicID,
				c.database.keyPrefix, c.database.keyHash, c.database.secretCiphertext,
			}},
		}, nil
	}
	if !strings.Contains(query, "FROM api_keys k") || len(args) != 1 ||
		args[0].Value != c.database.publicID || !c.database.exists {
		return &responsesWebSocketTestRows{}, nil
	}
	now := time.Now().UTC()
	return &responsesWebSocketTestRows{
		columns: make([]string, 22),
		values: [][]driver.Value{{
			c.database.keyID, c.database.publicID, c.database.keyPrefix, c.database.keyHash,
			c.database.userID, "device-id", nil, "Codex CLI", c.database.status,
			[]byte("[]"), nil, nil, nil, nil, now, c.database.expiresAt, nil, true,
			nil, store.StatusActive, store.StatusActive, nil,
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
