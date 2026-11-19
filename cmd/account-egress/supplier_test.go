package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestSupplierLookupRejectsMixedAnswers(t *testing.T) {
	_, err := supplierAddresses(t.Context(), "api.example.com", func(_ context.Context, network, domain string) ([]netip.Addr, error) {
		if network != "ip" || domain != "api.example.com." {
			t.Errorf("lookup %s %s", network, domain)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.77.0.1")}, nil
	})
	if err == nil {
		t.Fatal("private DNS answer accepted")
	}
}

func TestSupplierParentUsesConnectAuthority(t *testing.T) {
	const target = "[2606:4700::1111]:443"
	parent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != target || r.RequestURI != target {
			t.Errorf("method=%s host=%q target=%q", r.Method, r.Host, r.RequestURI)
		}
		if user, key, ok := proxyCredentials(r.Header); !ok || user != "route" || key != "ticket" {
			t.Error("missing parent authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer parent.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := supplierParent(ctx, parent.Listener.Addr().(*net.TCPAddr).String(), target, "route", "ticket")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}
