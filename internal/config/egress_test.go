package config

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestEgressOptionalAndControlKeyFile(t *testing.T) {
	t.Setenv("EGRESS_CONTROL_KEY", "")
	t.Setenv("EGRESS_CONTROL_KEY_FILE", "")
	t.Setenv("EGRESS_CONTROL_URL", "")
	var cfg Config
	if err := cfg.loadEgress(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateEgress(); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/control-key"
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 32)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EGRESS_CONTROL_KEY_FILE", path)
	if err := cfg.loadEgress(); err != nil {
		t.Fatal(err)
	}
	if cfg.EgressControlKey != strings.Repeat("k", 32) || cfg.EgressControlURL.String() != "http://account-egress:17892" {
		t.Fatal("control configuration lost")
	}
	for _, raw := range []string{"file:///tmp/control", "http://user:pass@account-egress:17892", "http://account-egress/path", "http://account-egress?query=1"} {
		cfg.EgressControlURL, _ = url.Parse(raw)
		if cfg.validateEgress() == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
