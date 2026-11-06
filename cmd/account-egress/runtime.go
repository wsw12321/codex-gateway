package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

type nodeRuntime struct {
	node   egress.Node
	port   int
	active int
}

type routeRuntime struct {
	token string
	node  *nodeRuntime
}

type proxyController interface {
	apply(context.Context, []byte) error
	probe(context.Context, int) error
	controllerSecret() string
}

type runtime struct {
	mu          sync.Mutex
	key         string
	controller  proxyController
	status      egress.Status
	fingerprint [32]byte
	nodes       map[string]*nodeRuntime
	desired     map[string]bool
	routes      map[string]routeRuntime
	squidIP     string
	// Connectivity probes are owner-initiated; a small fixed pool keeps a
	// burst of tests from competing with model traffic for local resources.
	probeSlots   chan struct{}
	probeTimeout time.Duration
	probeRoots   *x509.CertPool // nil selects the system trust store
}

const (
	maxConcurrentProbes = 4
	defaultProbeTimeout = 10 * time.Second
)

func newRuntime(key string, controller proxyController) *runtime {
	return &runtime{key: key, controller: controller, status: egress.Status{InstanceID: randomID(), UpdatedAt: time.Now().UTC()}, nodes: map[string]*nodeRuntime{}, desired: map[string]bool{}, routes: map[string]routeRuntime{}, squidIP: "172.28.60.2", probeSlots: make(chan struct{}, maxConcurrentProbes), probeTimeout: defaultProbeTimeout}
}

func nodeKey(id string, revision int64) string { return id + "/" + strconv.FormatInt(revision, 10) }

func validServer(server string) bool {
	if ip, err := netip.ParseAddr(server); err == nil {
		return ip.IsValid() && ip.Zone() == ""
	}
	if len(server) == 0 || len(server) > 253 || strings.ToLower(server) != server {
		return false
	}
	labels := strings.Split(server, ".")
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
	return true
}

func validateSnapshot(snapshot egress.Snapshot, key string) error {
	if snapshot.Version < 0 || len(snapshot.Nodes) > 4096 || len(snapshot.Routes) > 65536 {
		return errors.New("invalid snapshot")
	}
	nodes := map[string]bool{}
	for _, node := range snapshot.Nodes {
		if !identifierPattern.MatchString(node.ID) || node.Revision < 1 || !validServer(node.Server) || node.Port < 1 || node.Port > 65535 {
			return errors.New("invalid node")
		}
		if !egress.SupportedCipher(node.Cipher) {
			return errors.New("invalid cipher")
		}
		// SS2022 keys are checked here as well: Mihomo rejects the whole
		// configuration for one malformed key, which would block every node.
		if !egress.ValidPassword(node.Cipher, node.Password) {
			return errors.New("invalid node")
		}
		id := nodeKey(node.ID, node.Revision)
		if nodes[id] {
			return errors.New("duplicate node")
		}
		nodes[id] = true
	}
	routes := map[string]bool{}
	for _, route := range snapshot.Routes {
		if !identifierPattern.MatchString(route.ID) || routes[route.ID] || !nodes[nodeKey(route.NodeID, route.NodeRevision)] || subtle.ConstantTimeCompare([]byte(route.Token), []byte(egress.RouteToken(key, route.ID))) != 1 {
			return errors.New("invalid route")
		}
		routes[route.ID] = true
	}
	return nil
}

