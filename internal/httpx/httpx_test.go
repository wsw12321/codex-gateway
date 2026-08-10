package httpx

import (
	"net"
	"net/http/httptest"
	"testing"
)

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
