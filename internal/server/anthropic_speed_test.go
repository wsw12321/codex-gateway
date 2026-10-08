package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
)

func TestExtractTopLevelRoutingSpeed(t *testing.T) {
	for _, test := range []struct {
		name, body, speed string
	}{
		{"default", `{"model":"claude-test","messages":[]}`, ""},
		{"standard", `{"speed":"standard","model":"claude-test"}`, "standard"},
		{"fast", `{"model":"claude-test","speed":"fast"}`, "fast"},
		{"unknown", `{"model":"claude-test","speed":"turbo"}`, "turbo"},
		{"independent service tier", `{"model":"claude-test","service_tier":"standard_only","speed":"fast"}`, "fast"},
		{"nested", `{"model":"claude-test","messages":[{"content":[{"type":"tool_result","content":{"speed":"fast","nested":{"speed":false}}}]}]}`, ""},
		{"nested with standard", `{"model":"claude-test","messages":[{"content":{"speed":"fast"}}],"speed":"standard"}`, "standard"},
		{"late", `{"model":"claude-test","messages":[{"content":"` + strings.Repeat("x", maxModelPrefix+1024) + `"}],"speed":"fast"}`, "fast"},
	} {
		t.Run(test.name, func(t *testing.T) {
			routing, err := extractTopLevelRouting([]byte(test.body))
			if err != nil || routing.Model != "claude-test" || routing.Speed != test.speed {
				t.Fatalf("routing=%+v error=%v", routing, err)
			}
		})
	}
}

func TestPrepareModelBodyIgnoresSpeedOutsideMessagesGeneration(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/messages/count_tokens"} {
		for _, fields := range []string{
			`"speed":"fast"`,
			`"speed":10`,
			`"speed":null`,
			`"speed":"standard","speed":"fast"`,
			`"Speed":{"custom":true}`,
		} {
			t.Run(path+"/"+fields, func(t *testing.T) {
				payload := `{"model":"gpt-test",` + fields + `}`
				s := &Server{}
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
				routing, body, err := s.prepareModelBody(httptest.NewRecorder(), request, 1024)
				if err != nil {
					t.Fatal(err)
				}
				defer body.Close()
				if routing.Model != "gpt-test" || routing.Speed != "" {
					t.Fatalf("speed affected routing: %+v", routing)
				}
				replayed, err := io.ReadAll(body)
				if err != nil || string(replayed) != payload {
					t.Fatalf("body=%s error=%v", replayed, err)
				}
			})
		}
	}
}

func TestAnthropicMessagesRejectsUnsupportedSpeedBeforeAdmission(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, test := range []struct {
			name, fields, wantCode string
		}{
			{"fast", `"speed":"fast"`, "service_tier_not_supported"},
			{"fast with standard tier", `"speed":"fast","service_tier":"standard_only"`, "service_tier_not_supported"},
			{"unknown", `"speed":"turbo"`, "service_tier_not_supported"},
			{"case sensitive value", `"speed":"Standard"`, "service_tier_not_supported"},
			{"late fast", `"metadata":{"padding":"` + strings.Repeat("x", maxModelPrefix+1024) + `"},"speed":"fast"`, "service_tier_not_supported"},
			{"duplicate", `"speed":"standard","speed":"fast"`, "model_required"},
			{"identical duplicate", `"speed":"standard","speed":"standard"`, "model_required"},
			{"late duplicate", `"speed":"standard","metadata":{"padding":"` + strings.Repeat("x", maxModelPrefix+1024) + `"},"speed":"fast"`, "model_required"},
			{"case sensitive key", `"Speed":"fast"`, "model_required"},
			{"escaped key", `"spe\u0065d":"fast"`, "model_required"},
			{"escaped value", `"speed":"fa\u0073t"`, "model_required"},
			{"null", `"speed":null`, "model_required"},
			{"number", `"speed":1`, "model_required"},
			{"boolean", `"speed":true`, "model_required"},
			{"object", `"speed":{"value":"fast"}`, "model_required"},
			{"array", `"speed":["fast"]`, "model_required"},
			{"empty", `"speed":""`, "model_required"},
			{"invalid syntax", `"speed":"fast/standard"`, "model_required"},
			{"too long", `"speed":"` + strings.Repeat("a", 33) + `"`, "model_required"},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				h := newResponsesWebSocketTestHarness(t)
				h.server.config.BodyLimit = 64 << 20
				var anthropicCalls atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					anthropicCalls.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer upstream.Close()
				base, err := url.Parse(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				h.server.anthropic = gatewayproxy.NewCPAAnthropic(base, "internal")
				body := fmt.Sprintf(`{"model":"claude-test","max_tokens":100,"stream":%t,"messages":[],%s}`, stream, test.fields)
				request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				request.Header.Set("X-Api-Key", h.apiKey)
				recorder := httptest.NewRecorder()
				h.handler.ServeHTTP(recorder, request)

				var response httpx.ErrorBody
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if recorder.Code != http.StatusBadRequest || response.Error.Code != test.wantCode {
					t.Fatalf("response %d %s", recorder.Code, recorder.Body)
				}
				if got := anthropicCalls.Load() + h.upstreamCalls.Load(); got != 0 {
					t.Fatalf("upstream calls=%d, want 0", got)
				}
				if queries := h.database.queriesSnapshot(); len(queries) != 1 || !strings.Contains(queries[0], "FROM api_keys k") {
					t.Fatalf("database queries=%#v, want only API key authentication and no reservation", queries)
				}
				if slots := len(h.server.spoolSlots); slots != 0 {
					t.Fatalf("request spool slots=%d, want 0", slots)
				}
			})
		}
	}
}
