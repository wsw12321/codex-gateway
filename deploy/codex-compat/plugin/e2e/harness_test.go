//go:build e2e

// Package e2e runs the plugin inside a real CLIProxyAPI process. Nothing here
// is mocked on the CPA side: the binary is the pinned upstream source plus the
// residual patch, the plugin is the shipped shared library, and credentials
// are OAuth files on disk. Only the two things outside the trust boundary are
// faked: the Gateway's allocation callbacks and the provider, which is reached
// through a TLS-intercepting proxy trusted via SSL_CERT_FILE.
//
// Everything stays on the loopback interface and no real account is involved.
package e2e

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	internalKey   = "internal-key-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	managementKey = "manage-key-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	userAlice     = "11111111-1111-4111-8111-111111111111"
	userBob       = "22222222-2222-4222-8222-222222222222"
	model         = "gpt-6-astra"
)

// ---- fake Gateway ---------------------------------------------------------

type attemptReport struct {
	RequestToken     string `json:"request_token"`
	UserID           string `json:"user_id"`
	AccountID        string `json:"account_id"`
	ConversationHash string `json:"conversation_hash"`
	Rejected         string `json:"rejected"`
}

type fakeGateway struct {
	server *httptest.Server
	mu     sync.Mutex
	// permitted maps a user to the accounts it may use; a missing user may use
	// every account.
	permitted map[string]map[string]bool
	limits    map[string]int64
	prefer    []string
	reports   []attemptReport
	calls     []string
	unauth    int
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{permitted: map[string]map[string]bool{}, limits: map[string]int64{}}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+internalKey {
		g.unauth++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const prefix = "/internal/upstream-accounts/"
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, prefix)
	g.calls = append(g.calls, action)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var selection struct {
		AccountIDs []string `json:"account_ids"`
		UserID     string   `json:"user_id"`
	}
	switch action {
	case "eligible":
		_ = json.Unmarshal(body, &selection)
		type row struct {
			ID              string `json:"id"`
			ConcurrentLimit int64  `json:"concurrent_limit"`
		}
		rows := []row{}
		for _, id := range selection.AccountIDs {
			if allowed, restricted := g.permitted[selection.UserID]; restricted && !allowed[id] {
				continue
			}
			limit := g.limits[id]
			if limit == 0 {
				limit = 8
			}
			rows = append(rows, row{id, limit})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": rows})
	case "select":
		_ = json.Unmarshal(body, &selection)
		offered := map[string]bool{}
		for _, id := range selection.AccountIDs {
			offered[id] = true
		}
		chosen := ""
		for _, id := range g.prefer {
			if offered[id] {
				chosen = id
				break
			}
		}
		if chosen == "" {
			sort.Strings(selection.AccountIDs)
			chosen = selection.AccountIDs[0]
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"account_id": chosen})
	case "attempt":
		var report attemptReport
		_ = json.Unmarshal(body, &report)
		g.reports = append(g.reports, report)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *fakeGateway) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reports, g.calls = nil, nil
}

func (g *fakeGateway) reportsFor(token string) []attemptReport {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []attemptReport
	for _, report := range g.reports {
		if report.RequestToken == token {
			out = append(out, report)
		}
	}
	return out
}

// ---- fake provider behind a TLS-intercepting proxy -------------------------

type upstreamCall struct {
	Via     string
	Host    string
	Path    string
	Bearer  string
	Headers http.Header
}

type fakeProvider struct {
	t      *testing.T
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	caPEM  []byte

	mu sync.Mutex
	// behavior maps an access token to what the provider answers.
	behavior map[string]string
	calls    []upstreamCall
	leaves   map[string]*tls.Certificate
}

func newFakeProvider(t *testing.T) *fakeProvider {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "codex-gateway e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &fakeProvider{
		t: t, caKey: key, caCert: cert, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		behavior: map[string]string{}, leaves: map[string]*tls.Certificate{},
	}
}

func (p *fakeProvider) leaf(host string) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cert := p.leaves[host]; cert != nil {
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	p.leaves[host] = cert
	return cert, nil
}

// listen starts one forward proxy that records calls under the name via.
func (p *fakeProvider) listen(via string) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			go p.tunnel(via, conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

func (p *fakeProvider) tunnel(via string, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil || request.Method != http.MethodConnect {
		_, _ = io.WriteString(conn, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
		return
	}
	host, _, _ := net.SplitHostPort(request.Host)
	if host != "chatgpt.com" {
		// CPA's own background lookups are not part of any scenario.
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	secure := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			}
			return p.leaf(name)
		},
	})
	if secure.Handshake() != nil {
		return
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.serve(via, host, w, r) }), ReadHeaderTimeout: 10 * time.Second}
	_ = server.Serve(&singleListener{conn: secure, done: make(chan struct{})})
}

type singleListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *singleListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = &notifyConn{Conn: l.conn, closed: l.done} })
	if conn != nil {
		return conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleListener) Close() error   { return nil }
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

type notifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

const sseBody = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"MODEL","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"MODEL","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":4}}}

`

func (p *fakeProvider) serve(via, host string, w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	p.calls = append(p.calls, upstreamCall{Via: via, Host: host, Path: r.URL.Path, Bearer: bearer, Headers: r.Header.Clone()})
	behavior := p.behavior[bearer]
	p.mu.Unlock()
	switch behavior {
	case "limit":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
	case "e500":
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
	case "e400":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request shape","type":"invalid_request_error"}}`)
	case "hold":
		// Keep the stream open until the client goes away.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = io.WriteString(w, strings.SplitN(strings.ReplaceAll(sseBody, "MODEL", model), "\n\n", 2)[0]+"\n\n")
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	default:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.ReplaceAll(sseBody, "MODEL", model))
	}
}

func (p *fakeProvider) set(token, behavior string) {
	p.mu.Lock()
	p.behavior[token] = behavior
	p.mu.Unlock()
}

func (p *fakeProvider) reset() {
	p.mu.Lock()
	p.calls = nil
	p.mu.Unlock()
}

func (p *fakeProvider) seen() []upstreamCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]upstreamCall(nil), p.calls...)
}

// ---- CPA process -----------------------------------------------------------

type account struct {
	name  string
	token string
	// id is the stable Gateway account index.
	id string
}

type harness struct {
	t        *testing.T
	dir      string
	authDir  string
	base     string
	gateway  *fakeGateway
	provider *fakeProvider
	proxy    string
	egress   string
	accounts map[string]account
	cmd      *exec.Cmd
	logs     *bytes.Buffer
	sequence int
	// pluginEnabled and strategy let a scenario start CPA misconfigured.
	pluginEnabled bool
	strategy      string
	gatewayURL    string
	// dataPlaneClosed is set by scenarios in which the plugin refuses even the
	// model listing, so readiness cannot be observed through it.
	dataPlaneClosed bool
}

func requireEnv(t *testing.T, name string) string {
	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is not set", name)
	}
	return value
}

