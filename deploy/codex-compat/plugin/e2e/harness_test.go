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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// namespaces records the account namespace of every callback.
	namespaces []string
	unauth     int
	// routes binds an account to an egress proxy URL; a missing account uses
	// the global egress. egressDown makes every resolution fail.
	routes     map[string]string
	egressDown bool
	// registrations maps an egress registration to the route the account it
	// is committed to gets; committed records "registration=account".
	registrations map[string]string
	committed     []string
	// resolved counts the routes confirmed per account.
	resolved map[string]int
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{permitted: map[string]map[string]bool{}, limits: map[string]int64{}, routes: map[string]string{}, resolved: map[string]int{}}
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
	prefix := "/internal/upstream-accounts/"
	for _, namespace := range []string{"/internal/anthropic-accounts/", "/internal/antigravity-accounts/"} {
		if strings.HasPrefix(r.URL.Path, namespace) {
			prefix = namespace
		}
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, prefix)
	g.calls = append(g.calls, action)
	g.namespaces = append(g.namespaces, strings.Trim(strings.TrimPrefix(prefix, "/internal/"), "/"))
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
	case "resolve-egress":
		var input struct {
			Provider  string `json:"provider"`
			AccountID string `json:"account_id"`
		}
		_ = json.Unmarshal(body, &input)
		if g.egressDown {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		g.resolved[input.AccountID]++
		if route, ok := g.routes[input.AccountID]; ok {
			// Like the Gateway's route id, the revision is opaque.
			digest := sha256.Sum256([]byte(route))
			_ = json.NewEncoder(w).Encode(map[string]string{"mode": "account", "proxy_url": route, "revision": "route:" + hex.EncodeToString(digest[:8])})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"mode": "global", "revision": "global:0"})
	case "register-egress":
		var input struct {
			RegistrationID string `json:"registration_id"`
			Provider       string `json:"provider"`
			AccountID      string `json:"account_id"`
		}
		_ = json.Unmarshal(body, &input)
		route, ok := g.registrations[input.RegistrationID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		g.routes[input.AccountID] = route
		g.committed = append(g.committed, input.RegistrationID+"="+input.AccountID)
		_ = json.NewEncoder(w).Encode(map[string]string{"registration_id": input.RegistrationID, "account_id": input.AccountID, "status": "committed", "revision": "route:1"})
	case "attempt":
		var report attemptReport
		_ = json.Unmarshal(body, &report)
		g.reports = append(g.reports, report)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// egressState returns how often each account's route was confirmed and the
// proxy URL each account must carry, given the global egress.
func (g *fakeGateway) egressState(global string, ids []string) (resolved map[string]int, want map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	resolved, want = map[string]int{}, map[string]string{}
	for _, id := range ids {
		resolved[id] = g.resolved[id]
		want[id] = global
		if route, ok := g.routes[id]; ok {
			want[id] = route
		}
	}
	return resolved, want
}

func (g *fakeGateway) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reports, g.calls, g.namespaces = nil, nil, nil
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
	// ProxyAuth is the credential CPA presented to the proxy on CONNECT.
	ProxyAuth string
	// Form is the decoded body of a token endpoint call.
	Form url.Values
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
	// refused records CONNECTs refused because their route is down.
	refused []upstreamCall
	// discovered is the catalog Google reports for every Antigravity account.
	discovered []string
}

// antigravityHost is where CPA discovers an Antigravity account's models.
const antigravityHost = "daily-cloudcode-pa.googleapis.com"

// deadRouteUser names a route whose egress node is down: the account
// listener refuses its CONNECT the way Squid does when the account's parent
// proxy is unreachable, and never forwards it anywhere else.
const deadRouteUser = "dead"

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
	if host != "chatgpt.com" && host != "api.anthropic.com" && host != "auth.openai.com" && host != antigravityHost {
		// CPA's own background lookups are not part of any scenario.
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if user, _, ok := parseProxyBasic(request.Header.Get("Proxy-Authorization")); ok && user == deadRouteUser {
		p.mu.Lock()
		p.refused = append(p.refused, upstreamCall{Via: via, Host: host, ProxyAuth: request.Header.Get("Proxy-Authorization")})
		p.mu.Unlock()
		_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
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
	proxyAuth := request.Header.Get("Proxy-Authorization")
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.serve(via, host, proxyAuth, w, r) }), ReadHeaderTimeout: 10 * time.Second}
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

