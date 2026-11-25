package egress

import (
	"strings"
	"testing"
)

func TestProtocolSupport(t *testing.T) {
	for _, p := range []string{"", ProtocolSS, ProtocolSOCKS5} {
		if !SupportedProtocol(p) {
			t.Fatalf("protocol %q rejected", p)
		}
	}
	for _, p := range []string{"socks", "SOCKS5", "ftp", "socks4", " ss"} {
		if SupportedProtocol(p) {
			t.Fatalf("protocol %q accepted", p)
		}
	}
	if NormalizeProtocol("") != ProtocolSS || NormalizeProtocol(ProtocolSOCKS5) != ProtocolSOCKS5 {
		t.Fatal("protocol normalization changed")
	}
}

func TestSOCKS5Credentials(t *testing.T) {
	long := strings.Repeat("a", MaxSOCKS5CredentialBytes)
	for _, c := range [][2]string{{"", ""}, {"user", "pass"}, {long, long}, {"用户", "p a$s\\'\""}} {
		if !ValidSOCKS5Credentials(c[0], c[1]) {
			t.Fatalf("credentials %q rejected", c)
		}
	}
	for _, c := range [][2]string{
		{"user", ""}, {"", "pass"}, {long + "a", "pass"}, {"user", long + "a"},
		{"us\ner", "pass"}, {"user", "pa\rss"}, {"user", "pa\x00ss"}, {"\xff", "pass"},
	} {
		if ValidSOCKS5Credentials(c[0], c[1]) {
			t.Fatalf("credentials %q accepted", c)
		}
	}
	if !ValidSOCKS5Username("") || ValidSOCKS5Username("a\nb") || ValidSOCKS5Username(long+"a") {
		t.Fatal("username bounds changed")
	}
}
