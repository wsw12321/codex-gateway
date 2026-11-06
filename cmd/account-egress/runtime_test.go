package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
)

const fixtureKey = "private-control-credential-never-returned-to-browser"

type fixtureController struct {
	configurations  [][]byte
	fail, probeFail bool
	failedPort      int
}

func (c *fixtureController) controllerSecret() string { return "loopback-controller-only" }
func (c *fixtureController) apply(_ context.Context, config []byte) error {
	c.configurations = append(c.configurations, config)
	if c.fail {
		return errors.New("sensitive underlying error")
	}
	return nil
}
func (c *fixtureController) probe(_ context.Context, port int) error {
	if c.probeFail || c.failedPort == port {
		return errors.New("listener failed")
	}
	return nil
}

func fixtureSnapshot(version int64) egress.Snapshot {
	id := "opaque-route-" + strconv.FormatInt(version, 10)
	return egress.Snapshot{Version: version, Nodes: []egress.Node{{ID: "node-one", Revision: version, Server: "ss.example.com", Port: 8388, Cipher: "aes-256-gcm", Password: " literal:'\"\\$`中文 ", UDP: true}}, Routes: []egress.Route{{ID: id, Token: egress.RouteToken(fixtureKey, id), NodeID: "node-one", NodeRevision: version}}}
}

func TestControlAuthenticationAndSecretFreeStatus(t *testing.T) {
	r := newRuntime(fixtureKey, &fixtureController{})
	server := httptest.NewServer(r.controlHandler())
	defer server.Close()
	config, _ := json.Marshal(fixtureSnapshot(1))
	for _, token := range []string{"", "Bearer invalid", "Bearer " + fixtureKey} {
		request, _ := http.NewRequest(http.MethodPut, server.URL+"/config", strings.NewReader(string(config)))
		request.Header.Set("Authorization", token)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if token != "Bearer "+fixtureKey {
			if response.StatusCode != 401 {
				t.Fatal("accepted untrusted control request")
			}
			continue
		}
		var status egress.Status
		if json.Unmarshal(body, &status) != nil || response.StatusCode != 200 || !status.Ready || status.AppliedVersion != 1 {
			t.Fatalf("control update failed: %s", body)
		}
		for _, secret := range []string{fixtureKey, fixtureSnapshot(1).Nodes[0].Password, fixtureSnapshot(1).Routes[0].Token} {
			if strings.Contains(string(body), secret) {
				t.Fatal("status exposed a credential")
			}
		}
	}
}

func TestSnapshotRevisionFencesAndListenerFailure(t *testing.T) {
	c := &fixtureController{}
	r := newRuntime(fixtureKey, c)
	if err := r.install(context.Background(), fixtureSnapshot(1)); err != nil {
		t.Fatal(err)
	}
	if err := r.install(context.Background(), fixtureSnapshot(1)); err != nil || len(c.configurations) != 1 {
		t.Fatal("same snapshot was reapplied")
	}
	stale := fixtureSnapshot(1)
	stale.Version = 0
	if r.install(context.Background(), stale) == nil {
		t.Fatal("accepted stale snapshot")
	}
	conflict := fixtureSnapshot(1)
	conflict.Nodes[0].Password = "changed"
	if r.install(context.Background(), conflict) == nil {
		t.Fatal("accepted mutation without revision")
	}
	c.probeFail = true
	if r.install(context.Background(), fixtureSnapshot(2)) == nil || r.status.Ready || r.status.AppliedVersion != 1 {
		t.Fatal("reported an unready listener as applied")
	}
	c.probeFail = false
	if err := r.install(context.Background(), fixtureSnapshot(2)); err != nil || !r.status.Ready || r.status.AppliedVersion != 2 {
		t.Fatalf("retry did not recover: %v", err)
	}
}

