package egress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const SupplierProxyUser = "gateway-supplier"

var supplierLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var supplierPath = regexp.MustCompile(`^(?:/[A-Za-z0-9._~-]+)*$`)
var supplierRouteID = regexp.MustCompile(`^(?:global|relay|[a-f0-9]{64})$`)
var supplierAccountID = regexp.MustCompile(`^[a-f0-9]{16}$`)

func ValidSupplierHost(host string) bool {
	labels := strings.Split(host, ".")
	if len(host) > 253 || len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !supplierLabel.MatchString(label) {
			return false
		}
	}
	suffix := labels[len(labels)-1]
	return suffix[0] >= 'a' && suffix[0] <= 'z' && suffix != "localhost" && suffix != "local" && suffix != "internal" && suffix != "onion" && suffix != "arpa"
}

func SupplierBaseURL(raw, version string) (*url.URL, bool) {
	value, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || value.Scheme != "https" || value.User != nil || value.RawQuery != "" || value.ForceQuery || strings.ContainsAny(raw, "?#%\\") {
		return nil, false
	}
	host := strings.ToLower(value.Hostname())
	if !ValidSupplierHost(host) || (value.Host != value.Hostname() && value.Host != value.Hostname()+":443") {
		return nil, false
	}
	path := strings.TrimSuffix(value.Path, "/")
	if !supplierPath.MatchString(path) {
		return nil, false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return nil, false
		}
	}
	if !strings.HasSuffix(path, "/"+version) {
		path += "/" + version
	}
	return &url.URL{Scheme: "https", Host: host, Path: path}, true
}

var nonPublicSupplierPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

var publicSupplierIPv6 = netip.MustParsePrefix("2000::/3")

func PublicSupplierIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" || (ip.Is6() && !publicSupplierIPv6.Contains(ip)) {
		return false
	}
	for _, blocked := range nonPublicSupplierPrefixes {
		if blocked.Contains(ip) {
			return false
		}
	}
	return true
}

func SupplierTicket(key, account, host, route string, now time.Time) string {
	payload := strings.Join([]string{account, host, route, strconv.FormatInt(now.Add(30*time.Second).Unix(), 10)}, "/")
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + supplierMAC(key, payload)
}

func VerifySupplierTicket(key, ticket, host string, now time.Time) (account, route string, ok bool) {
	if len(key) < 32 || len(ticket) > 768 || !ValidSupplierHost(host) {
		return "", "", false
	}
	encoded, sig, found := strings.Cut(ticket, ".")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if !found || err != nil || !hmac.Equal([]byte(sig), []byte(supplierMAC(key, string(payload)))) {
		return "", "", false
	}
	fields := strings.Split(string(payload), "/")
	if len(fields) != 4 || fields[1] != host || !supplierAccountID.MatchString(fields[0]) || !supplierRouteID.MatchString(fields[2]) {
		return "", "", false
	}
	expires, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || expires < now.Unix() || expires > now.Add(45*time.Second).Unix() {
		return "", "", false
	}
	return fields[0], fields[2], true
}

func supplierMAC(key, payload string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("gateway-supplier-connect/v1\x00" + payload))
	return hex.EncodeToString(mac.Sum(nil))
}