func fakeJWT(accountID string) string {
	payload, _ := json.Marshal(map[string]any{
		"email": accountID + "@example.com", "exp": time.Now().Add(30 * 24 * time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID, "chatgpt_plan_type": "plus", "chatgpt_user_id": "user-" + accountID},
	})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t: t, dir: dir, authDir: filepath.Join(dir, "auth"), gateway: newFakeGateway(t), provider: newFakeProvider(t),
		accounts: map[string]account{}, logs: &bytes.Buffer{}, pluginEnabled: true, strategy: "plugin-only",
	}
	h.gatewayURL = h.gateway.server.URL
	h.proxy = h.provider.listen("global")
	h.egress = h.provider.listen("account-egress")
	if err := os.MkdirAll(h.authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

// addAccount writes a Codex OAuth credential file. extra is merged into it.
func (h *harness) addAccount(name string, extra map[string]any) account {
	h.t.Helper()
	accountID := "acct-" + name
	token := fakeJWT(accountID)
	document := map[string]any{
		"type": "codex", "email": name + "@example.com", "account_id": accountID, "id_token": token, "access_token": token,
		"refresh_token": "refresh-" + name, "expired": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	for key, value := range extra {
		document[key] = value
	}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(h.authDir, "codex-"+name+".json"), raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	created := account{name: name, token: token, id: stableIndex(accountID)}
	h.accounts[name] = created
	return created
}

func freePort(t *testing.T) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func (h *harness) start() {
	h.t.Helper()
	binary := requireEnv(h.t, "CPA_E2E_BINARY")
	pluginDir := requireEnv(h.t, "CPA_E2E_PLUGIN_DIR")
	port := freePort(h.t)
	h.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	write := func(name, content string) string {
		path := filepath.Join(h.dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			h.t.Fatal(err)
		}
		return path
	}
	keyFile := write("sidecar_api_key", internalKey+"\n")
	managementFile := write("cpa_management_key", managementKey+"\n")
	caFile := write("ca.pem", string(h.provider.caPEM))
	config := write("config.yaml", fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
proxy-url: %q
response-compression:
  enabled: false
plugins:
  enabled: true
  dir: %q
  configs:
    codex-gateway:
      enabled: %t
      priority: 100
discovery:
  enabled: false
codex:
  response-steering: false
debug: false
commercial-mode: true
logging-to-file: false
request-log: false
usage-statistics-enabled: false
request-retry: 0
max-retry-credentials: 4
max-retry-interval: 0
passthrough-headers: false
ws-auth: true
streaming:
  send-keepalive-before-ft: false
  bootstrap-retries: 0
routing:
  strategy: %q
  session-affinity: false
pprof:
  enable: false
api-keys:
  - %q
remote-management:
  allow-remote: true
  secret-key: %q
  disable-control-panel: true
  disable-auto-update-panel: true
`, port, h.authDir, h.proxy, pluginDir, h.pluginEnabled, h.strategy, internalKey, managementKey))

	h.cmd = exec.Command(binary, "-config", config, "-local-model")
	h.cmd.Dir = h.dir
	h.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + h.dir, "TZ=UTC",
		"SSL_CERT_FILE=" + caFile, "SSL_CERT_DIR=" + filepath.Join(h.dir, "no-certs"),
		"CPA_PLUGIN_GATEWAY_URL=" + h.gatewayURL,
		"CPA_PLUGIN_LOOPBACK_URL=" + h.base,
		"CPA_PLUGIN_AUTH_DIR=" + h.authDir,
		"CLIPROXY_API_KEY_FILE=" + keyFile,
		"CPA_MANAGEMENT_KEY_FILE=" + managementFile,
	}
	h.cmd.Stdout, h.cmd.Stderr = h.logs, h.logs
	if err := h.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(h.stop)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if h.credentialsLoaded() && (h.dataPlaneClosed || h.modelsReady()) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("CPA did not become ready:\n%s", h.logs.String())
}

// credentialsLoaded reports whether CPA has registered every credential file.
func (h *harness) credentialsLoaded() bool {
	status, body := h.management(http.MethodGet, "/v0/management/auth-files", managementKey, "")
	if status != http.StatusOK {
		return false
	}
	var listing struct {
		Files []json.RawMessage `json:"files"`
	}
	return json.Unmarshal([]byte(body), &listing) == nil && len(listing.Files) >= len(h.accounts)
}

func (h *harness) modelsReady() bool {
	if len(h.accounts) == 0 {
		return true
	}
	request, _ := http.NewRequest(http.MethodGet, h.base+"/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+internalKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode == http.StatusOK && strings.Contains(string(body), `"`+model+`"`)
}

func (h *harness) stop() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Kill()
	_, _ = h.cmd.Process.Wait()
	h.cmd = nil
	if h.t.Failed() {
		logs := h.logs.String()
		if len(logs) > 6000 {
			logs = logs[len(logs)-6000:]
		}
		h.t.Logf("CPA output (tail):\n%s", logs)
	}
}

func (h *harness) alive() bool {
	return h.cmd != nil && h.cmd.Process != nil && h.cmd.ProcessState == nil && h.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// ---- requests --------------------------------------------------------------

type call struct {
	user     string
	affinity byte
	session  string
	parent   string
	groups   string
	token    string
	key      string
	path     string
	headers  map[string]string
	noToken  bool
	timeout  time.Duration
}

type result struct {
	status  int
	body    string
	headers http.Header
	token   string
}

func scope(seed byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func (h *harness) newToken() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (h *harness) do(c call) result {
	h.t.Helper()
	if c.user == "" {
		c.user = userAlice
	}
	if c.affinity == 0 {
		c.affinity = 1
	}
	if c.token == "" && !c.noToken {
		c.token = h.newToken()
	}
	if c.key == "" {
		c.key = internalKey
	}
	if c.path == "" {
		c.path = "/v1/responses"
	}
	if c.timeout == 0 {
		c.timeout = 30 * time.Second
	}
	body := `{"model":"` + model + `","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	request, _ := http.NewRequest(http.MethodPost, h.base+c.path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+c.key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Codex-Gateway-User", c.user)
	request.Header.Set("X-Codex-Gateway-Affinity", scope(c.affinity))
	if !c.noToken {
		request.Header.Set("X-Codex-Gateway-Request", c.token)
	}
	if c.groups != "" {
		request.Header.Set("X-Codex-Gateway-Groups", c.groups)
	}
	if c.session != "" {
		request.Header.Set("session_id", c.session)
	}
	if c.parent != "" {
		request.Header.Set("X-Codex-Parent-Thread-Id", c.parent)
	}
	for name, value := range c.headers {
		request.Header.Set(name, value)
	}
	client := &http.Client{Timeout: c.timeout}
	response, err := client.Do(request)
	if err != nil {
		return result{status: 0, body: err.Error(), token: c.token}
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return result{status: response.StatusCode, body: string(raw), headers: response.Header, token: c.token}
}

func (h *harness) management(method, path, key, body string) (int, string) {
	request, _ := http.NewRequest(method, h.base+path, strings.NewReader(body))
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return response.StatusCode, string(raw)
}

// served returns which account the provider saw for the calls made since the
// last reset, by credential name.
func (h *harness) served() []string {
	var names []string
	for _, c := range h.provider.seen() {
		name := "?"
		for _, candidate := range h.accounts {
			if candidate.token == c.Bearer {
				name = candidate.name
			}
		}
		names = append(names, name)
	}
	return names
}

func (h *harness) resetObservations() {
	h.gateway.reset()
	h.provider.reset()
}

// eventually polls until check passes; asynchronous hooks (usage, completion)
// land shortly after the response.
func (h *harness) eventually(what string, check func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}