func TestFailedUpdateKeepsConfirmedUnchangedRoutesAndNodeStatus(t *testing.T) {
	c := &fixtureController{}
	r := newRuntime(fixtureKey, c)
	first := fixtureSnapshot(1)
	if err := r.install(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	c.failedPort = 20001
	if r.install(context.Background(), fixtureSnapshot(2)) == nil {
		t.Fatal("accepted failed listener")
	}
	if !r.status.Ready || r.status.AppliedVersion != 1 || r.status.ErrorCode != "configuration_apply_failed" {
		t.Fatal("successful rollback discarded prior readiness")
	}
	if len(r.status.RouteIDs) != 1 || r.status.RouteIDs[0] != first.Routes[0].ID || r.status.NodeRevisions["node-one"] != 1 {
		t.Fatal("rollback published unconfirmed routes or node revisions")
	}
	if _, ok := r.routes[first.Routes[0].ID]; !ok {
		t.Fatal("removed a confirmed old route during rollback")
	}
}

func TestControlCredentialRejectsSymlinksPermissionsAndOversizedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(fixtureKey+"\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if key, err := readControlKey(path); err != nil || key != fixtureKey {
		t.Fatalf("literal credential rejected: %v", err)
	}
	link := path + "-link"
	if os.Symlink(path, link) != nil {
		t.Fatal("cannot create fixture symlink")
	}
	if _, err := readControlKey(link); err == nil {
		t.Fatal("accepted a symlink credential")
	}
	if os.Chmod(path, 0644) != nil {
		t.Fatal("cannot change fixture permissions")
	}
	if _, err := readControlKey(path); err == nil {
		t.Fatal("accepted a public credential")
	}
	if os.Chmod(path, 0640) != nil {
		t.Fatal("cannot restore fixture permissions")
	}
	for _, data := range []string{"short", strings.Repeat("a", 513), fixtureKey + "\nsecond", fixtureKey + " "} {
		if os.WriteFile(path, []byte(data), 0640) != nil {
			t.Fatal("cannot write fixture credential")
		}
		if _, err := readControlKey(path); err == nil {
			t.Fatal("accepted malformed credential")
		}
	}
}

func TestMihomoConfigurationUsesOnlyNamedSSAndLoopback(t *testing.T) {
	node := fixtureSnapshot(1).Nodes[0]
	raw, err := marshalMihomo(map[string]*nodeRuntime{"one": {node: node, port: 23456}}, "controller-secret")
	if err != nil {
		t.Fatal(err)
	}
	var configuration map[string]any
	if json.Unmarshal(raw, &configuration) != nil {
		t.Fatal("invalid configuration")
	}
	listener := configuration["listeners"].([]any)[0].(map[string]any)
	proxy := configuration["proxies"].([]any)[0].(map[string]any)
	if listener["listen"] != "127.0.0.1" || listener["port"] != "23456" || listener["proxy"] != proxy["name"] || proxy["password"] != node.Password || proxy["udp"] != true {
		t.Fatal("changed isolated listener or literal SS settings")
	}
	if configuration["port"] != float64(0) || configuration["socks-port"] != float64(0) || configuration["external-controller"] != controllerAddress || configuration["rules"].([]any)[0] != "MATCH,REJECT" {
		t.Fatal("permitted a default proxy or unexpected inbound")
	}
}

func TestCONNECTRejectsUnexpectedSourcesDestinationsAndCredentials(t *testing.T) {
	r := newRuntime(fixtureKey, &fixtureController{})
	r.squidIP = "127.0.0.1"
	if err := r.install(context.Background(), fixtureSnapshot(1)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		source, target, credential string
		want                       int
	}{
		{"192.0.2.1:5000", "chatgpt.com:443", "", 403},
		{"127.0.0.1:5000", "evil.invalid:443", "", 403},
		{"127.0.0.1:5000", "chatgpt.com:80", "", 403},
		{"127.0.0.1:5000", "127.0.0.1:443", "", 403},
		{"127.0.0.1:5000", "chatgpt.com:443", "", 407},
		{"127.0.0.1:5000", "chatgpt.com:443", "opaque-route-1:bad", 502},
		{"127.0.0.1:5000", "chatgpt.com:443", "unknown:bad", 502},
	} {
		request := httptest.NewRequest(http.MethodConnect, "http://"+test.target, nil)
		request.Host, request.RequestURI = test.target, test.target
		request.RemoteAddr = test.source
		if test.credential != "" {
			request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(test.credential)))
		}
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("target %q returned %d, want %d", test.target, response.Code, test.want)
		}
	}
}

