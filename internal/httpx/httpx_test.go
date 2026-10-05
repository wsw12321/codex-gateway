package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