func (r *runtime) install(ctx context.Context, snapshot egress.Snapshot) error {
	if err := validateSnapshot(snapshot, r.key); err != nil {
		return err
	}
	raw, _ := json.Marshal(snapshot)
	fingerprint := sha256.Sum256(raw)
	r.mu.Lock()
	defer r.mu.Unlock()
	if snapshot.Version < r.status.AppliedVersion {
		return errors.New("stale snapshot")
	}
	if snapshot.Version == r.status.AppliedVersion && r.status.Ready {
		if fingerprint != r.fingerprint {
			return errors.New("snapshot version conflict")
		}
		return nil
	}
	candidate := map[string]*nodeRuntime{}
	usedPorts := map[int]bool{}
	for id, node := range r.nodes {
		usedPorts[node.port] = true
		if node.active > 0 {
			candidate[id] = node
		}
	}
	desired := map[string]bool{}
	for _, node := range snapshot.Nodes {
		id := nodeKey(node.ID, node.Revision)
		desired[id] = true
		if previous := r.nodes[id]; previous != nil {
			if previous.node != node {
				return errors.New("node revision conflict")
			}
			candidate[id] = previous
			continue
		}
		port := 20000
		for usedPorts[port] && port <= 60000 {
			port++
		}
		if port > 60000 {
			return errors.New("local listener capacity exceeded")
		}
		usedPorts[port] = true
		candidate[id] = &nodeRuntime{node: node, port: port}
	}
	configuration, err := marshalMihomo(candidate, r.controller.controllerSecret())
	if err == nil {
		err = r.controller.apply(ctx, configuration)
	}
	if err == nil {
		for id, node := range candidate {
			if r.nodes[id] == nil {
				if err = r.controller.probe(ctx, node.port); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		// The controller can report success while an individual listener failed.
		// A successful rollback keeps unchanged routes available. Changed route
		// IDs are absent from this older map and remain unavailable.
		restored := false
		if old, marshalErr := marshalMihomo(r.nodes, r.controller.controllerSecret()); marshalErr == nil {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rollbackErr := r.controller.apply(rollbackCtx, old)
			if rollbackErr == nil {
				for _, node := range r.nodes {
					if rollbackErr = r.controller.probe(rollbackCtx, node.port); rollbackErr != nil {
						break
					}
				}
			}
			restored = rollbackErr == nil
			cancel()
		}
		r.status.Ready = r.status.Ready && restored
		r.status.ErrorCode = "configuration_apply_failed"
		r.status.UpdatedAt = time.Now().UTC()
		return errors.New("configuration apply failed")
	}
	routes := make(map[string]routeRuntime, len(snapshot.Routes))
	for _, route := range snapshot.Routes {
		routes[route.ID] = routeRuntime{token: route.Token, node: candidate[nodeKey(route.NodeID, route.NodeRevision)]}
	}
	r.nodes, r.desired, r.routes = candidate, desired, routes
	r.fingerprint = fingerprint
	r.status.AppliedVersion = snapshot.Version
	r.status.Ready = true
	r.status.ErrorCode = ""
	r.status.RouteIDs = make([]string, 0, len(routes))
	for id := range routes {
		r.status.RouteIDs = append(r.status.RouteIDs, id)
	}
	slices.Sort(r.status.RouteIDs)
	r.status.NodeRevisions = make(map[string]int64, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		r.status.NodeRevisions[node.ID] = node.Revision
	}
	r.status.UpdatedAt = time.Now().UTC()
	return nil
}

func (r *runtime) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.prune(ctx)
		}
	}
}

func (r *runtime) prune(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.status.Ready {
		return
	}
	keep := map[string]*nodeRuntime{}
	for id, node := range r.nodes {
		if r.desired[id] || node.active > 0 {
			keep[id] = node
		}
	}
	if len(keep) == len(r.nodes) {
		return
	}
	config, err := marshalMihomo(keep, r.controller.controllerSecret())
	if err != nil {
		return
	}
	applyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if r.controller.apply(applyCtx, config) == nil {
		r.nodes = keep
	}
}