func echoProxy(t *testing.T) (int, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections sync.WaitGroup
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer connection.Close()
				reader := bufio.NewReader(connection)
				request, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				if request.Header.Get("Proxy-Authorization") != "" {
					return
				}
				_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
				_, _ = io.Copy(connection, reader)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, func() { listener.Close(); connections.Wait() }
}

func openTunnel(t *testing.T, address string, route egress.Route) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(route.ID + ":" + route.Token))
	fmt.Fprintf(connection, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	return connection, reader, response.StatusCode
}

func TestClientDisconnectReleasesTunnelWhenPeerRemainsIdle(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	releasePeer := make(chan struct{})
	defer close(releasePeer)
	peerSawEOF := make(chan struct{}, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		if _, err := http.ReadRequest(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_, _ = io.Copy(io.Discard, reader)
		peerSawEOF <- struct{}{}
		// Deliberately keep the peer's write side open after EOF. A client
		// cancellation must release its runtime reference without peer traffic.
		<-releasePeer
	}()
	r := newRuntime(fixtureKey, &fixtureController{})
	r.squidIP = "127.0.0.1"
	snapshot := fixtureSnapshot(1)
	if err := r.install(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	node := r.nodes[nodeKey("node-one", 1)]
	node.port = listener.Addr().(*net.TCPAddr).Port
	server := httptest.NewServer(r)
	defer server.Close()
	connection, _, status := openTunnel(t, strings.TrimPrefix(server.URL, "http://"), snapshot.Routes[0])
	if status != 200 {
		connection.Close()
		t.Fatalf("CONNECT returned %d", status)
	}
	connection.Close()
	select {
	case <-peerSawEOF:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not reach its peer")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		active := node.active
		r.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("client cancellation retained an idle SS tunnel")
}

func TestConfigurationUpdateKeepsActiveTunnelAndRejectsOldRoutes(t *testing.T) {
	port, closeProxy := echoProxy(t)
	defer closeProxy()
	r := newRuntime(fixtureKey, &fixtureController{})
	r.squidIP = "127.0.0.1"
	first := fixtureSnapshot(1)
	if err := r.install(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	r.nodes[nodeKey("node-one", 1)].port = port
	server := httptest.NewServer(r)
	defer server.Close()
	connection, reader, status := openTunnel(t, strings.TrimPrefix(server.URL, "http://"), first.Routes[0])
	defer connection.Close()
	if status != 200 {
		t.Fatalf("CONNECT returned %d", status)
	}
	ping := func() {
		t.Helper()
		if _, err := io.WriteString(connection, "ping"); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, 4)
		if _, err := io.ReadFull(reader, reply); err != nil || string(reply) != "ping" {
			t.Fatalf("stream failed: %v", err)
		}
	}
	ping()
	if err := r.install(context.Background(), fixtureSnapshot(2)); err != nil {
		t.Fatal(err)
	}
	ping()
	if r.nodes[nodeKey("node-one", 1)] == nil {
		t.Fatal("removed revision in use by an active stream")
	}
	old, _, status := openTunnel(t, strings.TrimPrefix(server.URL, "http://"), first.Routes[0])
	old.Close()
	if status != 502 {
		t.Fatal("accepted an obsolete route")
	}
	connection.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		active := r.nodes[nodeKey("node-one", 1)].active
		r.mu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.prune(context.Background())
	if r.nodes[nodeKey("node-one", 1)] != nil {
		t.Fatal("did not drain unused revision")
	}
}

const ss2022Key = "AAECAwQFBgcICQoLDA0ODw=="

func TestSS2022NodesRequireBase64KeysBeforeMihomo(t *testing.T) {
	c := &fixtureController{}
	r := newRuntime(fixtureKey, c)
	snapshot := fixtureSnapshot(1)
	snapshot.Nodes[0].Cipher = egress.SS2022AES128GCM
	snapshot.Nodes[0].Password = ss2022Key + ":EBESExQVFhcYGRobHB0eHw=="
	if err := r.install(context.Background(), snapshot); err != nil {
		t.Fatalf("valid SS2022 node rejected: %v", err)
	}
	var configuration struct {
		Proxies []map[string]any `json:"proxies"`
	}
	if json.Unmarshal(c.configurations[len(c.configurations)-1], &configuration) != nil || configuration.Proxies[0]["cipher"] != egress.SS2022AES128GCM || configuration.Proxies[0]["password"] != snapshot.Nodes[0].Password {
		t.Fatal("SS2022 cipher or key was not passed to Mihomo literally")
	}
	applied := len(c.configurations)
	for _, password := range []string{"plain password", "AAECAwQFBgcICQoLDA0ODw", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", ss2022Key + ":"} {
		bad := fixtureSnapshot(2)
		bad.Nodes[0].Cipher = egress.SS2022AES128GCM
		bad.Nodes[0].Password = password
		if r.install(context.Background(), bad) == nil {
			t.Errorf("accepted SS2022 key %q", password)
		}
	}
	if len(c.configurations) != applied || r.status.AppliedVersion != 1 || !r.status.Ready {
		t.Fatal("malformed SS2022 key reached Mihomo or disturbed the applied configuration")
	}
	for _, cipher := range []string{"2022-blake3-aes-256-gcm", "none", "rc4-md5"} {
		bad := fixtureSnapshot(2)
		bad.Nodes[0].Cipher = cipher
		if r.install(context.Background(), bad) == nil {
			t.Errorf("accepted cipher %q", cipher)
		}
	}
}

// providerTLS serves a certificate for chatgpt.com, standing in for the
// provider reached through an SS exit.
func providerTLS(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chatgpt.com"}, DNSNames: []string{"chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
				_ = connection.(*tls.Conn).Handshake()
				_, _ = io.Copy(io.Discard, connection)
			}()
		}
	}()
	return listener.Addr().String(), roots
}

