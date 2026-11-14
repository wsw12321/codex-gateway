package proxy

import (
	"context"
	"errors"
	"fmt"
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

const nativeGeminiModel = antigravityPublicModel
const nativeGeminiRequest = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"read_file","parametersJsonSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}]}`

// CPA reports the provider's model version, which is not the public model ID.
const nativeGeminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-3.8-flash","responseId":"response-1","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":7,"cachedContentTokenCount":3,"totalTokenCount":22}}`

func nativeGeminiRouter(t *testing.T, fn roundTripFunc) *Router {
	t.Helper()
	primary := testRouterClient(false, func(*http.Request) (*http.Response, error) {
		t.Error("native Gemini request reached the primary client")
		return nil, errors.New("unexpected primary request")
	})
	return NewRouter(primary, testRouterClient(true, fn), map[string]string{nativeGeminiModel: nativeGeminiModel})
}

func TestNativeGeminiCredentialsUsageAndToolRoundTrip(t *testing.T) {
	t.Parallel()
	const scope = "0123456789012345678901234567890123456789012"
	const conversationHash = "conv-0123456789abcdef0123456789abcdef"
	requestBody := `{"systemInstruction":{"parts":[{"text":"Conversation ID: 12345678-1234-5678-9012-123456789abc"}]},` + nativeGeminiRequest[1:]
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
				ledger := NewAllocationLedger()
				router := nativeGeminiRouter(t, func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == pluginPath("antigravity-accounts/capabilities") {
						if r.Header.Get("Authorization") != "Bearer "+testManagementKey {
							t.Error("capability probe without the management key")
						}
						return routerTestResponse(http.StatusOK, `{"protocol":"upstream_account_access_v1"}`), nil
					}
					if r.Header.Get(gatewayUserHeader) != "00000000-0000-0000-0000-000000000001" {
						t.Error("trusted user identity missing or spoofed")
					}
					if r.Header.Get(affinityHeader) != scope {
						t.Error("trusted affinity scope missing or spoofed")
					}
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != requestBody || r.Method != http.MethodPost || r.URL.Path != path {
						t.Errorf("forwarded request changed: %s %s %s, %v", r.Method, r.URL, body, err)
					}
					query := ""
					if stream {
						query = "alt=sse"
					}
					if r.URL.RawQuery != query || r.Header.Get("Authorization") != "Bearer antigravity-secret" {
						t.Error("wrong internal query or credential")
					}
					for key := range r.Header {
						if key != "Authorization" && key != "Content-Type" && key != "Cache-Control" && key != "Accept" && key != gatewayUserHeader && key != affinityHeader && key != dispatchHeader {
							t.Errorf("caller header crossed the CPA boundary: %s", key)
						}
					}
					reportAllocation(t, ledger, "antigravity", r, "0123456789abcdef", conversationHash)
					response := routerTestResponse(http.StatusOK, output)
					response.Header.Set("Content-Type", mediaType)
					// Attribution comes from the plugin's report alone.
					response.Header.Set(retiredAccountHeader, "fedcba9876543210")
					response.Header.Set(retiredConversationHeader, "conv-ffffffffffffffffffffffffffffffff")
					response.Header.Set("Set-Cookie", "provider=secret")
					response.Body = io.NopCloser(iotest.OneByteReader(strings.NewReader(output)))
					return response, nil
				})
				router.antigravity.UseAllocationLedger(ledger)
				request := httptest.NewRequest(http.MethodPost, path+"?key=public-key&token=public-secret&alt=sse", strings.NewReader(requestBody))
				for _, key := range []string{"Authorization", "X-Goog-Api-Key", "Cookie", affinityHeader, retiredConversationHeader, gatewayUserHeader, dispatchHeader, "X-Api-Key", "User-Agent", "Session-Id", "X-Goog-User-Project"} {
					request.Header.Set(key, "caller-secret")
				}
				recorder := httptest.NewRecorder()
				attributed, conversation := "", ""
				result, failure := router.ForwardGemini(context.Background(), recorder, request, nativeGeminiModel, path,
					ForwardOptions{
						UserID: "00000000-0000-0000-0000-000000000001", AffinityScope: scope,
						OnUpstreamAccount: func(id string) { attributed = id },
						OnConversation:    func(hash string) { conversation = hash },
					})
				if failure != nil || recorder.Body.String() != output || recorder.Header().Get("Set-Cookie") != "" {
					t.Fatalf("result=%+v failure=%v output=%s", result, failure, recorder.Body)
				}
				if result.Model != nativeGeminiModel || result.ServiceTier != "default" || result.Usage != (Usage{InputTokens: 10, CachedTokens: 3, OutputTokens: 12, ReasoningTokens: 7}) || result.Usage.Total() != 22 {
					t.Fatalf("incorrect Gemini accounting: %+v", result)
				}
				if result.BytesOut != int64(len(output)) || result.FirstByteAt.IsZero() || result.FirstTokenAt.IsZero() || result.CompletedAt.IsZero() || attributed != "0123456789abcdef" {
					t.Fatalf("missing result metadata: %+v, attribution=%q", result, attributed)
				}
				if result.ConversationHash != conversationHash || conversation != conversationHash {
					t.Fatalf("missing conversation attribution: result=%q callback=%q", result.ConversationHash, conversation)
				}
				for _, key := range []string{affinityHeader, dispatchHeader, retiredConversationHeader, retiredAccountHeader} {
					if recorder.Header().Get(key) != "" {
						t.Errorf("internal header leaked to caller: %s", key)
					}
				}
			}
		})
	}
}