func (p *fakeProvider) serve(via, host, proxyAuth string, w http.ResponseWriter, r *http.Request) {
	var form url.Values
	if host == "auth.openai.com" {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		form, _ = url.ParseQuery(string(raw))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	p.calls = append(p.calls, upstreamCall{Via: via, Host: host, Path: r.URL.Path, Bearer: bearer, Headers: r.Header.Clone(), ProxyAuth: proxyAuth, Form: form})
	behavior := p.behavior[bearer]
	p.mu.Unlock()
	if host == "api.anthropic.com" {
		serveClaude(r, w)
		return
	}
	if host == "auth.openai.com" {
		serveCodexToken(r, form, w)
		return
	}
	if host == antigravityHost {
		p.serveAntigravity(r, w)
		return
	}
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
	case "quota-signals", "quota-signals-invalid":
		for name, value := range codexQuotaHeaders {
			w.Header().Set(name, value)
		}
		if behavior == "quota-signals-invalid" {
			w.Header().Set("X-Codex-Primary-Used-Percent", "120")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.ReplaceAll(sseBody, "MODEL", model))
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

// codexQuotaHeaders are the rate-limit headers a Codex response carries.
// CPA records them on the credential; the plan type must never reach the
// Gateway.
var codexQuotaHeaders = map[string]string{
	"X-Codex-Primary-Used-Percent":               "30",
	"X-Codex-Primary-Window-Minutes":             "300",
	"X-Codex-Primary-Reset-At":                   "1794200000",
	"X-Codex-Secondary-Used-Percent":             "55",
	"X-Codex-Secondary-Window-Minutes":           "10080",
	"X-Codex-Secondary-Reset-At":                 "1794700000",
	"X-Codex-Code-Review-Primary-Used-Percent":   "10",
	"X-Codex-Code-Review-Primary-Window-Minutes": "10080",
	"X-Codex-Plan-Type":                          "plus",
}

// serveCodexToken answers ChatGPT's token endpoint. A refresh token
// "rt-NAME" or an authorization code "code-NAME" yields the tokens of
// ChatGPT account "acct-NAME": access token "at-NAME", refresh token
// "rt2-NAME", and an ID token naming the account.
func serveCodexToken(r *http.Request, form url.Values, w http.ResponseWriter) {
	name := ""
	switch {
	case r.URL.Path != "/oauth/token":
	case form.Get("grant_type") == "refresh_token":
		name = strings.TrimPrefix(strings.TrimPrefix(form.Get("refresh_token"), "rt2-"), "rt-")
	case form.Get("grant_type") == "authorization_code":
		name = strings.TrimPrefix(form.Get("code"), "code-")
	}
	w.Header().Set("Content-Type", "application/json")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at-" + name, "refresh_token": "rt2-" + name, "id_token": fakeJWT("acct-" + name),
		"token_type": "Bearer", "expires_in": 3600,
	})
}

// serveAntigravity answers the model discovery CPA runs for an Antigravity
// account. Nothing else of the provider is part of a scenario.
func (p *fakeProvider) serveAntigravity(r *http.Request, w http.ResponseWriter) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p.mu.Lock()
	models := make(map[string]any, len(p.discovered))
	for _, id := range p.discovered {
		models[id] = map[string]any{}
	}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// serveClaude answers a Messages request with the unified rate limit headers
