// ss-egress-helper prepares Mihomo without putting its password in environment,
// command-line arguments, logs, or a persistent filesystem.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	secretPath = "/run/secrets/shadowsocks_password"
	configDir  = "/run/mihomo"
	maxSecret  = 4096
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Errors intentionally contain neither configuration nor credential data.
		fmt.Fprintln(os.Stderr, "ss-egress:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "healthcheck" {
		f, err := os.Open("/proc/net/tcp")
		if err != nil {
			return errors.New("cannot inspect local proxy listener")
		}
		defer f.Close()
		if listenerReady(f) {
			return nil
		}
		return errors.New("local proxy listener is not ready")
	}
	if len(args) != 0 {
		return errors.New("only the healthcheck subcommand is supported")
	}
	server, port, cipher, err := settings(os.Getenv)
	if err != nil {
		return err
	}
	password, err := readPassword(secretPath)
	if err != nil {
		return err
	}
	config, err := marshalConfig(server, port, cipher, password)
	if err != nil {
		return errors.New("cannot encode proxy configuration")
	}
	path, err := writeConfig(configDir, config)
	if err != nil {
		return err
	}
	// #nosec G204 -- Executable and arguments are fixed internal paths; no shell or credential arguments.
	if err := syscall.Exec("/usr/local/bin/mihomo", []string{"mihomo", "-d", configDir, "-f", path}, os.Environ()); err != nil {
		return errors.New("cannot start Mihomo")
	}
	return nil
}

func settings(getenv func(string) string) (string, int, string, error) {
	server := getenv("SHADOWSOCKS_SERVER")
	if !validServer(server) {
		return "", 0, "", errors.New("SHADOWSOCKS_SERVER must be a canonical IPv4 address or lowercase DNS hostname")
	}
	portText := getenv("SHADOWSOCKS_PORT")
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return "", 0, "", errors.New("SHADOWSOCKS_PORT must be a decimal port from 1 to 65535")
	}
	cipher := getenv("SHADOWSOCKS_CIPHER")
	if cipher == "" {
		cipher = "aes-128-gcm"
	}
	switch cipher {
	case "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305":
	default:
		return "", 0, "", errors.New("SHADOWSOCKS_CIPHER must be aes-128-gcm, aes-256-gcm or chacha20-ietf-poly1305")
	}
	return server, port, cipher, nil
}

func validServer(value string) bool {
	if ip, err := netip.ParseAddr(value); err == nil {
		return ip.Is4() && ip.String() == value
	}
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	for _, c := range labels[len(labels)-1] {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func readPassword(path string) (string, error) {
	// #nosec G304 -- Production passes only secretPath; metadata and symlinks are checked before reading.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("cannot open Shadowsocks password file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0640 ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return "", errors.New("Shadowsocks password must be a regular file with mode 0640")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Gid) != int64(os.Getegid()) {
		return "", errors.New("Shadowsocks password group must match GATEWAY_SECRET_GID")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSecret+1))
	if err != nil || len(raw) > maxSecret {
		return "", errors.New("Shadowsocks password must contain at most 4096 bytes")
	}
	password := string(raw)
	if strings.HasSuffix(password, "\n") {
		password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
	}
	if password == "" || !utf8.ValidString(password) || strings.ContainsAny(password, "\x00\r\n") {
		return "", errors.New("Shadowsocks password must be one nonempty UTF-8 line")
	}
	return password, nil
}

func marshalConfig(server string, port int, cipher, password string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"port":                17890,
		"bind-address":        "172.28.50.3",
		"allow-lan":           true,
		"lan-allowed-ips":     []string{"172.28.50.2/32"},
		"mode":                "rule",
		"log-level":           "warning",
		"ipv6":                false,
		"find-process-mode":   "off",
		"geo-auto-update":     false,
		"external-controller": "",
		"tun":                 map[string]bool{"enable": false},
		"dns":                 map[string]bool{"enable": false},
		"ntp":                 map[string]bool{"enable": false},
		"profile":             map[string]bool{"store-selected": false, "store-fake-ip": false},
		"proxies": []map[string]any{{
			"name": "ss-upstream", "type": "ss", "server": server,
			"port": port, "cipher": cipher, "password": password, "udp": false,
		}},
		"rules": []string{"MATCH,ss-upstream"},
	})
}

func writeConfig(directory string, config []byte) (string, error) {
	f, err := os.CreateTemp(directory, ".config-*.json")
	if err != nil {
		return "", errors.New("cannot create temporary proxy configuration")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(config); err != nil {
		_ = f.Close() // Preserve the original write error; the temporary file is removed.
		return "", errors.New("cannot write temporary proxy configuration")
	}
	if err := f.Close(); err != nil {
		return "", errors.New("cannot close temporary proxy configuration")
	}
	path := filepath.Join(directory, "config.json")
	if err := os.Rename(tmp, path); err != nil {
		return "", errors.New("cannot install temporary proxy configuration")
	}
	return path, nil
}

// Linux exports IPv4 sockets in little-endian hex. Check the exact address and
// LISTEN state, not a connect() that the intentional .2-only source ACL rejects.
// This is local readiness only and never treats B availability as health.
func listenerReady(reader io.Reader) bool {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 4 && fields[1] == "03321CAC:45E2" && fields[3] == "0A" {
			return true
		}
	}
	return false
}
