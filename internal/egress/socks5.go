package egress

import (
	"strings"
	"unicode/utf8"
)

// Node protocols. An empty protocol is Shadowsocks, so a snapshot from a
// Gateway that predates SOCKS5 still validates.
const (
	ProtocolSS     = "ss"
	ProtocolSOCKS5 = "socks5"
)

// MaxSOCKS5CredentialBytes is the RFC 1929 limit for each of the username and
// password.
const MaxSOCKS5CredentialBytes = 255

// NormalizeProtocol maps the legacy empty protocol to Shadowsocks.
func NormalizeProtocol(protocol string) string {
	if protocol == "" {
		return ProtocolSS
	}
	return protocol
}

// SupportedProtocol reports whether both Gateway and the runtime accept protocol.
func SupportedProtocol(protocol string) bool {
	p := NormalizeProtocol(protocol)
	return p == ProtocolSS || OptionalAuth(p) || AdvancedProtocol(p)
}

// ValidSOCKS5Username bounds a SOCKS5 username. Empty means no authentication.
func ValidSOCKS5Username(username string) bool {
	return username == "" || validSOCKS5Field(username)
}

// ValidSOCKS5Credentials accepts either no authentication (both empty) or an
// RFC 1929 username and password, each nonempty, single-line UTF-8 of at most
// 255 bytes.
func ValidSOCKS5Credentials(username, password string) bool {
	if username == "" && password == "" {
		return true
	}
	return validSOCKS5Field(username) && validSOCKS5Field(password)
}

func validSOCKS5Field(v string) bool {
	return v != "" && len(v) <= MaxSOCKS5CredentialBytes && utf8.ValidString(v) && !strings.ContainsAny(v, "\x00\r\n")
}