// CPA observes passively.
func serveClaude(r *http.Request, w http.ResponseWriter) {
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed_warning")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1794200000")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.1")
	w.Header().Set("Request-Id", "req_e2e")
	if r.URL.Path != "/v1/messages" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || r.URL.Query().Get("stream") == "true" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, claudeSSE)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"hello"}],`+
		`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}`)
}

const claudeSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

func (p *fakeProvider) set(token, behavior string) {
	p.mu.Lock()
	p.behavior[token] = behavior
	p.mu.Unlock()
}

func (p *fakeProvider) reset() {
	p.mu.Lock()
	p.calls, p.refused = nil, nil
	p.mu.Unlock()
}

func (p *fakeProvider) refusedConnects() []upstreamCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]upstreamCall(nil), p.refused...)
}

// parseProxyBasic decodes a Basic Proxy-Authorization value.
func parseProxyBasic(value string) (user, password string, ok bool) {
	encoded, found := strings.CutPrefix(value, "Basic ")
	if !found {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	user, password, ok = strings.Cut(string(decoded), ":")
	return user, password, ok
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
	// skipEgressWait is set by scenarios whose accounts the plugin can never
	// route, so start must not wait for their credentials to carry a route.
	skipEgressWait bool
	// anthropicIdentities is the verified identity registry written before
	// CPA starts, as the credential page's registration would leave it.
	anthropicIdentities map[string]string
	// antigravityIdentities is the same for Antigravity, keyed by the e-mail
	// address CPA records with the credential.
	antigravityIdentities map[string]string
	// files maps an account name to its credential file.
	files map[string]string
	// readyModel is the model whose listing marks CPA as ready.
	readyModel string
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
		files: map[string]string{}, readyModel: model,
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
	h.accounts[name], h.files[name] = created, "codex-"+name+".json"
	return created
}

// addClaudeAccount writes a Claude OAuth credential whose verified identity
// is already registered, and returns the account with the given stable id.
func (h *harness) addClaudeAccount(name, id string) account {
	h.t.Helper()
	return h.addClaudeAccountWithToken(name, id, "claude-token-"+name)
}

// addClaudeAccountWithToken is addClaudeAccount with a chosen access token.
// CPA takes its Claude Code request path, the one a real OAuth account uses,
// only for a token shaped like Anthropic's OAuth access tokens.
func (h *harness) addClaudeAccountWithToken(name, id, token string) account {
	h.t.Helper()
	accountUUID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa" + fmt.Sprintf("%02x", len(h.accounts))
	organizationUUID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	subject := accountUUID + ":" + organizationUUID
	if h.anthropicIdentities == nil {
		h.anthropicIdentities = map[string]string{}
	}
	h.anthropicIdentities[subject] = id
	document := map[string]any{
		"type": "claude", "email": name + "@example.com", "access_token": token, "refresh_token": "refresh-" + name,
		"expired": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339), "last_refresh": time.Now().UTC().Format(time.RFC3339),
		"account_uuid": accountUUID, "organization_uuid": organizationUUID, "gateway_anthropic_subject": subject,
	}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(h.authDir, "claude-"+name+".json"), raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	created := account{name: name, token: token, id: id}
	h.accounts[name], h.files[name] = created, "claude-"+name+".json"
	return created
}

// addAntigravityAccount writes an Antigravity OAuth credential as CPA's own
// login stores it, with its e-mail identity already registered. The file
// carries the global egress from the start, as every credential in a running
// deployment does, so CPA's first model discovery already has its route.
func (h *harness) addAntigravityAccount(name, id string) account {
	h.t.Helper()
	email := name + "@example.com"
	if h.antigravityIdentities == nil {
		h.antigravityIdentities = map[string]string{}
	}
	h.antigravityIdentities[email] = id
	token := "antigravity-token-" + name
	now := time.Now()
	document := map[string]any{
		"type": "antigravity", "email": email, "project_id": "project-" + name, "access_token": token, "refresh_token": "refresh-" + name,
		"expires_in": 30 * 24 * 3600, "timestamp": now.UnixMilli(), "expired": now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"proxy_url": h.proxy,
	}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(h.authDir, "antigravity-"+name+".json"), raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	created := account{name: name, token: token, id: id}
	h.accounts[name], h.files[name] = created, "antigravity-"+name+".json"
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
	if len(h.anthropicIdentities) > 0 {
		registry, _ := json.Marshal(map[string]any{"version": 1, "accounts": h.anthropicIdentities})
		if err := os.WriteFile(filepath.Join(h.authDir, ".gateway-anthropic-identities"), registry, 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	if len(h.antigravityIdentities) > 0 {
		registry, _ := json.Marshal(map[string]any{"version": 1, "accounts": h.antigravityIdentities})
		if err := os.WriteFile(filepath.Join(h.authDir, ".gateway-antigravity-email-identities"), registry, 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	keyFile := write("sidecar_api_key", internalKey+"\n")
	managementFile := write("cpa_management_key", managementKey+"\n")
	caFile := write("ca.pem", string(h.provider.caPEM))
	config := write("config.yaml", fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
oauth-excluded-models:
  antigravity:
    - "claude-*"
    - "gpt-*"
    - "gemini-3.1-flash-image"
    - "gemini-3.8-flash-tiered"
    - "gemini-4-carbon-high"
    - "gemini-4-hydrogen-xhigh"
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
`, port, h.authDir, pluginDir, h.pluginEnabled, h.strategy, internalKey, managementKey))

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
		// The account-egress listener stands in for egress-allowlist:3129.
		"CPA_PLUGIN_EGRESS_PROXY_HOST=" + strings.TrimPrefix(h.egress, "http://"),
		// As in the entrypoint, CPA has no proxy of its own: the plugin
		// writes the global egress into every credential the Gateway leaves
		// on it, so a CPA fallback could only connect directly.
		"CPA_PLUGIN_GLOBAL_PROXY_URL=" + h.proxy,
		// Only the start-up passes run within a scenario; later changes are
		// left to the per-pick check and the Gateway's push.
		"CPA_PLUGIN_EGRESS_RECONCILE_SECONDS=3600",
	}
	// CPA loads a plugin from a private copy under its temporary directory, so
	// that directory must allow mapping executable pages. CPA_E2E_TMPDIR lets
	// a run reproduce a container's temporary filesystem.
	if tmp := os.Getenv("CPA_E2E_TMPDIR"); tmp != "" {
		h.cmd.Env = append(h.cmd.Env, "TMPDIR="+tmp)
	}
	h.cmd.Stdout, h.cmd.Stderr = h.logs, h.logs
	ids := make([]string, 0, len(h.accounts))
	for _, created := range h.accounts {
		ids = append(ids, created.id)
	}
	before, _ := h.gateway.egressState(h.proxy, ids)
	if err := h.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(h.stop)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if h.credentialsLoaded() && (h.dataPlaneClosed || h.modelsReady()) {
			h.awaitEgress(before, ids)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("CPA did not become ready:\n%s", h.logs.String())
}

