package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

const nativeGeminiModel = "gemini-3.1-pro-high"
const nativeGeminiRequest = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"read_file","parametersJsonSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}]}`
const nativeGeminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-3.1-pro-high","responseId":"response-1","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":7,"cachedContentTokenCount":3,"totalTokenCount":22}}`

func nativeGeminiRouter(t *testing.T, fn roundTripFunc) *Router {
	t.Helper()
	primary := testRouterClient(false, func(*http.Request) (*http.Response, error) {
		t.Error("native Gemini request reached primary upstream")
		return nil, errors.New("unexpected primary request")
	})
	return NewRouter(primary, testRouterClient(true, fn), map[string]string{nativeGeminiModel: nativeGeminiModel})
}

func TestNativeGeminiCredentialsUsageAndToolRoundTrip(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		name, operation, mediaType := "JSON", ":generateContent", "application/json"
		if stream {
			name, operation, mediaType = "SSE", ":streamGenerateContent", "text/event-stream"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, toolCall := range []bool{false, true} {
				output := nativeGeminiResponse
				if toolCall {
					output = strings.Replace(output, `{"text":"hello"}`, `{"functionCall":{"name":"read_file","args":{"path":"main.go"},"id":"read-1"}}`, 1)
				}
				if stream {
					output = "data: " + output + "\n\n"
				}
				path := "/v1beta/models/" + nativeGeminiModel + operation
				router := nativeGeminiRouter(t, func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/internal/upstream-accounts/capabilities" {
						return routerTestResponse(http.StatusOK, `{"protocol":"upstream_account_access_v1"}`), nil
					}
					if r.Header.Get(gatewayUserHeader) != "00000000-0000-0000-0000-000000000001" {
						t.Error("trusted user identity missing or spoofed")
					}
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != nativeGeminiRequest || r.Method != http.MethodPost || r.URL.Path != path {
						t.Errorf("forwarded request changed: %s %s %s, %v", r.Method, r.URL, body, err)
					}
					query := ""
					if stream {
						query = "alt=sse"
					}
					if r.URL.RawQuery != query || r.Header.Get("Authorization") != "Bearer bridge-secret" {
						t.Error("wrong internal query or credential")
					}
					for key := range r.Header {
						if key != "Authorization" && key != "Content-Type" && key != "Cache-Control" && key != "Accept" && key != gatewayUserHeader {
							t.Errorf("caller header crossed bridge boundary: %s", key)
						}
					}
					response := routerTestResponse(http.StatusOK, output)
					response.Header.Set("Content-Type", mediaType)
					response.Header.Set(upstreamAccountHeader, "0123456789abcdef")
					response.Header.Set("Set-Cookie", "provider=secret")
					response.Body = io.NopCloser(iotest.OneByteReader(strings.NewReader(output)))
					return response, nil
				})
				request := httptest.NewRequest(http.MethodPost, path+"?key=public-key&token=public-secret&alt=sse", strings.NewReader(nativeGeminiRequest))
				for _, key := range []string{"Authorization", "X-Goog-Api-Key", "Cookie", affinityHeader, gatewayUserHeader, "X-Api-Key", "User-Agent", "Session-Id", "X-Goog-User-Project"} {
					request.Header.Set(key, "caller-secret")
				}
				recorder := httptest.NewRecorder()
				attributed := ""
				result, failure := router.ForwardGemini(context.Background(), recorder, request, nativeGeminiModel, path,
					ForwardOptions{UserID: "00000000-0000-0000-0000-000000000001", AffinityScope: "must-not-cross", OnUpstreamAccount: func(id string) { attributed = id }})
				if failure != nil || recorder.Body.String() != output || recorder.Header().Get("Set-Cookie") != "" {
					t.Fatalf("result=%+v failure=%v output=%s", result, failure, recorder.Body)
				}
				if result.Model != nativeGeminiModel || result.Usage != (Usage{InputTokens: 10, CachedTokens: 3, OutputTokens: 12, ReasoningTokens: 7}) || result.Usage.Total() != 22 {
					t.Fatalf("incorrect Gemini accounting: %+v", result)
				}
				if result.BytesOut != int64(len(output)) || result.FirstByteAt.IsZero() || result.FirstTokenAt.IsZero() || result.CompletedAt.IsZero() || attributed != "0123456789abcdef" {
					t.Fatalf("missing result metadata: %+v, attribution=%q", result, attributed)
				}
			}
		})
	}
}

func TestNativeGeminiRejectsInvalidResponseBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
	}{
		{"invalid JSON", `{`},
		{"trailing JSON", nativeGeminiResponse + `{}`},
		{"duplicate field", strings.Replace(nativeGeminiResponse, `"index":0`, `"index":0,"index":0`, 1)},
		{"unknown response field", strings.Replace(nativeGeminiResponse, `"responseId"`, `"unsafeMetadata"`, 1)},
		{"wrong model", strings.Replace(nativeGeminiResponse, nativeGeminiModel, "gpt-6", 1)},
		{"unfinished candidate", strings.Replace(nativeGeminiResponse, `"STOP"`, `""`, 1)},
		{"missing usage", strings.Replace(nativeGeminiResponse, `"usageMetadata":{`, `"usageMetadataMissing":{`, 1)},
		{"missing prompt count", strings.Replace(nativeGeminiResponse, `"promptTokenCount":10,`, ``, 1)},
		{"missing output count", strings.Replace(nativeGeminiResponse, `"candidatesTokenCount":5,`, ``, 1)},
		{"missing thought count", strings.Replace(nativeGeminiResponse, `"thoughtsTokenCount":7,`, ``, 1)},
		{"null thought count", strings.Replace(nativeGeminiResponse, `"thoughtsTokenCount":7`, `"thoughtsTokenCount":null`, 1)},
		{"null cache count", strings.Replace(nativeGeminiResponse, `"cachedContentTokenCount":3`, `"cachedContentTokenCount":null`, 1)},
		{"null total", strings.Replace(nativeGeminiResponse, `"totalTokenCount":22`, `"totalTokenCount":null`, 1)},
		{"negative usage", strings.Replace(nativeGeminiResponse, `"promptTokenCount":10`, `"promptTokenCount":-10`, 1)},
		{"fractional usage", strings.Replace(nativeGeminiResponse, `"promptTokenCount":10`, `"promptTokenCount":10.5`, 1)},
		{"cached exceeds input", strings.Replace(nativeGeminiResponse, `"cachedContentTokenCount":3`, `"cachedContentTokenCount":11`, 1)},
		{"output overflow", strings.Replace(nativeGeminiResponse, `"candidatesTokenCount":5`, `"candidatesTokenCount":9223372036854775807`, 1)},
		{"total overflow", strings.Replace(nativeGeminiResponse, `"promptTokenCount":10`, `"promptTokenCount":9223372036854775807`, 1)},
		{"incorrect total", strings.Replace(nativeGeminiResponse, `"totalTokenCount":22`, `"totalTokenCount":29`, 1)},
		{"invalid UTF-8", strings.Replace(nativeGeminiResponse, `hello`, string([]byte{0xff}), 1)},
		{"empty text", strings.Replace(nativeGeminiResponse, `"text":"hello"`, `"text":""`, 1)},
		{"undeclared call", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"shell","args":{},"id":"x"}}`, 1)},
		{"non-object args", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"read_file","args":[],"id":"x"}}`, 1)},
		{"null args", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"read_file","args":null,"id":"x"}}`, 1)},
		{"mixed part", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"text":"hello","functionCall":{"name":"read_file","args":{},"id":"x"}}`, 1)},
		{"duplicate call ID", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"read_file","args":{},"id":"x"}},{"functionCall":{"name":"read_file","args":{},"id":"x"}}`, 1)},
		{"oversized", strings.Repeat(" ", maxGeminiResponseBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := nativeGeminiRouter(t, func(*http.Request) (*http.Response, error) { return routerTestResponse(http.StatusOK, test.body), nil })
			path := "/v1beta/models/" + nativeGeminiModel + ":generateContent"
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardGemini(context.Background(), recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{})
			if failure == nil || failure.Code != "upstream_protocol_error" || failure.Status != http.StatusBadGateway || result.Usage != (Usage{}) || recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
				t.Fatalf("invalid response escaped validation: result=%+v failure=%v body=%s", result, failure, recorder.Body)
			}
		})
	}
}

func TestNativeGeminiSSECompletionValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
		ok   bool
	}{
		{"complete", "data: " + nativeGeminiResponse + "\n\n", true},
		{"CRLF and comments", ": waiting\r\n\r\ndata: " + nativeGeminiResponse + "\r\n\r\n", true},
		{"multiline", "data: " + strings.Replace(nativeGeminiResponse, `,"modelVersion"`, ",\ndata: \"modelVersion\"", 1) + "\n\n", true},
		{"empty", "", false},
		{"no event delimiter", "data: " + nativeGeminiResponse + "\n", false},
		{"partial JSON", "data: {\n\n", false},
		{"DONE", "data: [DONE]\n\n", false},
		{"trailing DONE", "data: " + nativeGeminiResponse + "\n\ndata: [DONE]\n\n", false},
		{"duplicate complete", strings.Repeat("data: "+nativeGeminiResponse+"\n\n", 2), false},
		{"trailing partial", "data: " + nativeGeminiResponse + "\n\ndata: {", false},
		{"unexpected event", "event: response.completed\ndata: " + nativeGeminiResponse + "\n\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := nativeGeminiRouter(t, func(*http.Request) (*http.Response, error) {
				response := routerTestResponse(http.StatusOK, test.body)
				response.Header.Set("Content-Type", "text/event-stream")
				return response, nil
			})
			path := "/v1beta/models/" + nativeGeminiModel + ":streamGenerateContent"
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardGemini(context.Background(), recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{})
			if test.ok {
				if failure != nil || recorder.Body.String() != test.body || result.Usage.Total() != 22 {
					t.Fatalf("valid SSE rejected: %+v, %v", result, failure)
				}
			} else if failure == nil || failure.Status != http.StatusBadGateway || recorder.Body.Len() != 0 || result.Usage != (Usage{}) {
				t.Fatalf("invalid SSE accepted: %+v, %v", result, failure)
			}
		})
	}
}

func TestNativeGeminiBridgeFailuresAndRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, model, path string
		status, want      int
		code              string
	}{
		{"unknown model", "other", "/v1beta/models/other:generateContent", 200, 404, "model_not_found"},
		{"invalid path", nativeGeminiModel, "/v1/responses", 200, 404, "unsupported_endpoint"},
		{"unsupported parameter", nativeGeminiModel, "", 400, 400, "antigravity_parameter_unsupported"},
		{"request too large", nativeGeminiModel, "", 413, 413, "antigravity_request_too_large"},
		{"reauthenticate", nativeGeminiModel, "", 401, 503, "upstream_reauthentication_required"},
		{"rate limited", nativeGeminiModel, "", 429, 429, "upstream_rate_limited"},
		{"busy", nativeGeminiModel, "", 429, 429, "upstream_concurrency_exceeded"},
		{"unavailable", nativeGeminiModel, "", 503, 503, "upstream_unavailable"},
		{"timeout", nativeGeminiModel, "", 504, 504, "upstream_timeout"},
		{"bad gateway", nativeGeminiModel, "", 502, 502, "upstream_protocol_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			router := nativeGeminiRouter(t, func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				response := routerTestResponse(test.status, `{"error":{"code":"`+test.code+`","message":"provider-secret"}}`)
				response.Header.Set("Retry-After", "120")
				return response, nil
			})
			path := test.path
			if path == "" {
				path = "/v1beta/models/" + nativeGeminiModel + ":generateContent"
			}
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardGemini(context.Background(), recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), test.model, path, ForwardOptions{})
			if failure == nil || failure.Status != test.want || failure.Code != test.code || strings.Contains(failure.Message, "provider-secret") || recorder.Body.Len() != 0 || result.Usage != (Usage{}) {
				t.Fatalf("wrong failure: %+v, %v", result, failure)
			}
			if test.want == 404 && calls.Load() != 0 {
				t.Fatal("invalid native route reached bridge")
			}
			if test.want != 404 && failure.RetryAfter != 120 {
				t.Fatalf("Retry-After lost: %+v", failure)
			}
		})
	}
	for _, bridge := range []*Client{nil, testRouterClient(true, func(*http.Request) (*http.Response, error) { return nil, errors.New("unavailable") })} {
		router := nativeGeminiRouter(t, nil)
		router.antigravity = bridge
		path := "/v1beta/models/" + nativeGeminiModel + ":generateContent"
		_, failure := router.ForwardGemini(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{})
		if failure == nil || failure.Status != http.StatusServiceUnavailable {
			t.Fatalf("bridge outage failure: %+v", failure)
		}
	}
}

func TestNativeGeminiBoundedRequestAndResponseTransport(t *testing.T) {
	t.Parallel()
	path := "/v1beta/models/" + nativeGeminiModel + ":generateContent"
	for _, test := range []struct {
		name, request, contentType string
		status                     int
		readFailure                bool
		wantStatus, wantCalls      int
	}{
		{name: "oversized request", request: strings.Repeat("x", maxInternalResponseBodyBytes+1), wantStatus: 413},
		{name: "invalid request JSON", request: "{", wantStatus: 400},
		{name: "wrong content type", contentType: "text/html", status: 200, wantStatus: 502, wantCalls: 1},
		{name: "unexpected success status", contentType: "application/json", status: 201, wantStatus: 502, wantCalls: 1},
		{name: "empty response", contentType: "application/json", status: 204, wantStatus: 502, wantCalls: 1},
		{name: "trailing transport failure", contentType: "application/json", status: 200, readFailure: true, wantStatus: 502, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			router := nativeGeminiRouter(t, func(*http.Request) (*http.Response, error) {
				calls++
				response := routerTestResponse(test.status, nativeGeminiResponse)
				response.Header.Set("Content-Type", test.contentType)
				if test.readFailure {
					response.Body = io.NopCloser(io.MultiReader(strings.NewReader(nativeGeminiResponse), iotest.ErrReader(io.ErrUnexpectedEOF)))
				}
				return response, nil
			})
			body := test.request
			if body == "" {
				body = nativeGeminiRequest
			}
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardGemini(context.Background(), recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), nativeGeminiModel, path, ForwardOptions{})
			if failure == nil || failure.Status != test.wantStatus || calls != test.wantCalls || recorder.Body.Len() != 0 || result.Usage != (Usage{}) {
				t.Fatalf("result=%+v failure=%v calls=%d", result, failure, calls)
			}
		})
	}
}

func TestNativeGeminiCancellationAndTimeoutDoNotReleaseBufferedData(t *testing.T) {
	t.Parallel()
	for _, timeout := range []bool{false, true} {
		name := "cancel"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			started, stopped := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+nativeGeminiResponse+"\n\n")
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
				close(stopped)
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			bridge := NewWithHTTPClient(base, "bridge-secret", upstream.Client())
			bridge.antigravity = true
			router := NewRouter(nil, bridge, map[string]string{nativeGeminiModel: nativeGeminiModel})
			var ctx context.Context
			var cancel context.CancelFunc
			if timeout {
				ctx, cancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			done := make(chan *Failure, 1)
			recorder := httptest.NewRecorder()
			go func() {
				path := "/v1beta/models/" + nativeGeminiModel + ":streamGenerateContent"
				_, failure := router.ForwardGemini(ctx, recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{})
				done <- failure
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not start")
			}
			if !timeout {
				cancel()
			}
			select {
			case failure := <-done:
				wantCode, wantErr := "client_disconnected", context.Canceled
				if timeout {
					wantCode, wantErr = "upstream_timeout", context.DeadlineExceeded
				}
				if failure == nil || failure.Code != wantCode || !errors.Is(failure, wantErr) || recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
					t.Fatalf("buffered data escaped cancellation: %+v, body=%s", failure, recorder.Body)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("forwarder did not stop")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream survived cancellation")
			}
		})
	}
}
