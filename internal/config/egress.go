package config

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func (c *Config) loadEgress() error {
	if raw := os.Getenv("SUPPLIERS_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("SUPPLIERS_ENABLED must be a boolean")
		}
		c.SuppliersEnabled = value
	}
	key, err := envOrFile("EGRESS_CONTROL_KEY")
	if err != nil {
		return err
	}
	c.EgressControlKey = strings.TrimSpace(key)
	c.EgressControlURL, err = url.Parse(envDefault("EGRESS_CONTROL_URL", "http://account-egress:17892"))
	return err
}
func (c Config) validateEgress() error {
	if c.SuppliersEnabled && (c.EgressControlKey == "" || len(c.APIKeyEncryptionKey) != 32) {
		return errors.New("SUPPLIERS_ENABLED requires account egress and the API key encryption secret")
	}
	if c.EgressControlKey == "" {
		return nil
	}
	if len(c.EgressControlKey) < 32 || strings.ContainsAny(c.EgressControlKey, "\r\n\t ") {
		return errors.New("EGRESS_CONTROL_KEY must contain at least 32 bytes without whitespace")
	}
	u := c.EgressControlURL
	if u == nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("EGRESS_CONTROL_URL must be an absolute HTTP origin without credentials or query")
	}
	return nil
}
