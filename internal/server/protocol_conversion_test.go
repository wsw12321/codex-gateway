package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type fakeProtocolConversion struct {
	settings store.ProtocolConversionSettings
	err      error
	reads    atomic.Int64
}

func (f *fakeProtocolConversion) ProtocolConversionSettings(context.Context) (store.ProtocolConversionSettings, error) {
	f.reads.Add(1)
	return f.settings, f.err
}

func (f *fakeProtocolConversion) SetProtocolConversionSettings(context.Context, store.SetProtocolConversionSettingsParams) (store.ProtocolConversionSettings, error) {
	return f.settings, f.err
}

func TestClaudeResponsesRequireProtocolConversion(t *testing.T) {
	for _, test := range []struct {
		name, path, body, wantCode, wantMessage string
		routes                                  map[string]string
		enabled                                 bool
		readErr                                 error
		wantStatus                              int
		wantReads                               int64
	}{
		{name: "disabled by default", path: "/v1/responses", body: `{"model":"claude-test","input":"hi"}`,
			wantStatus: 400, wantCode: "protocol_conversion_disabled", wantMessage: "/v1/messages", wantReads: 1},
		{name: "routed target is named", path: "/v1/responses", body: `{"model":"gpt-alias","input":"hi"}`, routes: map[string]string{"gpt-alias": "claude-test"},
			wantStatus: 400, wantCode: "model_route_incompatible", wantMessage: "gpt-alias → claude-test 不适用于此接口：管理员未开启模型协议转换", wantReads: 1},
		{name: "unreadable setting fails closed", path: "/v1/responses", body: `{"model":"claude-test","input":"hi"}`, readErr: errors.New("database down"),
			wantStatus: 503, wantCode: "protocol_conversion_unavailable", wantReads: 1},
		// Enabled requests pass the gate and stop at the harness's empty pricing.
		{name: "enabled passes the gate", path: "/v1/responses", body: `{"model":"claude-test","input":"hi"}`, enabled: true,
			wantStatus: 400, wantCode: "model_pricing_not_found", wantReads: 1},
		{name: "native messages never read the setting", path: "/v1/messages", body: `{"model":"claude-test","max_tokens":1,"messages":[],"speed":"fast"}`,
			wantStatus: 400, wantCode: "service_tier_not_supported", wantReads: 0},
		{name: "codex models never read the setting", path: "/v1/responses", body: `{"model":"gpt-test","input":"hi"}`,
			wantStatus: 400, wantCode: "model_pricing_not_found", wantReads: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newResponsesWebSocketTestHarness(t)
			h.server.config.BodyLimit = 1 << 20
			h.server.modelRouteRepo = &fakeModelRoutes{routes: test.routes}
			conversion := &fakeProtocolConversion{settings: store.ProtocolConversionSettings{ClaudeResponsesEnabled: test.enabled, Revision: 1}, err: test.readErr}
			h.server.protocolConversionRepo = conversion
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			if strings.HasPrefix(test.path, "/v1/messages") {
				request.Header.Set("X-Api-Key", h.apiKey)
			} else {
				request.Header.Set("Authorization", "Bearer "+h.apiKey)
			}
			recorder := httptest.NewRecorder()
			h.handler.ServeHTTP(recorder, request)
			var response httpx.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.wantStatus || response.Error.Code != test.wantCode || !strings.Contains(response.Error.Message, test.wantMessage) {
				t.Fatalf("response %d %s", recorder.Code, recorder.Body)
			}
			if reads := conversion.reads.Load(); reads != test.wantReads {
				t.Fatalf("setting reads=%d, want %d", reads, test.wantReads)
			}
			if calls := h.upstreamCalls.Load(); calls != 0 {
				t.Fatalf("upstream calls=%d", calls)
			}
			if queries := h.database.queriesSnapshot(); len(queries) != 1 || !strings.Contains(queries[0], "FROM api_keys k") {
				t.Fatalf("database queries=%#v, want only API key authentication", queries)
			}
		})
	}
}
