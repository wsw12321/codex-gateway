package egress

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSupplierURLAndPublicIP(t *testing.T) {
	base, ok := SupplierBaseURL("https://API.Example.com:443/compat/v1/", "v1")
	if !ok || base.String() != "https://api.example.com/compat/v1" {
		t.Fatalf("base=%v valid=%v", base, ok)
	}
	for _, raw := range []string{"http://api.example.com", "https://user@api.example.com", "https://api.example.com:444", "https://api.example.com?", "https://api.example.com/#x", "https://api.example.com/a/../b", "https://api.example.com/%2e", "https://[::1]", "https://127.0.0.1", "https://server.internal"} {
		if _, ok := SupplierBaseURL(raw, "v1beta"); ok {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, value := range []string{"127.0.0.1", "172.28.70.2", "169.254.169.254", "100.64.0.1", "198.18.0.1", "::ffff:127.0.0.1", "::1", "fe80::1", "fd00::1", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1"} {
		if PublicSupplierIP(netip.MustParseAddr(value)) {
			t.Errorf("accepted %s", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "2606:4700::1111"} {
		if !PublicSupplierIP(netip.MustParseAddr(value)) {
			t.Errorf("rejected %s", value)
		}
	}
}

func TestSupplierTicketScope(t *testing.T) {
	const account, host = "abcdef0123456789", "api.example.com"
	secret := strings.Repeat("k", 32)
	now := time.Now()
	ticket := SupplierTicket(secret, account, host, "global", now)
	if id, route, ok := VerifySupplierTicket(secret, ticket, host, now); !ok || id != account || route != "global" {
		t.Fatal("invalid signed scope")
	}
	if _, _, ok := VerifySupplierTicket(secret, ticket, "other.example.com", now); ok {
		t.Fatal("ticket accepted for another host")
	}
	if _, _, ok := VerifySupplierTicket(secret, ticket, host, now.Add(31*time.Second)); ok {
		t.Fatal("expired ticket accepted")
	}
	if _, _, ok := VerifySupplierTicket(strings.Repeat("x", 32), ticket, host, now); ok {
		t.Fatal("wrong signer accepted")
	}
}
