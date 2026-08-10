package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestForwardStripsCredentialsAndParsesJSONUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer internal-secret" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("cookie leaked: %q", got)
		}
		if got := r.Header.Get("X-Forwarded-For"); got != "" {
			t.Errorf("forwarded header leaked: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","model":"gpt-test","output":[],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":5},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3}}}`)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	client := NewWithHTTPClient(base, "internal-secret", upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "https://gateway.test/v1/responses", strings.NewReader(`{"model":"gpt-test"}`))
	req.Header.Set("Authorization", "Bearer cgk_v1_public_secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	recorder := httptest.NewRecorder()
	result, failure := client.Forward(context.Background(), recorder, req, "/v1/responses")
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Model != "gpt-test" || result.Usage.Total() != 19 || result.Usage.CachedTokens != 5 || result.Usage.ReasoningTokens != 3 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestForwardSSEUsageAcrossChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-test\",\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens\":8,\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n")
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	client := NewWithHTTPClient(base, "secret", upstream.Client())
	req := httptest.NewRequest(http.MethodPost, "https://gateway.test/v1/responses", strings.NewReader("{}"))
	recorder := httptest.NewRecorder()
	result, failure := client.Forward(context.Background(), recorder, req, "/v1/responses")
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Usage.Total() != 18 || result.Model != "gpt-test" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.FirstByteAt.IsZero() || result.FirstTokenAt.IsZero() || result.FirstTokenAt.Before(result.FirstByteAt) {
		t.Fatalf("invalid stream timing: %+v", result)
	}
	if !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatal("SSE was not streamed")
	}
}

func TestUpstreamUnauthorizedIsReauthenticationError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Bearer sensitive"}}`)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	client := NewWithHTTPClient(base, "secret", upstream.Client())
	req := httptest.NewRequest(http.MethodGet, "https://gateway.test/v1/models", nil)
	_, failure := client.Forward(context.Background(), httptest.NewRecorder(), req, "/v1/models")
	if failure == nil || failure.Status != http.StatusServiceUnavailable || failure.Code != "upstream_reauthentication_required" {
		t.Fatalf("unexpected failure: %+v", failure)
	}
}

func TestOnlyKnownPaths(t *testing.T) {
	base, _ := url.Parse("http://example.invalid")
	client := NewWithHTTPClient(base, "secret", http.DefaultClient)
	req := httptest.NewRequest(http.MethodPost, "https://gateway.test/v1/chat/completions", nil)
	_, failure := client.Forward(context.Background(), httptest.NewRecorder(), req, "/v1/chat/completions")
	if failure == nil || failure.Code != "unsupported_endpoint" {
		t.Fatalf("unexpected failure: %+v", failure)
	}
}
