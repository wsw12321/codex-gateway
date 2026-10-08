package httpx

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicErrorPreservesGatewayCode(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request = request.WithContext(context.WithValue(request.Context(), requestIDKey, "request-id"))
			recorder := httptest.NewRecorder()
			WriteError(recorder, request, http.StatusBadRequest, "invalid_request_error", "service_tier_not_supported", "Unsupported speed")
			var body struct {
				Type      string      `json:"type"`
				Error     ErrorDetail `json:"error"`
				RequestID string      `json:"request_id"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusBadRequest || body.Type != "error" || body.Error.Type != "invalid_request_error" || body.Error.Code != "service_tier_not_supported" || body.Error.Message != "Unsupported speed" || body.RequestID != "request-id" {
				t.Fatalf("response %d %s", recorder.Code, recorder.Body)
			}
			if recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("error response must not be cached")
			}
		})
	}
}

func TestSecurityHeadersAllowOnlySameOriginBrandImages(t *testing.T) {
	recorder := httptest.NewRecorder()
	SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(recorder, httptest.NewRequest("GET", "/", nil))
	directives := make(map[string]string)
	for _, directive := range strings.Split(recorder.Header().Get("Content-Security-Policy"), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{
		"default-src": "'none'", "script-src": "'self'", "style-src": "'self'",
		"img-src": "'self'", "connect-src": "'self'", "frame-ancestors": "'none'",
	} {
		if got := directives[name]; got != want {
			t.Errorf("CSP %s = %q, want %q", name, got, want)
		}
	}
}

func TestResolveClientIPDoesNotTrustSpoofedHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "https://example.test/", nil)
	req.RemoteAddr = "203.0.113.9:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	if got := resolveClientIP(req, nil); !got.Equal(net.ParseIP("203.0.113.9")) {
		t.Fatalf("got spoofed address %v", got)
	}
}

func TestResolveClientIPThroughExplicitProxy(t *testing.T) {
	_, proxy, _ := net.ParseCIDR("172.20.0.0/16")
	req := httptest.NewRequest("GET", "https://example.test/", nil)
	req.RemoteAddr = "172.20.0.4:4567"
	req.Header.Set("X-Forwarded-For", "198.51.100.2, 172.20.0.3")
	if got := resolveClientIP(req, []*net.IPNet{proxy}); !got.Equal(net.ParseIP("198.51.100.2")) {
		t.Fatalf("unexpected address %v", got)
	}
}