// fakeListener imitates Mihomo's HTTP listener: it acknowledges CONNECT
// before dialing, then behaves as mode says.
func fakeListener(t *testing.T, mode, upstream string, targets chan<- string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				reader := bufio.NewReader(connection)
				request, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				if targets != nil {
					targets <- request.Method + " " + request.RequestURI
				}
				if mode == "reject" {
					_, _ = io.WriteString(connection, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
					return
				}
				_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection established\r\n\r\n")
				switch mode {
				case "close": // wrong SS password or unreachable SS server
					return
				case "hang":
					_, _ = io.Copy(io.Discard, reader)
					return
				}
				peer, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer peer.Close()
				go func() { _, _ = io.Copy(peer, reader); peer.Close() }()
				_, _ = io.Copy(connection, peer)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func probeRequest(t *testing.T, server *httptest.Server, credential string, body string) (int, egress.ProbeResult, string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/probe", strings.NewReader(body))
	request.Header.Set("Authorization", credential)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var result egress.ProbeResult
	if response.StatusCode == 200 && json.Unmarshal(raw, &result) != nil {
		t.Fatalf("invalid probe result %s", raw)
	}
	return response.StatusCode, result, string(raw)
}

func TestProbeCompletesTLSThroughAppliedNodeOnly(t *testing.T) {
	upstream, roots := providerTLS(t)
	r := newRuntime(fixtureKey, &fixtureController{})
	r.probeRoots = roots
	r.probeTimeout = 2 * time.Second
	snapshot := fixtureSnapshot(1)
	if err := r.install(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	node := r.nodes[nodeKey("node-one", 1)]
	targets := make(chan string, 8)
	server := httptest.NewServer(r.controlHandler())
	defer server.Close()
	auth := "Bearer " + fixtureKey
	body := `{"node_id":"node-one","node_revision":1,"target":"chatgpt.com"}`

	node.port = fakeListener(t, "forward", upstream, targets)
	status, result, raw := probeRequest(t, server, auth, body)
	if status != 200 || !result.OK || result.Code != egress.ProbeOK || result.LatencyMS < 0 {
		t.Fatalf("healthy node failed: %d %s", status, raw)
	}
	if got := <-targets; got != "CONNECT chatgpt.com:443" {
		t.Fatalf("probe requested %q", got)
	}
	for _, secret := range []string{fixtureKey, snapshot.Nodes[0].Password, snapshot.Nodes[0].Server, snapshot.Routes[0].Token} {
		if strings.Contains(raw, secret) {
			t.Fatal("probe result exposed private configuration")
		}
	}

	r.probeRoots = x509.NewCertPool()
	if _, result, _ = probeRequest(t, server, auth, body); result.OK || result.Code != egress.ProbeCertificateInvalid {
		t.Fatalf("untrusted certificate reported %+v", result)
	}
	r.probeRoots = roots
	for mode, want := range map[string]string{"close": egress.ProbeHandshakeFailed, "reject": egress.ProbeTunnelFailed} {
		node.port = fakeListener(t, mode, upstream, nil)
		if _, result, _ = probeRequest(t, server, auth, body); result.OK || result.Code != want {
			t.Fatalf("%s listener reported %+v, want %s", mode, result, want)
		}
	}
	r.probeTimeout = 200 * time.Millisecond
	node.port = fakeListener(t, "hang", upstream, nil)
	if _, result, _ = probeRequest(t, server, auth, body); result.OK || result.Code != egress.ProbeTimeout {
		t.Fatalf("stalled node reported %+v", result)
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	node.port = closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if _, result, _ = probeRequest(t, server, auth, body); result.OK || result.Code != egress.ProbeListenerUnavailable {
		t.Fatalf("missing listener reported %+v", result)
	}

	if _, result, _ = probeRequest(t, server, auth, `{"node_id":"node-one","node_revision":2,"target":"chatgpt.com"}`); result.OK || result.Code != egress.ProbeNodeNotApplied {
		t.Fatalf("unapplied revision reported %+v", result)
	}
	for i := 0; i < maxConcurrentProbes; i++ {
		r.probeSlots <- struct{}{}
	}
	if _, result, _ = probeRequest(t, server, auth, body); result.OK || result.Code != egress.ProbeBusy {
		t.Fatalf("saturated probes reported %+v", result)
	}
	for i := 0; i < maxConcurrentProbes; i++ {
		<-r.probeSlots
	}
	r.mu.Lock()
	active := node.active
	r.mu.Unlock()
	if active != 0 {
		t.Fatal("probe retained its node revision")
	}

	for _, invalid := range []string{
		`{"node_id":"node-one","node_revision":1,"target":"evil.invalid"}`,
		`{"node_id":"node-one","node_revision":1,"target":"127.0.0.1"}`,
		`{"node_id":"node-one","node_revision":1,"target":"chatgpt.com:80"}`,
		`{"node_id":"node-one","node_revision":1,"target":"CHATGPT.COM"}`,
		`{"node_id":"../node","node_revision":1,"target":"chatgpt.com"}`,
		`{"node_id":"node-one","node_revision":0,"target":"chatgpt.com"}`,
		`{"node_id":"node-one","node_revision":1,"target":"chatgpt.com","port":8443}`,
		body + body,
	} {
		if status, _, _ := probeRequest(t, server, auth, invalid); status != 400 {
			t.Errorf("accepted probe %s: %d", invalid, status)
		}
	}
	if status, _, _ := probeRequest(t, server, "Bearer wrong", body); status != 401 {
		t.Fatal("unauthenticated probe accepted")
	}
}