func marshalMihomo(nodes map[string]*nodeRuntime, secret string) ([]byte, error) {
	proxies := []map[string]any{}
	listeners := []map[string]any{}
	// Stable ordering also makes readiness and configuration comparisons useful.
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		node := nodes[id]
		name := "node-" + node.node.ID + "-" + strconv.FormatInt(node.node.Revision, 10)
		proxies = append(proxies, map[string]any{"name": name, "type": "ss", "server": node.node.Server, "port": node.node.Port, "cipher": node.node.Cipher, "password": node.node.Password, "udp": node.node.UDP})
		listeners = append(listeners, map[string]any{"name": name, "type": "http", "listen": "127.0.0.1", "port": strconv.Itoa(node.port), "proxy": name, "users": []any{}})
	}
	return json.Marshal(map[string]any{"port": 0, "socks-port": 0, "mixed-port": 0, "allow-lan": false, "bind-address": "127.0.0.1", "mode": "rule", "log-level": "silent", "ipv6": true, "find-process-mode": "off", "geo-auto-update": false, "external-controller": controllerAddress, "secret": secret, "tun": map[string]bool{"enable": false}, "dns": map[string]bool{"enable": false}, "ntp": map[string]bool{"enable": false}, "profile": map[string]bool{"store-selected": false, "store-fake-ip": false}, "proxies": proxies, "listeners": listeners, "rules": []string{"MATCH,REJECT"}})
}

func (r *runtime) controlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		values := request.Header.Values("Authorization")
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+r.key)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/probe":
			var probe egress.ProbeRequest
			decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 4<<10))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&probe) != nil {
				http.Error(w, "invalid probe", http.StatusBadRequest)
				return
			}
			var extra any
			if decoder.Decode(&extra) != io.EOF || !identifierPattern.MatchString(probe.NodeID) || probe.NodeRevision < 1 || !allowedTarget(net.JoinHostPort(probe.Target, "443")) {
				http.Error(w, "invalid probe", http.StatusBadRequest)
				return
			}
			result := r.probeNode(request.Context(), probe)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(result)
			return
		case request.Method == http.MethodGet && request.URL.Path == "/status":
		case request.Method == http.MethodPut && request.URL.Path == "/config":
			var snapshot egress.Snapshot
			decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 16<<20))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&snapshot) != nil {
				http.Error(w, "invalid configuration", http.StatusBadRequest)
				return
			}
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				http.Error(w, "invalid configuration", http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
			defer cancel()
			if err := r.install(ctx, snapshot); err != nil {
				http.Error(w, "configuration rejected", http.StatusConflict)
				return
			}
		default:
			http.NotFound(w, request)
			return
		}
		r.mu.Lock()
		status := r.status
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
}

func proxyCredentials(header http.Header) (string, string, bool) {
	values := header.Values("Proxy-Authorization")
	if len(values) != 1 {
		return "", "", false
	}
	scheme, value, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", "", false
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	return username, password, ok && username != "" && password != ""
}

func allowedTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || port != "443" || host != strings.ToLower(host) {
		return false
	}
	switch host {
	case "auth.openai.com", "chatgpt.com", "accounts.google.com", "oauth2.googleapis.com", "www.googleapis.com", "cloudcode-pa.googleapis.com", "daily-cloudcode-pa.googleapis.com", "daily-cloudcode-pa.sandbox.googleapis.com", "platform.claude.com", "api.anthropic.com":
		return true
	}
	return false
}

func (r *runtime) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	ip, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil || ip != r.squidIP {
		http.Error(w, "source rejected", http.StatusForbidden)
		return
	}
	if request.Method != http.MethodConnect || request.RequestURI != request.Host || !allowedTarget(request.Host) {
		http.Error(w, "destination rejected", http.StatusForbidden)
		return
	}
	id, token, ok := proxyCredentials(request.Header)
	if !ok {
		http.Error(w, "route authentication required", http.StatusProxyAuthRequired)
		return
	}
	r.mu.Lock()
	route, exists := r.routes[id]
	if !r.status.Ready || !exists || subtle.ConstantTimeCompare([]byte(route.token), []byte(token)) != 1 {
		r.mu.Unlock()
		http.Error(w, "route unavailable", http.StatusBadGateway)
		return
	}
	route.node.active++
	port := route.node.port
	r.mu.Unlock()
	defer func() { r.mu.Lock(); route.node.active--; r.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	upstream, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		http.Error(w, "route unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	_ = upstream.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = fmt.Fprintf(upstream, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", request.Host, request.Host); err != nil {
		http.Error(w, "route unavailable", http.StatusBadGateway)
		return
	}
	buffer := bufio.NewReader(upstream)
	response, err := http.ReadResponse(buffer, request)
	if err != nil || response.StatusCode != http.StatusOK {
		http.Error(w, "route unavailable", http.StatusBadGateway)
		return
	}
	_ = upstream.SetDeadline(time.Time{})
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tunnel unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if buffered.Flush() != nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, buffered)
		// HTTPS clients do not use TCP half-close as a request delimiter. Once
		// the client leaves, release the SS tunnel even if its peer stays idle.
		_ = upstream.Close()
		_ = client.Close()
		done <- struct{}{}
	}()
	_, _ = io.Copy(client, buffer)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// probeNode opens a TLS session to an allowed provider host through the