func TestNativeGeminiAffinityRequiresTrustedValidScope(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"", "invalid scope", strings.Repeat("a", 42), strings.Repeat("a", 44), strings.Repeat("a", 42) + "\n"} {
		t.Run(fmt.Sprintf("scope=%q", scope), func(t *testing.T) {
			t.Parallel()
			calls := 0
			router := nativeGeminiRouter(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get(affinityHeader) != "" || r.Header.Get(retiredConversationHeader) != "" {
					t.Error("caller supplied conversation identity crossed the CPA boundary")
				}
				return routerTestResponse(http.StatusOK, nativeGeminiResponse), nil
			})
			path := "/v1beta/models/" + nativeGeminiModel + ":generateContent"
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest))
			request.Header.Set(affinityHeader, strings.Repeat("a", 43))
			request.Header.Set(retiredConversationHeader, "conv-0123456789abcdef0123456789abcdef")
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardGemini(context.Background(), recorder, request, nativeGeminiModel, path, ForwardOptions{AffinityScope: scope})
			if scope != "" {
				if failure == nil || failure.Code != "upstream_unavailable" || calls != 0 || recorder.Body.Len() != 0 {
					t.Fatalf("invalid scope reached transport: failure=%v calls=%d body=%s", failure, calls, recorder.Body)
				}
			} else if failure != nil || calls != 1 || result.ConversationHash != "" {
				t.Fatalf("untrusted identity changed result: result=%+v failure=%v calls=%d", result, failure, calls)
			}
		})
	}
}

