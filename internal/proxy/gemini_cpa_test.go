package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCPANativeGeminiStreamUsageAndSignature(t *testing.T) {
	const model = "gemini-pro-agent"
	const scope = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	frames := []string{
		`{"responseId":"r","modelVersion":"gemini-3.1-pro-preview","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"thinking","thought":true,"thoughtSignature":"opaque"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0,"thoughtsTokenCount":2,"cachedContentTokenCount":3,"totalTokenCount":12}}`,
		`{"responseId":"r","candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"opaque-tool-signature"}]},"finishReason":"STOP","safetyRatings":[]}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"cachedContentTokenCount":3,"totalTokenCount":17},"createTime":"2026-09-30T00:00:00Z"}`,
	}
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/antigravity-accounts/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"protocol":"upstream_account_access_v1"}`)
			return
		}
		if r.URL.Path != "/v1beta/models/"+model+":streamGenerateContent" || r.URL.RawQuery != "alt=sse" {
			t.Errorf("wrong URL %s", r.URL)
		}
		if r.Header.Get("X-Provider") != "" || r.Header.Get(geminiStreamHeader) != "" || r.Header.Get(affinityHeader) != scope || r.Header.Get("Authorization") != "Bearer internal" {
			t.Error("untrusted header or missing caller scope")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(upstreamAccountHeader, "0123456789abcdef")
		for _, frame := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		}
	}))
	defer u.Close()
	base, _ := url.Parse(u.URL)
	c := NewCPAAntigravity(base, "internal")
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup"}]}]}`))
	req.Header.Set("X-Provider", "codex")
	w := httptest.NewRecorder()
	result, failure := c.forwardGemini(context.Background(), w, req, model, "/v1beta/models/"+model+":streamGenerateContent", ForwardOptions{AffinityScope: scope, UserID: "11111111-1111-4111-8111-111111111111"})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 7 || result.Usage.ReasoningTokens != 2 || result.Usage.CachedTokens != 3 {
		t.Fatalf("cumulative usage duplicated: %+v", result.Usage)
	}
	if !strings.Contains(w.Body.String(), "opaque-tool-signature") || result.UpstreamAccountID != "0123456789abcdef" {
		t.Fatal("signature or attribution lost")
	}
}

func TestCPANativeMissingUsageCannotSettleSuccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
			kind := "application/json"
			if stream {
				kind = "text/event-stream"
				body = "data: " + body + "\n\n"
			}
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body))}
			c := &Client{cpaNative: true}
			w := httptest.NewRecorder()
			result, failure := c.forwardNativeGemini(context.Background(), w, resp, "gemini-pro-agent", nil, stream, Result{})
			if failure == nil || !stream && w.Body.Len() != 0 || stream && !result.AbortStream {
				t.Fatalf("missing usage accepted: %+v, %v", result, failure)
			}
		})
	}
}

func TestCPANativeResponsesReasoningIsIncludedOnce(t *testing.T) {
	data := `{"model":"gemini-pro-agent","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":7,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2},"total_tokens":17}}`
	u, err := validateNativeResponse([]byte(data), "gemini-pro-agent", nil)
	if err != nil || u.OutputTokens != 7 || u.ReasoningTokens != 2 {
		t.Fatalf("usage %+v, %v", u, err)
	}
	for _, bad := range []string{strings.Replace(data, `"input_tokens":10,`, "", 1), strings.Replace(data, `"output_tokens":7`, `"output_tokens":-7`, 1), strings.Replace(data, `"total_tokens":17`, `"total_tokens":19`, 1)} {
		if _, err := validateNativeResponse([]byte(bad), "gemini-pro-agent", nil); err == nil {
			t.Fatal("invalid usage accepted")
		}
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + data + "}\n\ndata: [DONE]\n\n"))}
	c := &Client{cpaNative: true}
	r, f := c.forwardNativeResponses(context.Background(), httptest.NewRecorder(), resp, "gemini-pro-agent", nil, Result{})
	if f != nil || r.Usage.OutputTokens != 7 {
		t.Fatalf("native SSE result %+v %v", r, f)
	}
}

func TestCPANativeGeminiRequiresTerminalUsageAndPreservesSignatureOnly(t *testing.T) {
	initial := `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":10}}`
	signature := `{"candidates":[{"content":{"parts":[{"thoughtSignature":"opaque-final"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":15}}`
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"partial then final", "data: " + initial + "\n\ndata: " + signature + "\n\n", true},
		{"stale usage", `data: {"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}` + "\n\n" + `data: {"candidates":[{"content":{"parts":[{"text":"more"}]},"finishReason":"STOP"}]}` + "\n\n", false},
		{"null usage", "data: " + strings.Replace(signature, `"thoughtsTokenCount":3`, `"thoughtsTokenCount":null`, 1) + "\n\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(test.body))}
			w := httptest.NewRecorder()
			result, failure := (&Client{}).forwardNativeGemini(context.Background(), w, resp, "gemini-pro-agent", nil, true, Result{})
			if (failure == nil) != test.valid {
				t.Fatalf("result=%+v failure=%v", result, failure)
			}
			if test.valid && (result.Usage.OutputTokens != 5 || !strings.Contains(w.Body.String(), "opaque-final")) {
				t.Fatal("usage or signature lost")
			}
		})
	}
}

func TestCPANativeResponsesRejectsOutOfScopeOrMalformedSSE(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.fake\nevent: injected"}`,
		`{"type":"response.mcp_call.in_progress"}`,
		`{"type":"response.content_part.added","part":{"type":"audio"}}`,
		`{"type":"response.output_text.delta","delta":null}`,
		`{"type":"response.output_text.delta","delta":{"inlineData":"media"}}`,
	} {
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + event + "\n\n"))}
		w := httptest.NewRecorder()
		_, failure := (&Client{}).forwardNativeResponses(context.Background(), w, resp, "gemini-pro-agent", nil, Result{})
		if failure == nil || w.Body.Len() != 0 {
			t.Errorf("unsafe event emitted %s", event)
		}
	}
}
