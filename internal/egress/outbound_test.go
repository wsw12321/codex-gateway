package egress

import (
	"strings"
	"testing"
)

func TestOutboundParametersProtectEndpointAndProcess(t *testing.T) {
	for _, c := range []struct{ protocol, options string }{
		{"vmess", `{"server":"127.0.0.1"}`},
		{"trojan", `{"dialer-proxy":"DIRECT"}`},
		{"ssh", `{"private-key":"/run/secrets/service"}`},
		{"tailscale", `{"state-dir":"another-node"}`},
		{"wireguard", `{"peers":[{"server":"127.0.0.1"}]}`},
		{"vless", `{"ws-opts":{"headers":{"Host":"value\r\nheader"}}}`},
		{"vless", `{"ws-opts":{"path":"/one","path":"/two"}}`},
		{"vless", `{"xhttp-opts":{"download-settings":{"address":"127.0.0.1"}}}`},
		{"http", `{"skip-cert-verify":true}`},
		{"direct", `{}`},
	} {
		if _, err := DecodeOptions(c.protocol, c.options); err == nil {
			t.Errorf("accepted %s: %s", c.protocol, c.options)
		}
	}
	for _, p := range []string{"tailscale", "zerotier", "easytier"} {
		if _, ok := ProtocolFields(false)[p]; ok || !ValidEndpoint(p, "", 0) || ValidEndpoint(p, "192.0.2.1", 443) {
			t.Errorf("invalid mesh endpoint policy for %s", p)
		}
	}
}

func TestHTTPBasicCredentialLimits(t *testing.T) {
	password := strings.Repeat("p", 4096)
	if !ValidProxyCredentials("https", "user", password) || ValidProxyCredentials("https", "user", password+"p") || ValidProxyCredentials("http", "user:name", "secret") || ValidProxyCredentials("http", "", "secret") {
		t.Fatal("invalid HTTP Basic credential limits")
	}
}