// node's applied local listener. Mihomo acknowledges CONNECT before it dials
// the SS server, so only a completed handshake shows that the node is
// reachable, its cipher and password are accepted, and the exit reaches the
// provider. The probe uses the same listener as model traffic and never dials
// an origin directly. Results carry stable codes only, never error text.
func (r *runtime) probeNode(ctx context.Context, probe egress.ProbeRequest) egress.ProbeResult {
	select {
	case r.probeSlots <- struct{}{}:
		defer func() { <-r.probeSlots }()
	default:
		return egress.ProbeResult{Code: egress.ProbeBusy}
	}
	key := nodeKey(probe.NodeID, probe.NodeRevision)
	r.mu.Lock()
	node := r.nodes[key]
	if !r.status.Ready || node == nil || !r.desired[key] {
		r.mu.Unlock()
		return egress.ProbeResult{Code: egress.ProbeNodeNotApplied}
	}
	// Hold the revision like a tunnel so pruning keeps its listener.
	node.active++
	port := node.port
	r.mu.Unlock()
	defer func() { r.mu.Lock(); node.active--; r.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()
	started := time.Now()
	result := func(code string) egress.ProbeResult {
		return egress.ProbeResult{OK: code == egress.ProbeOK, Code: code, LatencyMS: time.Since(started).Milliseconds()}
	}
	failure := func(err error, fallback string) egress.ProbeResult {
		var timeout net.Error
		if ctx.Err() != nil || errors.As(err, &timeout) && timeout.Timeout() {
			return result(egress.ProbeTimeout)
		}
		return result(fallback)
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return failure(err, egress.ProbeListenerUnavailable)
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.SetDeadline(time.Now()) })
	defer stop()
	target := net.JoinHostPort(probe.Target, "443")
	if _, err = fmt.Fprintf(connection, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return failure(err, egress.ProbeTunnelFailed)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return failure(err, egress.ProbeTunnelFailed)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result(egress.ProbeTunnelFailed)
	}
	session := tls.Client(bufferedConn{Conn: connection, reader: reader}, &tls.Config{ServerName: probe.Target, MinVersion: tls.VersionTLS12, RootCAs: r.probeRoots, NextProtos: []string{"http/1.1"}})
	if err = session.HandshakeContext(ctx); err != nil {
		var verification *tls.CertificateVerificationError
		if errors.As(err, &verification) {
			return result(egress.ProbeCertificateInvalid)
		}
		return failure(err, egress.ProbeHandshakeFailed)
	}
	outcome := result(egress.ProbeOK)
	_ = session.Close()
	return outcome
}

type controller struct {
	base, secret string
	client       *http.Client
}

func newController(base, secret string) *controller {
	return &controller{base: base, secret: secret, client: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}}
}
func (c *controller) controllerSecret() string { return c.secret }
func (c *controller) request(ctx context.Context, method, path string, payload []byte) error {
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return errors.New("local controller unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+c.secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return errors.New("local controller unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("local controller rejected configuration")
	}
	return nil
}
func (c *controller) apply(ctx context.Context, configuration []byte) error {
	payload, _ := json.Marshal(map[string]string{"payload": string(configuration)})
	return c.request(ctx, http.MethodPut, "/configs?force=false", payload)
}
func (c *controller) health(ctx context.Context) error {
	return c.request(ctx, http.MethodGet, "/version", nil)
}
func (c *controller) probe(ctx context.Context, port int) error {
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		_ = connection.Close()
	}
	return err
}
