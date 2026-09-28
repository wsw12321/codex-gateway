package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

const nativeGeminiText = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`

func nativeGeminiPricing(t *testing.T) config.UsagePricing {
	t.Helper()
	data, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return pricing
}

func TestNativeGeminiRejectsBeforeReservation(t *testing.T) {
	for _, tc := range []struct {
		name, model, action, query, body, keyAllowlist string
		disabled, noRoute                              bool
		status                                         int
		state                                          string
		begins                                         int64
	}{
		{name: "key permission", keyAllowlist: `["gpt-6-astra"]`, status: 403, state: "PERMISSION_DENIED"},
		{name: "user permission", status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "retired customtools alias", model: "gemini-3.1-pro-preview-customtools", status: 404, state: "NOT_FOUND"},
		{name: "retired preview alias", model: "gemini-3.1-pro-preview", status: 404, state: "NOT_FOUND"},
		{name: "stream uses canonical permission", action: "streamGenerateContent", query: "?alt=sse", status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "disabled key", disabled: true, status: 403, state: "PERMISSION_DENIED"},
		{name: "unconfigured route", noRoute: true, status: 404, state: "NOT_FOUND"},
		{name: "title model", model: "gemini-2.5-flash-lite", status: 404, state: "NOT_FOUND"},
		{name: "unknown model", model: "gpt-6-astra", status: 404, state: "NOT_FOUND"},
		{name: "unconfigured Flash", model: "gemini-3.8-flash-high", status: 404, state: "NOT_FOUND"},
		{name: "no fuzzy alias", model: "gemini-3.1-pro-preview-customtools-extra", status: 404, state: "NOT_FOUND"},
		{name: "encoded model", model: "gemini%2D3.1-pro-preview", status: 404, state: "NOT_FOUND"},
		{name: "encoded action", action: "%67enerateContent", status: 404, state: "NOT_FOUND"},
		{name: "count tokens", action: "countTokens", status: 404, state: "NOT_FOUND"},
		{name: "query control", query: "?alt=json", status: 400, state: "INVALID_ARGUMENT"},
		{name: "duplicate alt", action: "streamGenerateContent", query: "?alt=sse&alt=sse", status: 400, state: "INVALID_ARGUMENT"},
		{name: "invalid JSON", body: `{"contents":`, status: 400, state: "INVALID_ARGUMENT"},
		{name: "duplicate JSON", body: `{"contents":[],"contents":[]}`, status: 400, state: "INVALID_ARGUMENT"},
		{name: "body model rejected", body: `{"model":"gpt-6-astra","contents":[{"parts":[{"text":"hello"}]}]}`, status: 400, state: "INVALID_ARGUMENT"},
		{name: "unsupported control", body: `{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"temperature":0.1}}`, status: 400, state: "INVALID_ARGUMENT"},
		{name: "unsupported image", body: `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}]}`, status: 400, state: "INVALID_ARGUMENT"},
		{name: "body limit", body: `{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 1<<20) + `"}]}]}`, status: 413, state: "INVALID_ARGUMENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponsesWebSocketTestHarness(t)
			h.server.config.UsagePricing = nativeGeminiPricing(t)
			if !tc.noRoute {
				h.server.config.AntigravityModelRoutes = map[string]string{config.AntigravityPublicModel: config.AntigravityCLIModel}
			}
			if tc.disabled {
				h.database.status = store.StatusDisabled
			}
			database := &geminiAdmissionTestConnector{auth: h.database, keyAllowlist: tc.keyAllowlist}
			db := sql.OpenDB(database)
			t.Cleanup(func() { _ = db.Close() })
			h.server.store = store.New(db)
			if tc.model == "" {
				tc.model = config.AntigravityPublicModel
			}
			if tc.action == "" {
				tc.action = "generateContent"
			}
			if tc.body == "" {
				tc.body = nativeGeminiText
			}
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+tc.model+":"+tc.action+tc.query, strings.NewReader(tc.body))
			req.Header.Set("X-Goog-Api-Key", h.apiKey)
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)
			checkNativeGeminiError(t, rec, tc.status, tc.state)
			if h.upstreamCalls.Load() != 0 || database.writes.Load() != 0 || database.commits.Load() != 0 || database.begins.Load() != tc.begins || database.rollbacks.Load() != tc.begins {
				t.Fatalf("rejected request activity upstream=%d writes=%d commits=%d begins=%d rollbacks=%d", h.upstreamCalls.Load(), database.writes.Load(), database.commits.Load(), database.begins.Load(), database.rollbacks.Load())
			}
		})
	}
}

func checkNativeGeminiError(t *testing.T, rec *httptest.ResponseRecorder, status int, state string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != status || body.Error.Code != status || body.Error.Status != state || body.Error.Message == "" {
		t.Fatalf("response=%d %s; want %d %s", rec.Code, rec.Body, status, state)
	}
}

func TestNativeGeminiCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		headers     func(string) http.Header
		valid       bool
	}{
		{"Google header", "", func(k string) http.Header { return http.Header{"X-Goog-Api-Key": {k}} }, true},
		{"bearer header", "", func(k string) http.Header { return http.Header{"Authorization": {"Bearer " + k}} }, true},
		{"missing", "", func(string) http.Header { return http.Header{} }, false},
		{"invalid", "", func(string) http.Header { return http.Header{"X-Goog-Api-Key": {"invalid"}} }, false},
		{"multiple sources", "", func(k string) http.Header {
			return http.Header{"X-Goog-Api-Key": {k}, "Authorization": {"Bearer " + k}}
		}, false},
		{"duplicate Google", "", func(k string) http.Header { return http.Header{"X-Goog-Api-Key": {k, k}} }, false},
		{"duplicate bearer", "", func(k string) http.Header { return http.Header{"Authorization": {"Bearer " + k, "Bearer " + k}} }, false},
		{"comma joined", "", func(k string) http.Header { return http.Header{"X-Goog-Api-Key": {k + "," + k}} }, false},
		{"whitespace", "", func(k string) http.Header { return http.Header{"X-Goog-Api-Key": {" " + k}} }, false},
		{"query credential", "?key=ignored", func(k string) http.Header { return http.Header{"X-Goog-Api-Key": {k}} }, false},
		{"empty query credential", "?key=", func(k string) http.Header { return http.Header{"Authorization": {"Bearer " + k}} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponsesWebSocketTestHarness(t)
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash-lite:generateContent"+tc.query, strings.NewReader(nativeGeminiText))
			req.Header = tc.headers(h.apiKey)
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)
			if tc.valid {
				checkNativeGeminiError(t, rec, 404, "NOT_FOUND")
			} else {
				checkNativeGeminiError(t, rec, 401, "UNAUTHENTICATED")
			}
			if h.upstreamCalls.Load() != 0 {
				t.Fatal("unexpected upstream")
			}
		})
	}
}
