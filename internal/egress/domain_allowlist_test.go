package egress

import "testing"

func TestNormalizeAllowedDomain(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		" Proxy.Example.COM. ": "proxy.example.com",
		"*.example.com":        "*.example.com",
		"a-b.c.example.net":    "a-b.c.example.net",
	} {
		if got, ok := NormalizeAllowedDomain(raw); !ok || got != want {
			t.Errorf("NormalizeAllowedDomain(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"", "com", "*.com", "localhost", "svc.internal", "host.local", "10.0.0.1", "203.0.113.7",
		"https://example.com", "example.com:443", "example.com/path", "user@example.com",
		"**.example.com", "*example.com", "a.*.example.com", "-bad.example.com", "exa mple.com",
	} {
		if got, ok := NormalizeAllowedDomain(raw); ok {
			t.Errorf("NormalizeAllowedDomain(%q) = %q, want rejection", raw, got)
		}
	}
}

func TestDomainAllowed(t *testing.T) {
	t.Parallel()
	entries := []string{"proxy.example.com", "*.nodes.example.net"}
	for host, want := range map[string]bool{
		"proxy.example.com":       true,
		"PROXY.example.com.":      true,
		"a.proxy.example.com":     false,
		"example.com":             false,
		"jp.nodes.example.net":    true,
		"a.b.nodes.example.net":   true,
		"nodes.example.net":       false,
		"evilnodes.example.net":   false,
		"jp.nodes.example.net.cn": false,
		"localhost":               false,
		"203.0.113.7":             false,
	} {
		if got := DomainAllowed(entries, host); got != want {
			t.Errorf("DomainAllowed(%q) = %v, want %v", host, got, want)
		}
	}
	if DomainAllowed(nil, "proxy.example.com") {
		t.Error("an empty allowlist must reject every name")
	}
}

func TestIsHostName(t *testing.T) {
	t.Parallel()
	for server, want := range map[string]bool{"proxy.example.com": true, "203.0.113.7": false, "::1": false, "": false} {
		if got := IsHostName(server); got != want {
			t.Errorf("IsHostName(%q) = %v, want %v", server, got, want)
		}
	}
}
