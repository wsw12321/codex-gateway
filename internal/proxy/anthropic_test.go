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
	"time"
)

const anthropicTestUsage = `{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":12,"ephemeral_1h_input_tokens":18},"output_tokens":0}`
const anthropicTestMessage = `{"type":"message","model":"claude-test","id":"msg_test","content":[{"type":"tool_use","id":"tool","name":"test","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":12,"ephemeral_1h_input_tokens":18},"output_tokens":7}}`

func TestAnthropicCumulativeUsageAndMissingTTL(t *testing.T) {
	u, err := anthropicUsage([]byte(anthropicTestUsage), Usage{})
	if err != nil || u.InputTokens != 60 || u.CacheWrite5mTokens != 12 || u.CacheWrite1hTokens != 18 || !u.CacheWriteTTLPresent {
		t.Fatalf("initial usage %+v %v", u, err)
	}
	u, err = anthropicUsage([]byte(`{"output_tokens":7}`), u)
	if err != nil || u.InputTokens != 60 || u.OutputTokens != 7 || u.Total() != 67 || !u.CacheWriteTTLPresent {
		t.Fatalf("delta %+v %v", u, err)
	}
	u, err = anthropicUsage([]byte(`{"cache_creation":null}`), u)
	if err != nil || u.CacheWriteTTLPresent || u.CacheWrite5mTokens != 0 || u.CacheWrite1hTokens != 0 || u.CacheWriteTokens != 30 {
		t.Fatalf("explicit missing TTL retained stale breakdown: %+v %v", u, err)
	}
	u, err = anthropicUsage([]byte(`{"output_tokens":9,"cache_creation_input_tokens":40}`), u)
	if err != nil || u.InputTokens != 70 || u.OutputTokens != 9 || u.CacheWriteTTLPresent || u.CacheWrite5mTokens != 0 || u.CacheWrite1hTokens != 0 {
		t.Fatalf("missing TTL %+v %v", u, err)
	}
	u, err = anthropicUsage([]byte(`{"cache_creation_input_tokens":40,"cache_creation":{"ephemeral_5m_input_tokens":12}}`), u)
	if err != nil || u.CacheWriteTokens != 40 || u.CacheWriteTTLPresent || u.CacheWrite5mTokens != 0 {
		t.Fatalf("partial TTL %+v %v", u, err)
	}
	for _, body := range []string{`{"input_tokens":-1}`, `{"input_tokens":null}`, `{"input_tokens":1,"input_tokens":2}`, `{"input_tokens":9223372036854775807,"output_tokens":1}`, `{"cache_creation_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":1}}`} {
		if _, err := anthropicUsage([]byte(body), Usage{}); err == nil {
			t.Fatalf("accepted invalid usage %s", body)
		}
	}
}

func TestAnthropicNativeRequestAndResponse(t *testing.T) {
	requestBody := `{"model":"claude-test","max_tokens":100,"stream":false,"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],"thinking":{"type":"enabled","budget_tokens":32},"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}},{"type":"thinking","thinking":"opaque","signature":"signed"}]}]}`
	for _, count := range []bool{false, true} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			path, response := "/v1/messages", anthropicTestMessage
			if count {
				path, response = "/v1/messages/count_tokens", `{"input_tokens":91}`
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/anthropic-accounts/capabilities" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"protocol":"upstream_account_access_v1","anthropic_messages":"anthropic_messages_v1"}`)
					return
				}
				data, _ := io.ReadAll(r.Body)
				if r.URL.Path != path || r.URL.RawQuery != "beta=true" || string(data) != requestBody {
					t.Error("request was reshaped")
				}
				if r.Header.Get("Authorization") != "Bearer internal" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Provider") != "" || r.Header.Get("Cookie") != "" {
					t.Error("credential/provider boundary")
				}
				if r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Anthropic-Beta") != "thinking-test" || r.Header.Get("X-Claude-Code-Parent-Session-Id") != "parent" {
					t.Error("native headers lost")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(upstreamAccountHeader, "0123456789abcdef")
				fmt.Fprint(w, response)
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			client := NewCPAAnthropic(base, "internal")
			r := httptest.NewRequest(http.MethodPost, path+"?beta=true", strings.NewReader(requestBody))
			for name, value := range map[string]string{"Authorization": "Bearer caller", "X-Api-Key": "caller", "X-Provider": "codex", "Cookie": "private", "Anthropic-Version": "2023-06-01", "Anthropic-Beta": "thinking-test", "X-Claude-Code-Parent-Session-Id": "parent"} {
				r.Header.Set(name, value)
			}
			w := httptest.NewRecorder()
			result, failure := client.ForwardMessages(context.Background(), w, r, "claude-test", path, ForwardOptions{UserID: "12345678-1234-1234-1234-123456789abc", AffinityScope: strings.Repeat("a", 43)})
			if failure != nil || w.Body.String() != response || result.UpstreamAccountID != "0123456789abcdef" {
				t.Fatalf("result %+v failure %v body %s", result, failure, w.Body)
			}
			if count && result.Usage.Total() != 0 {
				t.Fatal("count_tokens produced billed usage")
			}
			if !count && (result.Usage.Total() != 67 || !result.Usage.CacheWriteTTLPresent) {
				t.Fatalf("usage %+v", result.Usage)
			}
		})
	}
}