// A response header never attributes a native Gemini request, even a
// well-formed one on a dispatch that has a ledger entry.
func TestNativeGeminiNeverAttributesFromResponseHeaders(t *testing.T) {
	t.Parallel()
	router := nativeGeminiRouter(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			return routerTestResponse(http.StatusOK, `{"protocol":"upstream_account_access_v1"}`), nil
		}
		response := routerTestResponse(http.StatusOK, nativeGeminiResponse)
		response.Header.Set(retiredAccountHeader, "0123456789abcdef")
		response.Header.Set(retiredConversationHeader, "conv-0123456789abcdef0123456789abcdef")
		return response, nil
	})
	router.antigravity.UseAllocationLedger(NewAllocationLedger())
	path := "/v1beta/models/" + nativeGeminiModel + ":generateContent"
	notified := 0
	recorder := httptest.NewRecorder()
	result, failure := router.ForwardGemini(context.Background(), recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{
		UserID:            "00000000-0000-0000-0000-000000000001",
		OnUpstreamAccount: func(string) { notified++ },
		OnConversation:    func(string) { notified++ },
	})
	if failure != nil || result.UpstreamAccountID != "" || result.ConversationHash != "" || notified != 0 ||
		recorder.Header().Get(retiredAccountHeader) != "" || recorder.Header().Get(retiredConversationHeader) != "" {
		t.Fatalf("a response header attributed the request: result=%+v notified=%d failure=%v", result, notified, failure)
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
		{"provider error", strings.Replace(nativeGeminiResponse, `"responseId"`, `"error":{"code":500},"responseId"`, 1)},
		{"second candidate", strings.Replace(nativeGeminiResponse, `"candidates":[`, `"candidates":[{"content":{"role":"model","parts":[{"text":"other"}]},"finishReason":"STOP","index":0},`, 1)},
		{"wrong candidate index", strings.Replace(nativeGeminiResponse, `"index":0`, `"index":1`, 1)},
		{"wrong role", strings.Replace(nativeGeminiResponse, `"role":"model"`, `"role":"user"`, 1)},
		{"unfinished candidate", strings.Replace(nativeGeminiResponse, `"STOP"`, `""`, 1)},
		{"unsupported finish reason", strings.Replace(nativeGeminiResponse, `"STOP"`, `"SAFETY"`, 1)},
		{"missing usage", strings.Replace(nativeGeminiResponse, `"usageMetadata":{`, `"usageMetadataMissing":{`, 1)},
		{"missing prompt count", strings.Replace(nativeGeminiResponse, `"promptTokenCount":10,`, ``, 1)},
		{"missing output count", strings.Replace(nativeGeminiResponse, `"candidatesTokenCount":5,`, ``, 1)},
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
		{"unsupported part", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"inlineData":{"mimeType":"image/png","data":"AA=="}}`, 1)},
		{"undeclared call", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"shell","args":{},"id":"x"}}`, 1)},
		{"non-object args", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"read_file","args":[],"id":"x"}}`, 1)},
		{"null args", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"read_file","args":null,"id":"x"}}`, 1)},
		{"mixed part", strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"text":"hello","functionCall":{"name":"read_file","args":{},"id":"x"}}`, 1)},
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

// A stream is relayed as it arrives, so an invalid one can only be cut off:
// it never settles usage, and once bytes are out the handler must abort.
func TestNativeGeminiSSECompletionValidation(t *testing.T) {
	t.Parallel()
	complete := "data: " + nativeGeminiResponse + "\n\n"
	for _, test := range []struct {
		name, body, want string
	}{
		{"complete", complete, complete},
		{"CRLF and comments", ": waiting\r\n\r\ndata: " + nativeGeminiResponse + "\r\n\r\n", complete},
		{"empty", "", ""},
		{"no event delimiter", "data: " + nativeGeminiResponse + "\n", ""},
		{"partial JSON", "data: {\n\n", ""},
		{"DONE", "data: [DONE]\n\n", ""},
		{"trailing DONE", complete + "data: [DONE]\n\n", ""},
		{"duplicate complete", complete + complete, ""},
		{"trailing partial", complete + "data: {", ""},
		{"unexpected event", "event: response.completed\ndata: " + nativeGeminiResponse + "\n\n", ""},
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
			if test.want != "" {
				if failure != nil || recorder.Body.String() != test.want || result.Usage.Total() != 22 || result.AbortStream {
					t.Fatalf("valid SSE rejected: %+v, %v, body=%q", result, failure, recorder.Body)
				}
				return
			}
			if failure == nil || result.Usage != (Usage{}) || result.AbortStream != (recorder.Body.Len() > 0) {
				t.Fatalf("invalid SSE accepted: %+v, %v, body=%q", result, failure, recorder.Body)
			}
			if recorder.Body.Len() == 0 && failure.Status != http.StatusBadGateway {
				t.Fatalf("unstarted stream failure = %+v", failure)
			}
		})
	}
}

func TestNativeGeminiFailuresAndRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, model, path string
		status            int
		code              string
		want              int
		wantCode          string
	}{
		{"unknown model", "other", "/v1beta/models/other:generateContent", 200, "", 404, "model_not_found"},
		{"invalid path", nativeGeminiModel, "/v1/responses", 200, "", 404, "unsupported_endpoint"},
		{"reauthenticate", nativeGeminiModel, "", 401, "anything", 503, "upstream_reauthentication_required"},
		{"forbidden", nativeGeminiModel, "", 403, "anything", 503, "upstream_reauthentication_required"},
		{"rate limited", nativeGeminiModel, "", 429, "resource_exhausted", 429, "upstream_rate_limited"},
		{"busy", nativeGeminiModel, "", 429, "upstream_concurrency_exceeded", 429, "upstream_concurrency_exceeded"},
		{"no account", nativeGeminiModel, "", 503, "gateway_allocation_unavailable", 503, "upstream_allocation_unavailable"},
		{"rejected request", nativeGeminiModel, "", 400, "invalid_argument", 400, "invalid_argument"},
		{"provider failure", nativeGeminiModel, "", 500, "internal", 502, "internal"},
		{"timeout", nativeGeminiModel, "", 504, "deadline_exceeded", 502, "deadline_exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			router := nativeGeminiRouter(t, func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				response := routerTestResponse(test.status, `{"error":{"code":"`+test.code+`","message":"Authorization: Bearer provider-secret"}}`)
				response.Header.Set("Retry-After", "120")
				return response, nil
			})
			path := test.path
			if path == "" {
				path = "/v1beta/models/" + nativeGeminiModel + ":generateContent"
			}
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardGemini(context.Background(), recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), test.model, path, ForwardOptions{})
			if failure == nil || failure.Status != test.want || failure.Code != test.wantCode || strings.Contains(failure.Message, "provider-secret") || recorder.Body.Len() != 0 || result.Usage != (Usage{}) {
				t.Fatalf("wrong failure: %+v, %v", result, failure)
			}
			if test.want == 404 && calls.Load() != 0 {
				t.Fatal("invalid native route reached CPA")
			}
			if test.want != 404 && failure.RetryAfter != 120 {
				t.Fatalf("Retry-After lost: %+v", failure)
			}
		})
	}
	for _, antigravity := range []*Client{nil, testRouterClient(true, func(*http.Request) (*http.Response, error) { return nil, errors.New("unavailable") })} {
		router := nativeGeminiRouter(t, nil)
		router.antigravity = antigravity
		path := "/v1beta/models/" + nativeGeminiModel + ":generateContent"
		_, failure := router.ForwardGemini(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{})
		if failure == nil || failure.Status != http.StatusServiceUnavailable {
			t.Fatalf("Antigravity outage failure: %+v", failure)
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
		// A reply cut short is an unavailable upstream, never a partial answer.
		{name: "trailing transport failure", contentType: "application/json", status: 200, readFailure: true, wantStatus: 503, wantCalls: 1},
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

func TestNativeGeminiCancellationAndTimeoutAbortWithoutUsage(t *testing.T) {
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
			antigravity := withManagement(NewWithHTTPClient(base, "antigravity-secret", upstream.Client()))
			antigravity.antigravity = true
			router := NewRouter(nil, antigravity, map[string]string{nativeGeminiModel: nativeGeminiModel})
			var ctx context.Context
			var cancel context.CancelFunc
			if timeout {
				ctx, cancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			type outcome struct {
				result  Result
				failure *Failure
			}
			done := make(chan outcome, 1)
			go func() {
				path := "/v1beta/models/" + nativeGeminiModel + ":streamGenerateContent"
				result, failure := router.ForwardGemini(ctx, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{})
				done <- outcome{result, failure}
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
			case got := <-done:
				wantCode, wantErr := "client_disconnected", context.Canceled
				if timeout {
					wantCode, wantErr = "upstream_timeout", context.DeadlineExceeded
				}
				// The stream never ended, so even a frame that carried a
				// finish reason and usage must not settle as a success.
				if got.failure == nil || got.failure.Code != wantCode || !errors.Is(got.failure, wantErr) || got.result.Usage != (Usage{}) {
					t.Fatalf("interrupted stream settled: %+v, %+v", got.result, got.failure)
				}
				if got.result.BytesOut > 0 && !got.result.AbortStream {
					t.Fatalf("started stream was not aborted: %+v", got.result)
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