// awaitEgress waits for the plugin's start-up pass: every account's route
// confirmed with the Gateway since this start and written into its credential.
// A deployment reaches the same state before the Gateway sends traffic.
func (h *harness) awaitEgress(before map[string]int, ids []string) {
	h.t.Helper()
	if h.skipEgressWait || !h.pluginEnabled || h.dataPlaneClosed || h.gatewayURL != h.gateway.server.URL {
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resolved, want := h.gateway.egressState(h.proxy, ids)
		done := true
		for _, created := range h.accounts {
			if resolved[created.id] <= before[created.id] || h.credentialProxy(created.name) != want[created.id] {
				done = false
			}
		}
		if done {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("credentials did not receive their routes:\n%s", h.logs.String())
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
	return response.StatusCode == http.StatusOK && strings.Contains(string(body), `"`+h.readyModel+`"`)
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
	// noScope omits the caller scope header.
	noScope bool
	timeout time.Duration
	// body replaces the default Responses request.
	body string
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
	if c.body != "" {
		body = c.body
	}
	request, _ := http.NewRequest(http.MethodPost, h.base+c.path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+c.key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Codex-Gateway-User", c.user)
	if !c.noScope {
		request.Header.Set("X-Codex-Gateway-Affinity", scope(c.affinity))
	}
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

// route returns an account-bound egress URL for the harness's egress listener.
func (h *harness) route(name, secret string) string {
	return "http://" + name + ":" + secret + "@" + strings.TrimPrefix(h.egress, "http://")
}

// credentialProxy reads the proxy stored in an account's credential file.
func (h *harness) credentialProxy(name string) string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.authDir, h.files[name]))
	if err != nil {
		h.t.Fatal(err)
	}
	var document struct {
		ProxyURL string `json:"proxy_url"`
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		h.t.Fatal(err)
	}
	return document.ProxyURL
}
