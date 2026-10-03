package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPasswordRemainsLiteralInPrivateJSONConfig(t *testing.T) {
	password := " space:'\"\\$`#[]{}:中文 & $(touch /tmp/never) "
	for _, suffix := range []string{"", "\n", "\r\n"} {
		t.Run("suffix="+suffix, func(t *testing.T) {
			path := writeSecret(t, []byte(password+suffix), 0640)
			got, err := readPassword(path)
			if err != nil || got != password {
				t.Fatalf("password changed or was rejected: %v", err)
			}
			config, err := marshalConfig("23.106.45.205", 24019, "aes-128-gcm", got)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Port        int              `json:"port"`
				BindAddress string           `json:"bind-address"`
				Allowed     []string         `json:"lan-allowed-ips"`
				Proxies     []map[string]any `json:"proxies"`
				Rules       []string         `json:"rules"`
				Controller  string           `json:"external-controller"`
				Tun         map[string]bool  `json:"tun"`
			}
			if err := json.Unmarshal(config, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Port != 17890 || decoded.BindAddress != "172.28.50.3" ||
				len(decoded.Allowed) != 1 || decoded.Allowed[0] != "172.28.50.2/32" ||
				decoded.Controller != "" || decoded.Tun["enable"] {
				t.Fatal("configuration permits an unexpected inbound interface")
			}
			if len(decoded.Proxies) != 1 || decoded.Proxies[0]["password"] != password ||
				decoded.Proxies[0]["type"] != "ss" || len(decoded.Rules) != 1 || decoded.Rules[0] != "MATCH,ss-upstream" {
				t.Fatal("credential or exclusive Shadowsocks route changed")
			}
			dir := t.TempDir()
			for range 2 { // Rotation replaces the old file and retains restrictive permissions.
				out, err := writeConfig(dir, config)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(out)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("config is not mode 0600: %v", err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
				t.Fatalf("unexpected temporary credential artifacts: %v", err)
			}
		})
	}
}

func TestPasswordRejectsUnsafeFileAndContent(t *testing.T) {
	for _, content := range [][]byte{nil, []byte("\n"), []byte("\r\n"), []byte("secret\nsecond"),
		[]byte("secret\n\n"), []byte("secret\r"), []byte("secret\x00"), {0xff}, []byte(strings.Repeat("x", maxSecret+1))} {
		path := writeSecret(t, content, 0640)
		if _, err := readPassword(path); err == nil {
			t.Fatal("accepted invalid password data")
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("error disclosed password data")
		}
	}
	for _, mode := range []os.FileMode{0600, 0644, 0660, 0666, 0750, 0640 | os.ModeSetuid, 0640 | os.ModeSetgid, 0640 | os.ModeSticky} {
		path := writeSecret(t, []byte("password-canary"), mode)
		if _, err := readPassword(path); err == nil {
			t.Errorf("accepted mode %04o", mode)
		}
	}
	path := writeSecret(t, []byte("password-canary"), 0640)
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPassword(link); err == nil {
		t.Fatal("accepted symlink password")
	}
	if _, err := readPassword(t.TempDir()); err == nil {
		t.Fatal("accepted directory password")
	}
}

func TestSettingsRejectInjectionAndUnsupportedCiphers(t *testing.T) {
	base := map[string]string{"SHADOWSOCKS_SERVER": "ss.example.com", "SHADOWSOCKS_PORT": "24019", "SHADOWSOCKS_CIPHER": "aes-128-gcm"}
	for key, invalid := range map[string][]string{
		"SHADOWSOCKS_SERVER": {"", "01.2.3.4", "256.2.3.4", "::1", "localhost", "host.example.123", "ss.example.com\nrules: [MATCH,DIRECT]", "$(touch /tmp/no)", "x.EXAMPLE.com", "-ss.example.com", "ss..example.com", "ss.example.com."},
		"SHADOWSOCKS_PORT":   {"", "0", "65536", "024019", "+24019", "24019\n", "24019; id"},
		"SHADOWSOCKS_CIPHER": {"none", "aes-128-cfb", "AES-128-GCM", "aes-128-gcm\n", "2022-blake3-aes-128-gcm"},
	} {
		for _, value := range invalid {
			_, _, _, err := settings(func(name string) string {
				if name == key {
					return value
				}
				return base[name]
			})
			if err == nil {
				t.Errorf("accepted invalid %s", key)
			}
		}
	}
	for _, server := range []string{"0.0.0.0", "23.106.45.205", "ss.example.com"} {
		for _, cipher := range []string{"", "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"} {
			base["SHADOWSOCKS_SERVER"], base["SHADOWSOCKS_CIPHER"] = server, cipher
			if _, _, _, err := settings(func(key string) string { return base[key] }); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestHealthRequiresExactLocalListeningSocket(t *testing.T) {
	for _, row := range []string{
		"0: 00000000:45E2 00000000:0000 0A", // Wildcard would violate isolation.
		"0: 03321CAC:45E2 00000000:0000 01", // Connected socket is not readiness.
		"0: 03321CAC:45E3 00000000:0000 0A",
		"0: 02321CAC:45E2 00000000:0000 0A",
		"",
	} {
		if listenerReady(strings.NewReader(row)) {
			t.Fatal("healthcheck accepted a different or inactive listener")
		}
	}
	if !listenerReady(strings.NewReader("header\n0: 03321CAC:45E2 00000000:0000 0A\n")) {
		t.Fatal("local listener was not ready")
	}
}
