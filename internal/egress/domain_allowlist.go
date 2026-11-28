package egress

import (
	"net/netip"
	"strings"
)

// MaxAllowedDomains caps the Owner's allowlist of member egress host names.
const MaxAllowedDomains = 256

// NormalizeAllowedDomain returns the canonical form of one allowlist entry:
// a public DNS name ("proxy.example.com") or a subdomain wildcard
// ("*.example.com"). Entries never carry a scheme, port or path; an entry
// applies to every port.
func NormalizeAllowedDomain(raw string) (string, bool) {
	value := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if !ValidSupplierHost(strings.TrimPrefix(value, "*.")) {
		return "", false
	}
	return value, true
}

// IsHostName reports whether server is a DNS name rather than an IP literal.
func IsHostName(server string) bool {
	_, err := netip.ParseAddr(server)
	return server != "" && err != nil
}

// DomainAllowed reports whether host matches an allowlist entry. An exact
// entry matches only itself; "*.example.com" matches every name below
// example.com but not example.com itself.
func DomainAllowed(entries []string, host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !ValidSupplierHost(host) {
		return false
	}
	for _, entry := range entries {
		if suffix, wildcard := strings.CutPrefix(entry, "*."); wildcard {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == entry {
			return true
		}
	}
	return false
}
