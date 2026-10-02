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
