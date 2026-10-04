package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
)

func (c *Config) loadOIDC() error {
	var err error
	if value := strings.TrimSpace(os.Getenv("OIDC_ENABLED")); value != "" {
		c.OIDCEnabled, err = strconv.ParseBool(value)
		if err != nil {
			return errors.New("OIDC_ENABLED must be a boolean")
		}
	}
	// Disabled deployments need neither an additional secret nor network access.
	if !c.OIDCEnabled {
		return nil
	}
	c.OIDCIssuer = strings.TrimSpace(os.Getenv("OIDC_ISSUER"))
	c.OIDCAuthorizationURL = strings.TrimSpace(os.Getenv("OIDC_AUTHORIZATION_URL"))
	c.OIDCClientID = strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID"))
	if c.OIDCClientSecret, err = envOrFile("OIDC_CLIENT_SECRET"); err != nil {
		return err
	}
	if c.OIDCProxyURL, err = parseURL("OIDC_PROXY_URL", os.Getenv("OIDC_PROXY_URL")); err != nil {
		return err
	}
	return nil
}

// ValidateOIDC validates only OIDC settings and never contacts the provider.
func (c Config) ValidateOIDC() error {
	if !c.OIDCEnabled {
		return nil
	}
	u, err := url.Parse(c.OIDCIssuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" ||
		u.RawPath != "" || strings.ContainsAny(u.Host+u.Path, "%\\") || u.EscapedPath() != u.Path ||
		(u.Path != "" && (path.Clean(u.Path) != u.Path || strings.HasSuffix(u.Path, "/"))) {
		return errors.New("OIDC_ISSUER must be an absolute https URL on port 443 with a clean path and no credentials, query, or fragment")
	}
	if net.ParseIP(u.Hostname()) != nil || !strings.Contains(u.Hostname(), ".") {
		return errors.New("OIDC_ISSUER must use the fixed public Auth hostname")
	}
	if c.OIDCAuthorizationURL != "" {
		a, err := url.Parse(c.OIDCAuthorizationURL)
		if err != nil || a.Scheme != "https" || !validOIDCAuthorizationHost(a.Hostname()) ||
			(a.Port() != "" && a.Port() != "443") || strings.HasSuffix(a.Host, ":") || a.User != nil || a.Opaque != "" ||
			a.RawQuery != "" || a.ForceQuery || a.Fragment != "" || a.RawFragment != "" ||
			strings.ContainsAny(c.OIDCAuthorizationURL, "?#") ||
			a.RawPath != "" || strings.ContainsAny(a.Host+a.Path, "%\\") || a.EscapedPath() != a.Path ||
			(a.Path != "" && path.Clean(a.Path) != a.Path) {
			return errors.New("OIDC_AUTHORIZATION_URL must be an absolute https URL on port 443 with a public hostname, clean path and no credentials, query, or fragment")
		}
	}
	if c.OIDCClientID == "" || len(c.OIDCClientID) > 512 ||
		c.OIDCClientSecret == "" || len(c.OIDCClientSecret) > 4096 ||
		strings.ContainsAny(c.OIDCClientID+c.OIDCClientSecret, "\r\n\x00") {
		return errors.New("OIDC_CLIENT_ID and OIDC_CLIENT_SECRET are required and must be valid client credentials")
	}
	p := c.OIDCProxyURL
	if p == nil || p.Scheme != "http" || p.User != nil || p.Opaque != "" ||
		(p.Path != "" && p.Path != "/") || p.RawPath != "" || p.RawQuery != "" || p.ForceQuery || p.Fragment != "" || p.RawFragment != "" {
		return errors.New("OIDC_PROXY_URL must be an explicit internal http proxy URL")
	}
	ip := net.ParseIP(p.Hostname())
	port, err := strconv.Atoi(p.Port())
	if ip == nil || !ip.IsPrivate() || err != nil || port < 1 || port > 65535 {
		return errors.New("OIDC_PROXY_URL must use a literal private IP address and explicit port")
	}
	if c.PublicURL == nil || c.PublicURL.Host == "" ||
		(c.PublicURL.Scheme != "https" && !(c.DevInsecure && c.PublicURL.Scheme == "http")) {
		return errors.New("OIDC requires a valid GATEWAY_PUBLIC_URL")
	}
	return nil
}

func validOIDCAuthorizationHost(host string) bool {
	if len(host) > 253 || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	// Browsers interpret numeric final labels as IPv4, including abbreviated,
	// octal and hexadecimal forms that net.ParseIP intentionally does not parse.
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last == "" || !((last[0] >= 'a' && last[0] <= 'z') || (last[0] >= 'A' && last[0] <= 'Z')) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}
