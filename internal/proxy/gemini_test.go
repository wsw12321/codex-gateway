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

const geminiUsageJSON = `"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":12,"output_tokens_details":{"reasoning_tokens":7},"total_tokens":22}`

func TestGeminiResponsesPreservesFunctionRoundTrip(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		name, streamJSON := "JSON", "false"
		if stream {
			name, streamJSON = "SSE", "true"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requests := []string{
				`{"model":"gemini-3.1-pro-preview","stream":` + streamJSON + `,"input":"Weather in Shanghai?","tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`,
				`{"model":"gemini-3.1-pro-preview","stream":` + streamJSON + `,"input":[{"type":"function_call","call_id":"call_weather_1","name":"weather","arguments":"{\"city\":\"Shanghai\"}"},{"type":"function_call_output","call_id":"call_weather_1","output":"Sunny, 25 C"}]}`,
			}
			responses := []string{
				`{"id":"resp-1","object":"response","model":"gemini-3.1-pro-preview","output":[{"type":"function_call","id":"fc_weather_1","call_id":"call_weather_1","name":"weather","arguments":"{\"city\":\"Shanghai\"}","status":"completed"}],` + geminiUsageJSON + `}`,
				`{"id":"resp-2","object":"response","model":"gemini-3.1-pro-preview","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Sunny, 25 C"}]}],` + geminiUsageJSON + `}`,
			}
			if stream {
				for index, response := range responses {
					responses[index] = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gemini-3.1-pro-preview\"}}\n\n" +
						"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n" +
						"data: [DONE]\n\n"
				}
			}
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(requests) {
					t.Error("unexpected extra upstream call")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost || string(body) != requests[index] {
					t.Errorf("upstream request changed: %s %s %s", r.Method, r.URL.Path, body)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = io.WriteString(w, responses[index])
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			client := NewWithHTTPClient(base, "internal-secret", upstream.Client())

			for index, body := range requests {
				request := httptest.NewRequest(http.MethodPost, "https://gateway.test/v1/responses", strings.NewReader(body))
				response := httptest.NewRecorder()
				result, failure := client.Forward(context.Background(), response, request, "/v1/responses")
				if failure != nil {
					t.Fatal(failure)
				}
				if response.Body.String() != responses[index] {
					t.Fatalf("response changed: %s", response.Body)
				}
				if result.Model != "gemini-3.1-pro-preview" || result.Usage.InputTokens != 10 || result.Usage.CachedTokens != 3 ||
					result.Usage.OutputTokens != 12 || result.Usage.ReasoningTokens != 7 || result.Usage.Total() != 22 {
					t.Fatalf("unexpected usage: %+v", result)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("upstream calls = %d, want 2", calls.Load())
			}
		})
	}
}

func TestGeminiSSEFragmentationPreservesReasoningSubset(t *testing.T) {
	t.Parallel()
	completed := "event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gemini-3.1-pro-preview\",\r\ndata: " + geminiUsageJSON + "}}\r\n\r\n"
	payload := ": heartbeat\r\n\r\nevent: response.reasoning_text.delta\r\ndata: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"thinking\"}\r\n\r\n" +
		completed + completed + "data: [DONE]\r\n\r\n"
	for _, test := range []struct {
		name string
		src  io.Reader
	}{
		{name: "split every byte", src: iotest.OneByteReader(strings.NewReader(payload))},
		{name: "coalesced events", src: strings.NewReader(payload)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var forwarded strings.Builder
			model, _, usage, firstToken, err := streamSSE(&forwarded, test.src)
			if err != nil {
				t.Fatal(err)
			}
			if forwarded.String() != payload || model != "gemini-3.1-pro-preview" || firstToken.IsZero() {
				t.Fatalf("stream metadata: model=%q firstToken=%v forwarded=%q", model, firstToken, forwarded.String())
			}
			if usage.OutputTokens != 12 || usage.ReasoningTokens != 7 || usage.CachedTokens != 3 || usage.Total() != 22 {
				t.Fatalf("reasoning or repeated completion was counted twice: %+v", usage)
			}
		})
	}
}

func TestGeminiUpstreamRateLimitDoesNotReportUsage(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Google quota exhausted"}}`)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	client := NewWithHTTPClient(base, "internal-secret", upstream.Client())
	request := httptest.NewRequest(http.MethodPost, "https://gateway.test/v1/responses", strings.NewReader(`{"model":"gemini-3.1-pro-preview","input":[]}`))
	response := httptest.NewRecorder()
	result, failure := client.Forward(context.Background(), response, request, "/v1/responses")
	if failure == nil || failure.Status != http.StatusTooManyRequests || failure.Code != "upstream_rate_limited" || failure.RetryAfter != 120 {
		t.Fatalf("failure = %+v", failure)
	}
	if result.Usage != (Usage{}) || result.BytesOut != 0 || response.Body.Len() != 0 {
		t.Fatalf("rate limit reported response usage: %+v", result)
	}
}

func TestGeminiSSEClientCancellationStopsUpstream(t *testing.T) {
	t.Parallel()
	started, stopped := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"model\":\"gemini-3.1-pro-preview\"}}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	client := NewWithHTTPClient(base, "internal-secret", upstream.Client())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "https://gateway.test/v1/responses", strings.NewReader(`{"model":"gemini-3.1-pro-preview","stream":true,"input":[]}`))
	done := make(chan *Failure, 1)
	go func() {
		_, failure := client.Forward(ctx, httptest.NewRecorder(), request, "/v1/responses")
		done <- failure
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream stream did not start")
	}
	cancel()
	select {
	case failure := <-done:
		if failure == nil || !errors.Is(failure, context.Canceled) {
			t.Fatalf("cancellation failure = %+v", failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not stop forwarding after cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request survived client cancellation")
	}
}
