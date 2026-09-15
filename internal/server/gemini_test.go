package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestGeminiModelAccessUsesExistingCatalogIntersection(t *testing.T) {
	t.Parallel()
	const model = "gemini-3.1-pro-preview"
	pricing := config.UsagePricing{Models: map[string]config.ModelPricing{model: {}, "gpt-6-astra": {}}}
	if !requiresUserModelAccess(http.MethodPost, model, pricing) {
		t.Fatal("Gemini generation did not require user model access")
	}
	for _, test := range []struct {
		name    string
		enabled []string
		key     []string
		want    map[string]struct{}
	}{
		{name: "enabled and unrestricted key", enabled: []string{model}, want: map[string]struct{}{model: {}}},
		{name: "user disabled", key: []string{model}, want: map[string]struct{}{}},
		{name: "key restricted", enabled: []string{model}, key: []string{"gpt-6-astra"}, want: map[string]struct{}{}},
		{name: "unpriced alias excluded", enabled: []string{"gemini-unpriced"}, want: map[string]struct{}{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := intersectAllowedModels(pricing, test.enabled, test.key); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("allowed models = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestGeminiRejectionsNeverReserveQuotaOrBill(t *testing.T) {
	t.Parallel()

	pricing, err := config.ParseUsagePricing(`{
		"schema_version":2,"catalog_as_of":"2026-09-15","fx_as_of":"2026-09-15","usd_cny_rate":"7.2",
		"fallback_policy":{"unknown_service_tier":"max_published","missing_price_combination":"max_published","missing_cache_write_tokens":"all_uncached_as_write"},
		"models":{"gemini-3.1-pro-preview":{
			"cache_write_mode":"included_in_input","max_input_tokens":1048576,"long_context_threshold_tokens":200000,
			"service_tiers":{"standard":{
				"short":{"input_usd_per_million":"2","cached_input_usd_per_million":"0.2","output_usd_per_million":"12"},
				"long":{"input_usd_per_million":"4","cached_input_usd_per_million":"0.4","output_usd_per_million":"18"}
			}}
		}}
	}`)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name            string
		path            string
		body            string
		keyAllowlist    string
		modelEnabled    bool
		missingAccess   bool
		wantStatus      int
		wantCode        string
		wantAccessReads int
		wantBegins      int64
	}{
		{
			name: "compact is unsupported", path: "/v1/responses/compact", modelEnabled: true,
			wantStatus: http.StatusNotImplemented, wantCode: "endpoint_not_supported", wantAccessReads: 1,
		},
		{
			name: "streaming compact is also unsupported", path: "/v1/responses/compact", modelEnabled: true,
			body:       `{"model":"gemini-3.1-pro-preview","stream":true,"input":[]}`,
			wantStatus: http.StatusNotImplemented, wantCode: "endpoint_not_supported", wantAccessReads: 1,
		},
		{
			name: "compact respects key permission", path: "/v1/responses/compact", keyAllowlist: `["gpt-6-astra"]`,
			wantStatus: http.StatusForbidden, wantCode: "model_not_allowed",
		},
		{
			name: "compact respects user permission", path: "/v1/responses/compact",
			wantStatus: http.StatusForbidden, wantCode: "model_not_allowed", wantAccessReads: 1,
		},
		{
			name: "compact fails closed for missing user permission", path: "/v1/responses/compact", missingAccess: true,
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error", wantAccessReads: 1,
		},
		{
			name: "responses respects key permission", path: "/v1/responses", keyAllowlist: `["gpt-6-astra"]`,
			wantStatus: http.StatusForbidden, wantCode: "model_not_allowed",
		},
		{
			name: "responses respects user permission", path: "/v1/responses",
			wantStatus: http.StatusForbidden, wantCode: "model_not_allowed", wantAccessReads: 1, wantBegins: 1,
		},
		{
			name: "responses fails closed for missing user permission", path: "/v1/responses", missingAccess: true,
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error", wantAccessReads: 1, wantBegins: 1,
		},
		{
			name: "compact still requires a configured model", path: "/v1/responses/compact",
			body:       `{"model":"gemini-unpriced","input":[]}`,
			wantStatus: http.StatusBadRequest, wantCode: "model_pricing_not_found",
		},
		{
			name: "compact cannot hide a duplicate model", path: "/v1/responses/compact",
			body:       `{"model":"gemini-3.1-pro-preview","model":"gpt-6-astra","input":[]}`,
			wantStatus: http.StatusBadRequest, wantCode: "model_required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newResponsesWebSocketTestHarness(t)
			harness.server.config.BodyLimit = 64 << 20
			harness.server.config.UsagePricing = pricing
			database := &geminiAdmissionTestConnector{
				auth: harness.database, keyAllowlist: test.keyAllowlist,
				modelEnabled: test.modelEnabled, missingAccess: test.missingAccess,
			}
			db := sql.OpenDB(database)
			t.Cleanup(func() { _ = db.Close() })
			harness.server.store = store.New(db)
			body := test.body
			if body == "" {
				body = `{"model":"gemini-3.1-pro-preview","input":[]}`
			}
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+harness.apiKey)
			response := httptest.NewRecorder()

			harness.handler.ServeHTTP(response, request)

			var result httpx.ErrorBody
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.wantStatus || result.Error.Code != test.wantCode {
				t.Fatalf("response = %d %s, want %d %s", response.Code, response.Body, test.wantStatus, test.wantCode)
			}
			if got := harness.upstreamCalls.Load(); got != 0 {
				t.Fatalf("upstream calls = %d, want 0", got)
			}
			queries := harness.database.queriesSnapshot()
			if len(queries) != 1+test.wantAccessReads || !strings.Contains(queries[0], "FROM api_keys k") {
				t.Fatalf("queries = %#v, want authentication and %d permission reads", queries, test.wantAccessReads)
			}
			if test.wantAccessReads > 0 && !strings.Contains(queries[1], "SELECT a.enabled") {
				t.Fatalf("unexpected post-authentication query: %s", queries[1])
			}
			if database.begins.Load() != test.wantBegins || database.commits.Load() != 0 || database.writes.Load() != 0 {
				t.Fatalf("database activity: begins=%d commits=%d writes=%d", database.begins.Load(), database.commits.Load(), database.writes.Load())
			}
			if database.rollbacks.Load() != test.wantBegins {
				t.Fatalf("rollbacks = %d, want %d", database.rollbacks.Load(), test.wantBegins)
			}
		})
	}
}

// This driver accepts authentication and model permission reads only. Any
// quota, usage, or billing write is recorded and rejected, including writes
// hidden inside a transaction that would subsequently roll back.
type geminiAdmissionTestConnector struct {
	auth          *responsesWebSocketTestDatabase
	keyAllowlist  string
	modelEnabled  bool
	missingAccess bool
	begins        atomic.Int64
	commits       atomic.Int64
	rollbacks     atomic.Int64
	writes        atomic.Int64
}

func (c *geminiAdmissionTestConnector) Connect(context.Context) (driver.Conn, error) {
	return geminiAdmissionTestConn{responsesWebSocketTestConn{database: c.auth}, c}, nil
}

func (c *geminiAdmissionTestConnector) Driver() driver.Driver {
	return responsesWebSocketTestDriver{database: c.auth}
}

type geminiAdmissionTestConn struct {
	responsesWebSocketTestConn
	fixture *geminiAdmissionTestConnector
}

func (c geminiAdmissionTestConn) Begin() (driver.Tx, error) {
	c.fixture.begins.Add(1)
	return geminiAdmissionTestTx{fixture: c.fixture}, nil
}

func (c geminiAdmissionTestConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT a.enabled") {
		c.database.recordQuery(query)
		if len(args) != 2 || args[0].Value != c.database.userID || args[1].Value != "gemini-3.1-pro-preview" {
			return nil, errors.New("unexpected model permission lookup")
		}
		rows := &responsesWebSocketTestRows{columns: []string{"enabled"}}
		if !c.fixture.missingAccess {
			rows.values = [][]driver.Value{{c.fixture.modelEnabled}}
		}
		return rows, nil
	}
	rows, err := c.responsesWebSocketTestConn.QueryContext(ctx, query, args)
	if err == nil && c.fixture.keyAllowlist != "" && strings.Contains(query, "FROM api_keys k") {
		rows.(*responsesWebSocketTestRows).values[0][9] = []byte(c.fixture.keyAllowlist)
	}
	return rows, err
}

func (c geminiAdmissionTestConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.fixture.writes.Add(1)
	return nil, errors.New("unexpected database write for rejected Gemini request")
}

type geminiAdmissionTestTx struct {
	fixture *geminiAdmissionTestConnector
}

func (tx geminiAdmissionTestTx) Commit() error {
	tx.fixture.commits.Add(1)
	return nil
}

func (tx geminiAdmissionTestTx) Rollback() error {
	tx.fixture.rollbacks.Add(1)
	return nil
}
