package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/store"
)

// Generation controls captured from the official AGY 1.2.12 client with
// --model gemini-3.8-flash-high and --model gemini-3.8-flash-medium.
const nativeFlashHigh = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":-1}}}`
const nativeFlashMedium = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":4000}}}`

func TestNativeFlashPreservesAdmissionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, model, body, allowlist string
		noRoute, noPrice             bool
		status                       int
		state                        string
		begins                       int64
	}{
		{name: "high user permission", model: "gemini-3.8-flash-high", body: nativeFlashHigh, allowlist: `["gemini-3.8-flash-high"]`, status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "medium user permission", model: "gemini-3.8-flash-medium", body: nativeFlashMedium, allowlist: `["gemini-3.8-flash-medium"]`, status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "alias cannot grant access", model: "gemini-3.8-flash-high", body: nativeFlashHigh, allowlist: `["gemini-3.8-flash"]`, status: 403, state: "PERMISSION_DENIED"},
		{name: "medium cannot use high key", model: "gemini-3.8-flash-medium", body: nativeFlashMedium, allowlist: `["gemini-3.8-flash-high"]`, status: 403, state: "PERMISSION_DENIED"},
		{name: "high cannot use medium key", model: "gemini-3.8-flash-high", body: nativeFlashHigh, allowlist: `["gemini-3.8-flash-medium"]`, status: 403, state: "PERMISSION_DENIED"},
		{name: "canonical route required", model: "gemini-3.8-flash-medium", body: nativeFlashMedium, noRoute: true, status: 404, state: "NOT_FOUND"},
		{name: "canonical price required", model: "gemini-3.8-flash-high", body: nativeFlashHigh, noPrice: true, status: 404, state: "NOT_FOUND"},
		{name: "custom budget rejected", model: "gemini-3.8-flash-high", body: strings.Replace(nativeFlashMedium, "4000", "4001", 1), status: 400, state: "INVALID_ARGUMENT"},
		{name: "duplicate budget rejected", model: "gemini-3.8-flash-medium", body: strings.Replace(nativeFlashMedium, `"thinkingBudget":4000`, `"thinkingBudget":-1,"thinkingBudget":4000`, 1), status: 400, state: "INVALID_ARGUMENT"},
		{name: "malformed body rejected", model: "gemini-3.8-flash-high", body: `{"contents":`, status: 400, state: "INVALID_ARGUMENT"},
		{name: "body bound retained", model: "gemini-3.8-flash-high", body: strings.Repeat("x", (1<<20)+1), status: 413, state: "INVALID_ARGUMENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponsesWebSocketTestHarness(t)
			h.server.config.UsagePricing = nativeGeminiPricing(t)
			if tc.noPrice {
				delete(h.server.config.UsagePricing.Models, tc.model)
			}
			if !tc.noRoute {
				h.server.config.AntigravityModelRoutes = map[string]string{tc.model: tc.model}
			}
			database := &geminiAdmissionTestConnector{auth: h.database, keyAllowlist: tc.allowlist, model: tc.model}
			db := sql.OpenDB(database)
			t.Cleanup(func() { _ = db.Close() })
			h.server.store = store.New(db)
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", strings.NewReader(tc.body))
			req.Header.Set("X-Goog-Api-Key", h.apiKey)
			response := httptest.NewRecorder()
			h.handler.ServeHTTP(response, req)
			checkNativeGeminiError(t, response, tc.status, tc.state)
			if h.upstreamCalls.Load() != 0 || database.writes.Load() != 0 || database.commits.Load() != 0 || database.begins.Load() != tc.begins || database.rollbacks.Load() != tc.begins {
				t.Fatal("rejected Flash request crossed an admission boundary")
			}
		})
	}
}
