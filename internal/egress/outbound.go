package egress

import (
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strings"

	"github.com/wsw/codex-gateway/internal/cpaprotocol"
)

const MaxOptionsBytes = 24 << 10

// These are remote adapters in the pinned Mihomo parser. Direct, DNS, Reject
// and Rematch are local routing actions. Only the gateway creates routes,
// listeners, dialers, endpoints and state directories.
var optionFields = map[string]string{
	"ssr":         "password cipher protocol protocol-param obfs obfs-param",
	"vmess":       "uuid alterId cipher tls servername network alpn client-fingerprint ws-opts grpc-opts h2-opts http-opts reality-opts packet-encoding",
	"vless":       "uuid flow encryption tls servername network alpn client-fingerprint ws-opts grpc-opts h2-opts http-opts xhttp-opts reality-opts packet-encoding",
	"trojan":      "password network sni alpn client-fingerprint ws-opts grpc-opts reality-opts",
	"snell":       "psk version reuse obfs-opts",
	"hysteria":    "up down auth auth-str obfs sni alpn protocol",
	"hysteria2":   "password up down obfs obfs-password sni alpn",
	"wireguard":   "ip ipv6 private-key public-key pre-shared-key reserved allowed-ips",
	"tuic":        "uuid password token sni alpn reduce-rtt udp-relay-mode congestion-controller",
	"shadowquic":  "username password sni alpn congestion-controller",
	"gost-relay":  "username password tls sni mux",
	"ssh":         "username password private-key private-key-passphrase host-key host-key-algorithms",
	"mieru":       "username password transport multiplexing handshake-mode",
	"anytls":      "password sni alpn client-fingerprint",
	"sudoku":      "key aead-method table-type http-mask http-mask-mode http-mask-tls http-mask-host path-root",
	"masque":      "private-key public-key ip ipv6 uri sni network",
	"trusttunnel": "username password sni alpn quic client-fingerprint",
	"openvpn":     "username password proto cipher auth data-ciphers data-cipher-fallback ca cert key tls-auth tls-crypt tls-crypt-v2 key-direction",
	"tailscale":   "auth-key hostname control-url exit-node ephemeral",
	"zerotier":    "network identity-secret",
	"easytier":    "network-name network-secret hostname ipv4 dhcp peers exit-nodes",
}

func AdvancedProtocol(protocol string) bool {
	_, ok := optionFields[protocol]
	return ok
}

func MeshProtocol(protocol string) bool {
	return protocol == "tailscale" || protocol == "zerotier" || protocol == "easytier"
}

func HTTPProtocol(protocol string) bool { return protocol == "http" || protocol == "https" }

func OptionalAuth(protocol string) bool { return protocol == ProtocolSOCKS5 || HTTPProtocol(protocol) }

func NeedsCredential(protocol, username string) bool {
	return !OptionalAuth(protocol) || username != ""
}

// ProtocolFields also supplies field hints and the protocol choices in the editor.
// Mesh adapters discover peers instead of dialing a tenant's checked public IP.
func ProtocolFields(platform bool) map[string][]string {
	fields := make(map[string][]string)
	for protocol, names := range optionFields {
		if platform || !MeshProtocol(protocol) {
			fields[protocol] = strings.Fields(names)
		}
	}
	return fields
}

