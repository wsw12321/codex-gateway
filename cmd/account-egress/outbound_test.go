package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
)

// The image tests set MIHOMO_TEST_BINARY to the executable shipped to users.
// With no executable the same table still exercises snapshot validation and rendering.
func TestRemoteOutboundCatalog(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString
	const uuid = "27c30fcb-6c4a-440b-a552-2252bb39c4c1"
	certServer := httptest.NewTLSServer(nil)
	certServer.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certServer.Certificate().Raw}))
	wg := make([]byte, 32)
	if _, err := rand.Read(wg); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]map[string]any{
		"ssr":         {"password": "sample", "cipher": "aes-256-cfb", "obfs": "plain", "protocol": "origin"},
		"vmess":       {"uuid": uuid, "alterId": 0, "cipher": "auto"},
		"vless":       {"uuid": uuid, "tls": true, "network": "ws", "ws-opts": map[string]any{"path": "/edge"}},
		"trojan":      {"password": "sample"},
		"snell":       {"psk": "sample"},
		"hysteria":    {"up": "10 Mbps", "down": "20 Mbps", "auth-str": "sample"},
		"hysteria2":   {"password": "sample"},
		"tuic":        {"uuid": uuid, "password": "sample"},
		"wireguard":   {"ip": "10.255.0.2", "private-key": b64(wg), "public-key": b64(wg)},
		"shadowquic":  {"username": "sample", "password": "sample"},
		"gost-relay":  {"username": "sample", "password": "sample"},
		"ssh":         {"username": "sample", "password": "sample"},
		"mieru":       {"username": "sample", "password": "sample", "transport": "TCP"},
		"anytls":      {"password": "sample"},
		"sudoku":      {"key": "sample"},
		"masque":      {"private-key": b64(private), "public-key": b64(public), "ip": "10.255.0.2"},
		"trusttunnel": {"username": "sample", "password": "sample"},
		"openvpn":     {"proto": "udp", "ca": ca, "username": "sample", "password": "sample"},
		"tailscale":   {"auth-key": "synthetic", "exit-node": "100.64.1.1", "ephemeral": true},
		"zerotier":    {"network": "0123456789abcdef"},
		"easytier":    {"network-name": "fixture", "network-secret": "sample", "peers": []string{"tcp://192.0.2.1:11010"}, "exit-nodes": []string{"10.0.0.1"}},
	}
	catalog := egress.ProtocolFields(true)
	if len(catalog) != len(fixtures) {
		t.Fatal("outbound catalog needs a fixture for each adapter")
	}
	for protocol := range catalog {
		t.Run(protocol, func(t *testing.T) {
			options, ok := fixtures[protocol]
			if !ok {
				t.Fatal("missing fixture")
			}
			raw, err := json.Marshal(options)
			if err != nil {
				t.Fatal(err)
			}
			n := egress.Node{ID: "fixture", Revision: 1, Protocol: protocol, Options: string(raw), Server: "192.0.2.1", Port: 443}
			if egress.MeshProtocol(protocol) {
				n.Server, n.Port = "", 0
			}
			if err := validateSnapshot(egress.Snapshot{Nodes: []egress.Node{n}}, "fixture"); err != nil {
				t.Fatal(err)
			}
			proxy, err := mihomoProxy("out", n)
			if err != nil || proxy["name"] != "out" || proxy["type"] != protocol {
				t.Fatal("invalid outbound")
			}
			if bin := os.Getenv("MIHOMO_TEST_BINARY"); bin != "" {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if ok, err := checkOutbound(ctx, bin, t.TempDir(), n); err != nil || !ok {
					t.Fatalf("core rejected fixture: %v", err)
				}
			}
		})
	}
}

func TestHTTPConnectTransport(t *testing.T) {
	bin := os.Getenv("MIHOMO_TEST_BINARY")
	if bin == "" {
		t.Skip("set MIHOMO_TEST_BINARY to run the real CONNECT transport test")
	}
	for _, tc := range []struct {
		protocol, serverName string
		want                 bool
	}{
		{"http", "", true}, {"https", "example.com", true}, {"https", "wrong.example", false},
	} {
		t.Run(tc.protocol+tc.serverName, func(t *testing.T) {
			dir := t.TempDir()
			proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "CONNECT" || r.Host != "target.invalid:443" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("sample:secret")) {
					http.Error(w, "unexpected proxy request", 407)
					return
				}
				if tc.protocol == "https" && (r.TLS == nil || r.TLS.ServerName != "example.com") {
					http.Error(w, "bad TLS", 400)
					return
				}
				c, b, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				_, _ = b.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
				_ = b.Flush()
				line, _ := b.ReadString('\n')
				if line == "ping\n" {
					_, _ = io.WriteString(c, "pong\n")
				}
			}))
			if tc.protocol == "https" {
				proxy.StartTLS()
			} else {
				proxy.Start()
			}
			defer proxy.Close()
			node := egress.Node{ID: "fixture", Revision: 1, Protocol: tc.protocol, Server: "127.0.0.1", Port: proxy.Listener.Addr().(*net.TCPAddr).Port, ServerName: tc.serverName, Username: "sample", Password: "secret"}
			local, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := local.Addr().(*net.TCPAddr).Port
			config, err := marshalMihomo(map[string]*nodeRuntime{"fixture": {node: node, port: port}}, "fixture")
			_ = local.Close()
			if err != nil {
				t.Fatal(err)
			}
			// Production has a fixed controller inside its own container. Reserve
			// no controller port on the test host.
			var cfg map[string]any
			if err := json.Unmarshal(config, &cfg); err != nil {
				t.Fatal(err)
			}
			delete(cfg, "external-controller")
			socket := filepath.Join(dir, "ctl.sock")
			cfg["external-controller-unix"] = socket
			config, err = json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "-d", dir, "-f", "-")
			cmd.Stdin = bytes.NewReader(config)
			if tc.protocol == "https" {
				ca := filepath.Join(dir, "test-ca.pem")
				if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}), 0600); err != nil {
					t.Fatal(err)
				}
				cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+ca)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); _ = cmd.Wait() }()
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			ctl := newController("http://localhost", "fixture")
			ctl.client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}}
			defer ctl.client.CloseIdleConnections()
			for ctl.health(ctx) != nil && ctx.Err() == nil {
				time.Sleep(20 * time.Millisecond)
			}
			// A listening socket exists before Mihomo finishes loading its rules.
			// Wait for the same synchronous apply used by the production runtime.
			if err := ctl.apply(ctx, config); err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			reader := bufio.NewReader(conn)
			_, err = fmt.Fprint(conn, "CONNECT target.invalid:443 HTTP/1.1\r\nHost: target.invalid:443\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("local tunnel unavailable: %v", err)
			}
			_, _ = io.WriteString(conn, "ping\n")
			line, readErr := reader.ReadString('\n')
			if (line == "pong\n") != tc.want {
				t.Fatalf("remote tunnel success=%v, want %v, read %q (%v)", line == "pong\n", tc.want, line, readErr)
			}
		})
	}
}
