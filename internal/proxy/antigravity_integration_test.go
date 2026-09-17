package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/antigravity"
)

type bridgeHTTPExecutor struct {
	run func(context.Context, string) (antigravity.Result, *antigravity.Failure)
}

func (e bridgeHTTPExecutor) Run(ctx context.Context, prompt string) (antigravity.Result, *antigravity.Failure) {
	return e.run(ctx, prompt)
}

func (bridgeHTTPExecutor) Check(context.Context) error { return nil }

func bridgeHTTPResult() antigravity.Result {
	return antigravity.Result{Status: "SUCCESS", Response: "Hello from Antigravity", NumTurns: 1, Usage: &antigravity.Usage{
		InputTokens: 101, CacheReadTokens: 31, OutputTokens: 37, ThinkingTokens: 29, TotalTokens: 138,
	}}
}

func newBridgeHTTPClient(t *testing.T, executor bridgeHTTPExecutor) (*Client, *antigravity.Server) {
	t.Helper()
	bridge := antigravity.NewServer(executor, "independent-bridge-secret")
	bridge.Refresh(context.Background())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer independent-bridge-secret" {
			t.Errorf("HTTP bridge received incorrect authentication: %q", got)
		}
		if r.Header.Get(affinityHeader) != "" || r.Header.Get("Cookie") != "" {
			t.Error("HTTP bridge received client cookie or Codex affinity")
		}
		bridge.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	base, _ := url.Parse(upstream.URL)
	return NewAntigravity(base, "independent-bridge-secret"), bridge
}

func TestAntigravityHTTPResponsesJSONAndSSE(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var calls atomic.Int64
			bridge, _ := newBridgeHTTPClient(t, bridgeHTTPExecutor{run: func(_ context.Context, prompt string) (antigravity.Result, *antigravity.Failure) {
				calls.Add(1)
				if !strings.Contains(prompt, `"instructions":"Be concise"`) || !strings.Contains(prompt, `"role":"user","content":"private request"`) {
					t.Error("pure text instructions/messages did not reach executor")
				}
				return bridgeHTTPResult(), nil
			}})
			primary := testRouterClient(false, func(*http.Request) (*http.Response, error) {
				t.Error("Antigravity request reached Codex")
				return routerTestResponse(500, `{}`), nil
			})
			router := NewRouter(primary, bridge, map[string]string{antigravity.PublicModel: antigravity.CLIModel})
			body := fmt.Sprintf(`{"model":"%s","instructions":"Be concise","input":[{"role":"user","content":[{"type":"input_text","text":"private request"}]}],"stream":%t,"store":false,"service_tier":"default"}`, antigravity.PublicModel, stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer caller-api-key")
			request.Header.Set("Cookie", "session=private")
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardWithOptions(context.Background(), recorder, request, antigravity.PublicModel, "/v1/responses", ForwardOptions{AffinityScope: strings.Repeat("a", 43)})
			if failure != nil || result.StatusCode != http.StatusOK || result.Model != antigravity.PublicModel || result.ServiceTier != "default" || calls.Load() != 1 {
				t.Fatalf("result=%+v failure=%v calls=%d body=%s", result, failure, calls.Load(), recorder.Body)
			}
			if result.Usage != (Usage{InputTokens: 101, CachedTokens: 31, OutputTokens: 37, ReasoningTokens: 29}) || result.Usage.Total() != 138 {
				t.Fatalf("HTTP usage lost detail or double-counted thinking: %+v", result.Usage)
			}
			if result.FirstByteAt.IsZero() || result.FirstTokenAt.IsZero() || result.BytesOut != int64(recorder.Body.Len()) || strings.Contains(recorder.Body.String(), "private request") {
				t.Fatalf("invalid timing, byte accounting, or request leak: %+v", result)
			}
			if stream {
				var events []string
				for _, line := range strings.Split(recorder.Body.String(), "\n") {
					if strings.HasPrefix(line, "event: ") {
						events = append(events, strings.TrimPrefix(line, "event: "))
					}
				}
				want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
				if !reflect.DeepEqual(events, want) || !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatalf("invalid Responses SSE sequence: %v", events)
				}
			} else {
				var response struct {
					Model  string `json:"model"`
					Status string `json:"status"`
					Output []struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"output"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Model != antigravity.PublicModel || response.Status != "completed" || len(response.Output) != 1 || len(response.Output[0].Content) != 1 || response.Output[0].Content[0].Text != "Hello from Antigravity" {
					t.Fatalf("invalid Responses JSON: %s, %v", recorder.Body, err)
				}
			}
		})
	}
}