func ValidServerName(s string) bool {
	if len(s) == 0 || len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func ValidEndpoint(protocol, server string, port int) bool {
	if MeshProtocol(protocol) {
		return server == "" && port == 0
	}
	if port < 1 || port > 65535 {
		return false
	}
	if ip, err := netip.ParseAddr(server); err == nil {
		return ip.Is4() && ip.String() == server
	}
	return ValidServerName(server)
}

func ValidProxyUsername(protocol, username string) bool {
	return ValidSOCKS5Username(username) && (!HTTPProtocol(protocol) || !strings.Contains(username, ":"))
}

// HTTP CONNECT uses Basic authentication. Its password is not subject to
// the one-byte length field in SOCKS5's RFC 1929 handshake.
func ValidProxyCredentials(protocol, username, password string) bool {
	if !HTTPProtocol(protocol) {
		return ValidSOCKS5Credentials(username, password)
	}
	if username == "" {
		return password == ""
	}
	return ValidProxyUsername(protocol, username) && ValidPassword("", password)
}

var errOptions = errors.New("invalid outbound options")

var nestedFields = map[string]string{
	"reality-opts": "public-key short-id",
	"ws-opts":      "path headers max-early-data early-data-header-name",
	"grpc-opts":    "grpc-service-name grpc-user-agent",
	"h2-opts":      "host path",
	"http-opts":    "method path headers",
	"xhttp-opts":   "path host mode headers",
	"obfs-opts":    "mode host",
}

// DecodeOptions permits protocol parameters only. In particular the core's
// weakly typed decoder must never see an alternate endpoint or a local path.
func DecodeOptions(protocol, raw string) (map[string]any, error) {
	fields, ok := optionFields[protocol]
	if !ok || len(raw) > MaxOptionsBytes {
		return nil, errOptions
	}
	if _, err := cpaprotocol.Object([]byte(raw)); err != nil {
		return nil, errOptions
	}
	var options map[string]any
	if json.Unmarshal([]byte(raw), &options) != nil || options == nil || !validFields(protocol, "", fields, options) {
		return nil, errOptions
	}
	return options, nil
}

func validFields(protocol, parent, fields string, options map[string]any) bool {
	allowed := strings.Fields(fields)
	for k, v := range options {
		if !slices.Contains(allowed, k) {
			return false
		}
		if children, ok := nestedFields[k]; ok {
			m, ok := v.(map[string]any)
			if !ok || !validFields(protocol, k, children, m) {
				return false
			}
			continue
		}
		if !validOption(protocol, parent, k, v) {
			return false
		}
	}
	return true
}

func optionText(v any) bool {
	s, ok := v.(string)
	return ok && !strings.ContainsAny(s, "\r\n\x00")
}

func validOption(protocol, parent, key string, v any) bool {
	switch key {
	case "headers":
		headers, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for name, value := range headers {
			if name == "" || strings.ContainsAny(name, " :\t\r\n\x00") {
				return false
			}
			if parent == "http-opts" {
				if !textList(value) {
					return false
				}
			} else if !optionText(value) {
				return false
			}
		}
		return true
	case "tls", "reuse", "reduce-rtt", "mux", "quic", "http-mask", "http-mask-tls", "ephemeral", "dhcp":
		_, ok := v.(bool)
		return ok
	case "alterId", "version", "max-early-data":
		n, ok := v.(float64)
		return ok && n >= 0 && n <= 65535 && n == float64(int(n))
	case "reserved":
		list, ok := v.([]any)
		if !ok || len(list) != 3 {
			return false
		}
		for _, x := range list {
			n, ok := x.(float64)
			if !ok || n < 0 || n > 255 || n != float64(int(n)) {
				return false
			}
		}
		return true
	case "alpn", "data-ciphers", "host-key", "host-key-algorithms", "allowed-ips", "peers", "exit-nodes":
		return textList(v)
	case "private-key":
		if protocol == "ssh" {
			s, ok := v.(string)
			return ok && strings.HasPrefix(s, "-----BEGIN ") && strings.Contains(s, "PRIVATE KEY-----")
		}
	case "ca", "cert", "key", "tls-auth", "tls-crypt", "tls-crypt-v2":
		if protocol == "openvpn" {
			_, ok := v.(string)
			return ok
		}
	case "host", "path":
		if parent == "h2-opts" && key == "host" || parent == "http-opts" && key == "path" {
			return textList(v)
		}
	}
	return optionText(v)
}

func textList(v any) bool {
	list, ok := v.([]any)
	if !ok || len(list) > 64 {
		return false
	}
	for _, value := range list {
		if !optionText(value) {
			return false
		}
	}
	return true
}
