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
		_, _ = io.WriteString(w, `{"id":"r1","model":"gpt-test","service_tier":"priority","output":[],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":2},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3}}}`)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	client := NewWithHTTPClient(base, "internal-secret", upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "https://gateway.test/v1/responses", strings.NewReader(`{"model":"gpt-requested"}`))
	req.Header.Set("Authorization", "Bearer cgk_v1_public_secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	recorder := httptest.NewRecorder()
	result, failure := client.Forward(context.Background(), recorder, req, "/v1/responses")
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Model != "gpt-test" || result.ServiceTier != "priority" ||
		result.Usage.Total() != 19 || result.Usage.CachedTokens != 5 ||
		result.Usage.CacheWriteTokens != 2 || !result.Usage.CacheWriteTokensPresent ||
		result.Usage.ReasoningTokens != 3 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestForwardSSEUsageAcrossChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-test\",\"service_tier\":\"flex\",\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":4,\"cache_write_tokens\":1},\"output_tokens\":8,\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n")
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
	if result.Usage.Total() != 18 || result.Model != "gpt-test" || result.ServiceTier != "flex" ||
		result.Usage.CacheWriteTokens != 1 || !result.Usage.CacheWriteTokensPresent {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.FirstByteAt.IsZero() || result.FirstTokenAt.IsZero() || result.FirstTokenAt.Before(result.FirstByteAt) {
		t.Fatalf("invalid stream timing: %+v", result)
	}
	if !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatal("SSE was not streamed")
	}
}

func TestStreamJSONUsesNestedCompactResponseModel(t *testing.T) {
	var forwarded strings.Builder
	model, tier, usage, err := streamJSON(&forwarded, strings.NewReader(
		`{"type":"response.compaction","response":{"id":"r-compact","model":"gpt-actual-compact","service_tier":"default","usage":{"input_tokens":21,"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":3},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":4}}}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if model != "gpt-actual-compact" || tier != "default" || usage.InputTokens != 21 || usage.CachedTokens != 8 ||
		usage.CacheWriteTokens != 3 || !usage.CacheWriteTokensPresent ||
		usage.OutputTokens != 5 || usage.ReasoningTokens != 4 {
		t.Fatalf("unexpected compact metadata: model=%q tier=%q usage=%+v", model, tier, usage)
	}
	if !strings.Contains(forwarded.String(), `"gpt-actual-compact"`) {
		t.Fatal("compact response was not forwarded")
	}
}

func TestResponseParsingDistinguishesMissingAndZeroCacheWrites(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		payload  string
		wantTier string
		present  bool
	}{
		{
			name:     "missing cache-write field",
			payload:  `{"model":"gpt-test","service_tier":"default","usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":1},"output_tokens":2}}`,
			wantTier: "default",
			present:  false,
		},
		{
			name:     "explicit zero cache-write field",
			payload:  `{"model":"gpt-test","service_tier":"default","usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":1,"cache_write_tokens":0},"output_tokens":2}}`,
			wantTier: "default",
			present:  true,
		},
		{
			name:     "unknown service tier is retained",
			payload:  `{"model":"gpt-test","service_tier":"ultrafast","usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":1},"output_tokens":2}}`,
			wantTier: "ultrafast",
			present:  false,
		},
		{
			name:     "missing service tier remains missing",
			payload:  `{"model":"gpt-test","usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":1},"output_tokens":2}}`,
			wantTier: "",
			present:  false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var forwarded strings.Builder
			model, tier, usage, err := streamJSON(&forwarded, strings.NewReader(test.payload))
			if err != nil {
				t.Fatal(err)
			}
			if model != "gpt-test" || tier != test.wantTier || usage.CacheWriteTokens != 0 ||
				usage.CacheWriteTokensPresent != test.present {
				t.Fatalf("metadata: model=%q tier=%q usage=%+v", model, tier, usage)
			}
		})
	}

	for _, test := range []struct {
		name    string
		details string
		present bool
	}{
		{name: "missing SSE field", details: `"cached_tokens":1`, present: false},
		{name: "explicit zero SSE field", details: `"cached_tokens":1,"cache_write_tokens":0`, present: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := `{"type":"response.completed","response":{"model":"gpt-test","service_tier":"flex","usage":{"input_tokens":3,"input_tokens_details":{` + test.details + `},"output_tokens":2}}}`
			model, tier, usage, _, ok := parseSSEData([]byte(payload))
			if !ok || model != "gpt-test" || tier != "flex" || usage.CacheWriteTokens != 0 ||
				usage.CacheWriteTokensPresent != test.present {
				t.Fatalf("metadata: ok=%t model=%q tier=%q usage=%+v", ok, model, tier, usage)
			}
		})
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
