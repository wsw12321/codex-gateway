package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForwardSupplierNativeProtocols(t *testing.T) {
	for _, test := range []struct {
		provider, path, model, response, media string
		input, output                          int64
	}{
		{"codex", "/v1/responses", "gpt-5.6", `{"object":"response","error":null,"usage":{"input_tokens":3,"output_tokens":2}}`, "application/json", 3, 2},
		{"codex", "/v1/responses", "gpt-5.6", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n", "text/event-stream", 3, 2},
		{"anthropic", "/v1/messages", "claude-sonnet-4-6", `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, "application/json", 3, 2},
		{"anthropic", "/v1/messages/count_tokens", "claude-sonnet-4-6", `{"input_tokens":3}`, "application/json", 0, 0},
		{"antigravity", "/v1beta/models/gemini-test:generateContent", "gemini-test", `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`, "application/json", 3, 2},
	} {
		t.Run(test.provider+test.path+test.media, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path || r.Header.Get("Cookie") != "" {
					t.Errorf("unexpected supplier request: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", test.media)
				_, _ = io.WriteString(w, test.response)
			}))
			defer remote.Close()
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, remote.URL+test.path, strings.NewReader(`{}`))
			writer := httptest.NewRecorder()
			result, failure := ForwardSupplier(t.Context(), writer, request, remote.Client(), test.provider, test.path, test.model, "abcdef0123456789", "")
			if failure != nil || result.Usage.InputTokens != test.input || result.Usage.OutputTokens != test.output || writer.Body.String() != test.response || result.UpstreamAccountID != "abcdef0123456789" {
				t.Fatalf("result=%+v failure=%+v body=%q", result, failure, writer.Body.String())
			}
		})
	}
}

func TestSupplierResponsesRejectInvalidStream(t *testing.T) {
	for _, stream := range []string{
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":-1,\"output_tokens\":2}}}\n\n",
		"data: " + strings.Repeat("x", maxSSEEventBytes) + "\n\n",
	} {
		response := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
		writer := httptest.NewRecorder()
		result, failure := forwardSupplierResponses(t.Context(), writer, response, "text/event-stream", Result{})
		if failure == nil || result.BytesOut != 0 || writer.Body.Len() != 0 {
			t.Fatalf("invalid SSE reached caller: result=%+v failure=%+v", result, failure)
		}
	}
}
