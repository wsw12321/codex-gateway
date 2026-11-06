package egress

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// MaxPasswordBytes bounds every SS credential before it reaches Mihomo.
const MaxPasswordBytes = 4096

// SS2022AES128GCM is the Shadowsocks 2022 method with a 16-byte BLAKE3 PSK.
const SS2022AES128GCM = "2022-blake3-aes-128-gcm"

var ciphers = [...]string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", SS2022AES128GCM}

// Ciphers returns the Shadowsocks methods accepted for account egress nodes.
func Ciphers() []string { return append([]string(nil), ciphers[:]...) }

// SupportedCipher reports whether both Gateway and the runtime accept method.
func SupportedCipher(method string) bool {
	for _, supported := range ciphers {
		if method == supported {
			return true
		}
	}
	return false
}

// IsSS2022 reports whether method uses Base64 pre-shared keys instead of a
// free-form password.
func IsSS2022(method string) bool { return method == SS2022AES128GCM }

// ValidPassword applies the common SS credential bounds and, for SS2022, the
// key format Mihomo's sing-shadowsocks2 parser requires: one or more standard
// padded Base64 keys separated by ':' (identity keys first), each exactly 16
// bytes. Rejecting a malformed key here keeps one node from making Mihomo
// refuse the whole runtime configuration.
func ValidPassword(method, password string) bool {
	if password == "" || len(password) > MaxPasswordBytes || !utf8.ValidString(password) || strings.ContainsAny(password, "\x00\r\n") {
		return false
	}
	if !IsSS2022(method) {
		return true
	}
	for _, key := range strings.Split(password, ":") {
		raw, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(raw) != 16 {
			return false
		}
	}
	return true
}