func TestAnthropicSSEPreservesEventsAndRejectsTruncation(t *testing.T) {
	frames := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":" + anthropicTestUsage + "}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, complete := range []bool{true, false} {
		body := frames
		if !complete {
			body = body[:strings.LastIndex(body, "event: message_stop")]
		}
		w := httptest.NewRecorder()
		result, failure := (&Client{}).forwardAnthropicResponse(context.Background(), w, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, false, Result{StatusCode: 200})
		if w.Body.String() != body {
			t.Fatal("SSE bytes or order changed")
		}
		if complete && (failure != nil || result.Usage.Total() != 67) {
			t.Fatalf("completion %+v %v", result, failure)
		}
		if !complete && (failure == nil || !result.AbortStream || result.Usage.Total() != 67) {
			t.Fatalf("truncation %+v %v", result, failure)
		}
	}
}

func TestAnthropicSSEFlushesPingBeforeCompletion(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	observed := make(chan string, 1)
	go func() {
		done <- readAnthropicEvents(reader, func(raw, data []byte) error { observed <- string(raw); return nil })
	}()
	ping := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	go func() { _, _ = io.WriteString(writer, ping) }()
	select {
	case got := <-observed:
		if got != ping {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("ping buffered until EOF")
	}
	writer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicNativeErrorsRetainRecoveryWording(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"thinking signature is bound to a different conversation"},"request_id":"req_test"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, body)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	w := httptest.NewRecorder()
	result, failure := NewCPAAnthropic(base, "internal").ForwardMessages(context.Background(), w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)), "claude-test", "/v1/messages", ForwardOptions{})
	if failure == nil || failure.Status != 0 || result.StatusCode != 400 || w.Body.String() != body {
		t.Fatalf("native error %+v %v %s", result, failure, w.Body)
	}
}

type anthropicReadFunc func([]byte) (int, error)

func (f anthropicReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestAnthropicInterruptedStreamRetainsObservedUsage(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":" + anthropicTestUsage + "}}\n\n"
	delta := "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n"
	for _, tc := range []struct {
		name, tail string
		cancelled  bool
	}{
		{name: "upstream disconnect"},
		{name: "client cancellation", cancelled: true},
		{name: "native error", tail: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Try again later\"}}\n\n"},
		{name: "invalid newer usage", tail: "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":-1}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var body io.Reader = strings.NewReader(start + delta + tc.tail)
			if tc.cancelled {
				body = io.MultiReader(body, anthropicReadFunc(func([]byte) (int, error) {
					cancel()
					return 0, context.Canceled
				}))
			}
			w := httptest.NewRecorder()
			result, failure := (&Client{}).forwardAnthropicResponse(ctx, w, &http.Response{
				StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body),
			}, false, Result{StatusCode: 200})
			if failure == nil || !result.AbortStream || failure.Status != 0 || result.Usage.InputTokens != 60 || result.Usage.OutputTokens != 7 || !result.Usage.CacheWriteTTLPresent || result.Usage.CacheWrite5mTokens != 12 || result.Usage.CacheWrite1hTokens != 18 {
				t.Fatalf("lost validated usage on interruption: %+v, %+v", result, failure)
			}
			if tc.cancelled && failure.Code != "client_disconnected" {
				t.Fatalf("cancellation classification = %s", failure.Code)
			}
			if tc.name == "native error" && w.Body.String() != start+delta+tc.tail {
				t.Fatal("native error event changed")
			}
		})
	}
}

func TestAnthropicMessageStopFinishesBeforeTransportEOF(t *testing.T) {
	frames := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":" + anthropicTestUsage + "}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	readAfterStop := false
	body := io.MultiReader(strings.NewReader(frames), anthropicReadFunc(func([]byte) (int, error) {
		readAfterStop = true
		return 0, io.ErrUnexpectedEOF
	}))
	w := httptest.NewRecorder()
	result, failure := (&Client{}).forwardAnthropicResponse(context.Background(), w, &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body),
	}, false, Result{StatusCode: 200})
	if failure != nil || readAfterStop || result.Usage.Total() != 67 || w.Body.String() != frames {
		t.Fatalf("message_stop waited for EOF: read=%v result=%+v failure=%v", readAfterStop, result, failure)
	}
}