func TestAntigravityHTTPErrorsRemainTypedAndPrivate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		body      string
		failure   *antigravity.Failure
		status    int
		code      string
		wantCalls int64
	}{
		{name: "tools", body: `{"model":"gemini-3.1-pro-preview","input":"hello","tools":[]}`, status: 400, code: "antigravity_tools_unsupported"},
		{name: "image", body: `{"model":"gemini-3.1-pro-preview","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://private.invalid/image"}]}]}`, status: 400, code: "antigravity_input_unsupported"},
		{name: "duplicate", body: `{"model":"gemini-3.1-pro-preview","input":"hello","input":"private"}`, status: 400, code: "antigravity_invalid_request"},
		{name: "large", body: `{"model":"gemini-3.1-pro-preview","input":"` + strings.Repeat("x", 1<<20) + `"}`, status: 413, code: "antigravity_request_too_large"},
		{name: "authentication", failure: &antigravity.Failure{Status: 503, Code: "upstream_unavailable", Message: "private provider token"}, status: 503, code: "upstream_unavailable", wantCalls: 1},
		{name: "subscription limit", failure: &antigravity.Failure{Status: 429, Code: "upstream_rate_limited", Message: "private provider token"}, status: 429, code: "upstream_rate_limited", wantCalls: 1},
		{name: "timeout", failure: &antigravity.Failure{Status: 504, Code: "upstream_timeout", Message: "private provider token"}, status: 504, code: "upstream_timeout", wantCalls: 1},
		{name: "protocol", failure: &antigravity.Failure{Status: 502, Code: "upstream_protocol_error", Message: "private provider token"}, status: 502, code: "upstream_protocol_error", wantCalls: 1},
		{name: "process", failure: &antigravity.Failure{Status: 502, Code: "upstream_process_error", Message: "private provider token"}, status: 502, code: "upstream_process_error", wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			client, _ := newBridgeHTTPClient(t, bridgeHTTPExecutor{run: func(context.Context, string) (antigravity.Result, *antigravity.Failure) {
				calls.Add(1)
				return antigravity.Result{}, test.failure
			}})
			body := test.body
			if body == "" {
				body = `{"model":"gemini-3.1-pro-preview","input":"hello"}`
			}
			recorder := httptest.NewRecorder()
			result, failure := client.Forward(context.Background(), recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)), "/v1/responses")
			if failure == nil || failure.Status != test.status || failure.Code != test.code || strings.Contains(failure.Message, "private") || result.BytesOut != 0 || result.Usage != (Usage{}) || recorder.Body.Len() != 0 || calls.Load() != test.wantCalls {
				t.Fatalf("result=%+v failure=%+v calls=%d", result, failure, calls.Load())
			}
		})
	}
}

func TestAntigravityHTTPReadinessRemovesAndRestoresModel(t *testing.T) {
	t.Parallel()
	client, bridge := newBridgeHTTPClient(t, bridgeHTTPExecutor{run: func(context.Context, string) (antigravity.Result, *antigravity.Failure) {
		return antigravity.Result{}, &antigravity.Failure{Status: 503, Code: "upstream_unavailable", Message: "expired authentication"}
	}})
	primary := testRouterClient(false, func(*http.Request) (*http.Response, error) {
		return routerTestResponse(200, `{"object":"list","data":[{"id":"gpt-6-astra"}]}`), nil
	})
	router := NewRouter(primary, client, map[string]string{antigravity.PublicModel: antigravity.CLIModel})
	catalog := func(wantBridge bool) {
		t.Helper()
		recorder := httptest.NewRecorder()
		_, failure := router.ForwardModelsWithOptions(context.Background(), recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil), map[string]struct{}{"gpt-6-astra": {}, antigravity.PublicModel: {}}, ForwardOptions{})
		if failure != nil || strings.Contains(recorder.Body.String(), antigravity.PublicModel) != wantBridge || !strings.Contains(recorder.Body.String(), "gpt-6-astra") {
			t.Fatalf("want bridge=%t catalog=%s failure=%v", wantBridge, recorder.Body, failure)
		}
	}
	catalog(true)
	_, failure := router.ForwardWithOptions(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gemini-3.1-pro-preview","input":"hello"}`)), antigravity.PublicModel, "/v1/responses", ForwardOptions{})
	if failure == nil || failure.Status != 503 {
		t.Fatalf("failure=%v", failure)
	}
	catalog(false)
	bridge.Refresh(context.Background())
	catalog(true)
}

func TestAntigravityHTTPBusyAndCancellation(t *testing.T) {
	t.Parallel()
	started, cancelled := make(chan struct{}), make(chan struct{})
	client, _ := newBridgeHTTPClient(t, bridgeHTTPExecutor{run: func(ctx context.Context, _ string) (antigravity.Result, *antigravity.Failure) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled", Message: "cancelled"}
	}})
	request := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gemini-3.1-pro-preview","input":"hello"}`))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *Failure, 1)
	go func() {
		_, failure := client.Forward(ctx, httptest.NewRecorder(), request(), "/v1/responses")
		done <- failure
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first CLI call did not start")
	}
	_, failure := client.Forward(context.Background(), httptest.NewRecorder(), request(), "/v1/responses")
	if failure == nil || failure.Status != 429 || failure.Code != "upstream_concurrency_exceeded" || failure.RetryAfter != 1 {
		t.Fatalf("busy failure=%+v", failure)
	}
	cancel()
	select {
	case failure := <-done:
		if failure == nil || failure.Code != "client_disconnected" {
			t.Fatalf("cancel failure=%+v", failure)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not cancel")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge executor survived cancellation")
	}
}

func TestAntigravityHTTPRejectsCodexCredential(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	bridge := antigravity.NewServer(bridgeHTTPExecutor{run: func(context.Context, string) (antigravity.Result, *antigravity.Failure) {
		calls.Add(1)
		return bridgeHTTPResult(), nil
	}}, "independent-bridge-secret")
	bridge.Refresh(context.Background())
	upstream := httptest.NewServer(bridge)
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	client := NewAntigravity(base, "codex-secret")
	result, failure := client.Forward(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", io.NopCloser(strings.NewReader(`{"model":"gemini-3.1-pro-preview","input":"hello"}`))), "/v1/responses")
	if failure == nil || failure.Status != 503 || failure.Code != "upstream_reauthentication_required" || result.BytesOut != 0 || calls.Load() != 0 {
		t.Fatalf("wrong-token request: result=%+v failure=%+v calls=%d", result, failure, calls.Load())
	}
}
