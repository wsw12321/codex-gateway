package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestForwardSessionHeaders(t *testing.T) {
	const scope = "0123456789012345678901234567890123456789012"
	const metadata = `{"turn_id":"Turn-CaSe/123","session_id":"metadata-session"}`
	const requestBody = `{"model":"gpt-test","input":[],"session_id":"body-session","conversation_id":"body-conversation","metadata":{"session_id":"nested-session"}}`
	const jsonResponse = `{"model":"gpt-test","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
	const sseResponse = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

	endpoints := []struct {
		name        string
		path        string
		contentType string
		response    string
	}{
		{name: "json", path: "/v1/responses", contentType: "application/json", response: jsonResponse},
		{name: "sse", path: "/v1/responses", contentType: "text/event-stream", response: sseResponse},
		{name: "compact", path: "/v1/responses/compact", contentType: "application/json", response: `{"type":"response.compaction","response":` + jsonResponse + `}`},
		{name: "models", path: "/v1/models", contentType: "application/json", response: `{"data":[{"id":"gpt-test"}]}`},
	}
	tests := []struct {
		name        string
		headers     http.Header
		want        http.Header
		scope       string
		antigravity bool
	}{
		{name: "no session header"},
		{
			name:    "hyphen",
			headers: http.Header{"Session-Id": {"Session-CaSe/1"}},
			want:    http.Header{"Session-Id": {"Session-CaSe/1"}},
		},
		{
			name:    "underscore",
			headers: http.Header{"Session_id": {"Session_CaSe/2"}},
			want:    http.Header{"Session_id": {"Session_CaSe/2"}},
		},
		{
			name:    "hyphen case insensitive",
			headers: http.Header{"sEsSiOn-iD": {"Session-CaSe/3"}},
			want:    http.Header{"Session-Id": {"Session-CaSe/3"}},
		},
		{
			name:    "underscore case insensitive",
			headers: http.Header{"sEsSiOn_iD": {"Session_CaSe/4"}},
			want:    http.Header{"Session_id": {"Session_CaSe/4"}},
		},
		{
			name:    "both spellings with gateway scope",
			headers: http.Header{"Session-Id": {"hyphen-session"}, "Session_id": {"underscore-session"}},
			want:    http.Header{"Session-Id": {"hyphen-session"}, "Session_id": {"underscore-session"}},
			scope:   scope,
		},
		{
			name: "multiple values and newline filtering",
			headers: http.Header{
				"Session-Id": {"first-hyphen", "bad\rvalue", "bad\nvalue", "bad\r\nX-Injected: true", "last-hyphen"},
				"Session_id": {"first-underscore", "bad\rvalue", "bad\nvalue", "bad\r\nX-Injected: true", "last-underscore"},
			},
			want: http.Header{"Session-Id": {"first-hyphen", "last-hyphen"}, "Session_id": {"first-underscore", "last-underscore"}},
		},
		{
			name:        "antigravity strips both spellings",
			headers:     http.Header{"sEsSiOn-iD": {"hyphen-session"}, "sEsSiOn_iD": {"underscore-session"}},
			antigravity: true,
		},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					method, body := http.MethodPost, requestBody
					if endpoint.path == "/v1/models" {
						method, body = http.MethodGet, ""
					} else if endpoint.contentType == "text/event-stream" {
						body = strings.TrimSuffix(body, "}") + `,"stream":true}`
					}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != method || r.URL.Path != endpoint.path {
							t.Errorf("upstream request = %s %s, want %s %s", r.Method, r.URL.Path, method, endpoint.path)
						}
						for _, header := range []string{"Session-Id", "Session_id"} {
							if got, want := r.Header.Values(header), test.want.Values(header); !slices.Equal(got, want) {
								t.Errorf("%s = %q, want %q", header, got, want)
							}
						}
						if got := r.Header.Get("X-Codex-Turn-Metadata"); got != metadata {
							t.Errorf("turn metadata = %q, want %q", got, metadata)
						}
						var wantScopes []string
						if test.scope != "" {
							wantScopes = []string{test.scope}
						}
						if got := r.Header.Values(affinityHeader); !slices.Equal(got, wantScopes) {
							t.Errorf("affinity scopes = %q, want %q", got, wantScopes)
						}
						if got := r.Header.Get("Authorization"); got != "Bearer internal-secret" {
							t.Errorf("authorization = %q", got)
						}
						for _, header := range []string{upstreamAccountHeader, "Cookie", "X-Parent-Session-Id", "X-Injected"} {
							if values := r.Header.Values(header); len(values) != 0 {
								t.Errorf("forbidden header %s leaked upstream: %q", header, values)
							}
						}
						gotBody, err := io.ReadAll(r.Body)
						if err != nil {
							t.Errorf("read upstream request body: %v", err)
						}
						if string(gotBody) != body {
							t.Errorf("request body = %q, want %q", gotBody, body)
						}
						w.Header().Set("Content-Type", endpoint.contentType)
						_, _ = io.WriteString(w, endpoint.response)
					}))
					defer upstream.Close()
					base, err := url.Parse(upstream.URL)
					if err != nil {
						t.Fatal(err)
					}
					client := NewWithHTTPClient(base, "internal-secret", upstream.Client())
					client.antigravity = test.antigravity
					request := httptest.NewRequest(method, "https://gateway.test"+endpoint.path, strings.NewReader(body))
					for key, values := range test.headers {
						// Preserve noncanonical key casing to exercise HTTP case-insensitive matching.
						request.Header[key] = slices.Clone(values)
					}
					request.Header.Set("X-Codex-Turn-Metadata", metadata)
					request.Header.Add("X-Codex-Turn-Metadata", "bad\r\nX-Injected: true")
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Authorization", "Bearer caller-api-key")
					request.Header.Set("Cookie", "session=caller-cookie")
					request.Header["x-codex-gateway-affinity"] = []string{strings.Repeat("z", 43)}
					request.Header["x-codex-upstream-account"] = []string{"fedcba9876543210"}
					request.Header.Set("X-Parent-Session-Id", "out-of-scope-parent")
					recorder := httptest.NewRecorder()
					options := ForwardOptions{AffinityScope: test.scope}
					var result Result
					var failure *Failure
					if endpoint.path == "/v1/models" {
						result, failure = client.ForwardModelsWithOptions(context.Background(), recorder, request, map[string]struct{}{"gpt-test": {}}, options)
					} else {
						result, failure = client.ForwardWithOptions(context.Background(), recorder, request, endpoint.path, options)
					}
					if failure != nil {
						t.Fatal(failure)
					}
					if result.StatusCode != http.StatusOK || recorder.Body.Len() == 0 {
						t.Fatalf("unexpected forwarding result: %+v, response body = %q", result, recorder.Body.String())
					}
				})
			}
		})
	}
}
