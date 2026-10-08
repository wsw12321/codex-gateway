package server

import (
	"encoding/json"
	"github.com/wsw/codex-gateway/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAnthropicUnknownQuotaRemainsEmptySnapshot(t *testing.T) {
	base, _ := url.Parse("http://sidecar.invalid")
	s := &Server{config: config.Config{SidecarURL: base, CPAManagementToken: "internal"}, cpaAdmin: &cpaAdminState{client: &http.Client{Transport: quotaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/internal/gateway-management/anthropic/accounts/0123456789abcdef/quota" {
			t.Fatal(r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"ok","quota":[],"windows":[]}`))}, nil
	})}}}
	r := httptest.NewRequest("POST", "/admin/cpa/api/anthropic/accounts/0123456789abcdef/quota", strings.NewReader(`{}`))
	r.SetPathValue("provider", "anthropic")
	r.SetPathValue("id", "0123456789abcdef")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.cpaQuota(w, r)
	var response map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 200 || string(response["windows"]) != "[]" || response["observed_at"] != nil {
		t.Fatalf("unknown quota %d %s", w.Code, w.Body)
	}
}

func TestAnthropicImportProjectsOnlyTokens(t *testing.T) {
	for _, raw := range []string{
		`{"access_token":"access","refresh_token":"refresh","type":"claude","account_uuid":"unverified","base_url":"https://evil.test","headers":{"Authorization":"private"}}`,
		`{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","subscriptionType":"max","scopes":["unverified"]},"other":"discard"}`,
	} {
		input, err := anthropicCredentialImport([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(input)
		if string(data) != `{"refresh_token":"refresh","access_token":"access"}` {
			t.Fatalf("unsafe projection %s", data)
		}
	}
	input, err := anthropicCredentialImport([]byte(`{"claudeAiOauth":{"accessToken":"access"}}`))
	if err != nil || input.AccessToken != "access" || input.RefreshToken != "" {
		t.Fatalf("access-only %+v %v", input, err)
	}
	for _, raw := range []string{`{}`, `{"access_token":null}`, `{"access_token":1}`, `{"access_token":"access","access_token":"other"}`, `{"access_token":"access","claudeAiOauth":{"accessToken":"other"}}`, `{"claudeAiOauth":{"accessToken":"access","accessToken":"other"}}`} {
		if _, err := anthropicCredentialImport([]byte(raw)); err == nil {
			t.Fatalf("accepted ambiguous import %s", raw)
		}
	}
}

func TestGatewayAnthropicCredentialAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name, path string
		headers    http.Header
		ok         bool
	}{
		{"bearer", "/v1/messages", http.Header{"Authorization": {"Bearer secret"}}, true},
		{"native", "/v1/messages/count_tokens", http.Header{"X-Api-Key": {"secret"}}, true},
		{"catalog", "/v1/models", http.Header{"X-Api-Key": {"secret"}}, true},
		{"both identical", "/v1/messages", http.Header{"X-Api-Key": {"secret"}, "Authorization": {"Bearer secret"}}, false},
		{"conflicting", "/v1/messages", http.Header{"X-Api-Key": {"secret"}, "Authorization": {"Bearer other"}}, false},
		{"duplicate", "/v1/messages", http.Header{"X-Api-Key": {"secret", "secret"}}, false},
		{"mixed casing", "/v1/messages", http.Header{"x-api-key": {"secret"}, "X-Api-Key": {"secret"}}, false},
		{"comma", "/v1/messages", http.Header{"X-Api-Key": {"secret,other"}}, false},
		{"other protocol", "/v1/responses", http.Header{"X-Api-Key": {"secret"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", test.path, nil)
			r.Header = test.headers
			_, ok := gatewayAPIKey(r)
			if ok != test.ok {
				t.Fatalf("accepted=%v", ok)
			}
		})
	}
}

func TestAnthropicOAuthHostAndGatewayError(t *testing.T) {
	state := strings.Repeat("a", 32)
	for _, host := range []string{"claude.ai", "claude.ai.evil.test", "api.anthropic.com", "claude.ai:443", "evil.test@claude.ai"} {
		if validCPAOAuthURL("anthropic", "https://"+host+"/oauth/authorize?state="+state, state) != (host == "claude.ai") {
			t.Fatal(host)
		}
	}
	h := newResponsesWebSocketTestHarness(t)
	s := h.server
	w := httptest.NewRecorder()
	s.requireAPIKey(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthenticated request admitted") })).ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", nil))
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"type":"error"`) || !strings.Contains(w.Body.String(), `"type":"authentication_error"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
