//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestEgressHTTPPostgresRuntimeIsolation(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	_, _, memberCookie := profileHTTPMember(t, f, "egress-member")
	f.s.config.EgressControlKey = strings.Repeat("c", 32)
	f.s.config.APIKeyEncryptionKey = bytes.Repeat([]byte{0x65}, 32)
	f.s.config.SidecarToken = "egress-sidecar"
	var mu sync.Mutex
	status := egress.Status{InstanceID: "fixture", Ready: true}
	applyFails := false
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.s.config.EgressControlKey {
			w.WriteHeader(401)
			return
		}
		if r.Method == "PUT" {
			if applyFails {
				w.WriteHeader(503)
				return
			}
			var snapshot egress.Snapshot
			if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			status.AppliedVersion = snapshot.Version
			status.RouteIDs = []string{}
			status.NodeRevisions = map[string]int64{}
			for _, route := range snapshot.Routes {
				status.RouteIDs = append(status.RouteIDs, route.ID)
			}
			for _, node := range snapshot.Nodes {
				status.NodeRevisions[node.ID] = node.Revision
			}
			status.UpdatedAt = time.Now().UTC()
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer control.Close()
	f.s.config.EgressControlURL, _ = url.Parse(control.URL)
	create := func(name string) string {
		w := f.send(t, "POST", "/admin/egress-servers", map[string]any{"name": name, "server": "SS.EXAMPLE.COM", "port": 8388, "cipher": "aes-256-gcm", "password": "SS-secret-canary", "udp": true}, f.cookie, 201)
		if strings.Contains(w.Body.String(), "SS-secret-canary") || strings.Contains(w.Body.String(), "route_ids") {
			t.Fatal("browser response leaked private config")
		}
		var response struct {
			Server egressServerDTO `json:"server"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Server.Server != "ss.example.com" || !response.Server.HasPassword {
			t.Fatal("node normalization failed")
		}
		return response.Server.ID
	}
	f.send(t, "GET", "/admin/egress-servers", nil, "", 401)
	f.send(t, "GET", "/admin/egress-servers", nil, memberCookie, 403)
	first, second := create("first"), create("second")
	const account = "0123456789abcdef"
	const other = "fedcba9876543210"
	for _, id := range []string{account, other} {
		if err := f.s.store.EnsureUpstreamAccount(context.Background(), id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(id, node string) {
		f.send(t, "PUT", "/admin/upstream-accounts/"+id+"/egress", map[string]any{"egress_server_id": node}, f.cookie, 200)
	}
	bind(account, first)
	bind(other, second)
	internal := func(action string, body any, want int) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/internal/upstream-accounts/"+action, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer egress-sidecar")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: status=%d %s", action, w.Code, w.Body)
		}
		return w
	}
	resolve := func(id string, want int) egress.Resolution {
		w := internal("resolve-egress", map[string]any{"provider": "codex", "account_id": id}, want)
		var result egress.Resolution
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		return result
	}
	before := resolve(other, 200)
	// Missing selection must never silently unbind; explicit null does unbind.
	f.send(t, "PUT", "/admin/upstream-accounts/"+other+"/egress", map[string]any{}, f.cookie, 400)
	if unchanged := resolve(other, 200); unchanged.Revision != before.Revision {
		t.Fatal("missing binding selection changed the route")
	}
	f.send(t, "PUT", "/admin/upstream-accounts/"+other+"/egress", map[string]any{"egress_server_id": nil}, f.cookie, 200)
	if unbound := resolve(other, 200); unbound.Mode != "global" {
		t.Fatal("explicit null did not unbind the route")
	}
	bind(other, second)
	before = resolve(other, 200)
	mu.Lock()
	applyFails = true
	mu.Unlock()
	f.send(t, "PUT", "/admin/egress-servers/"+first, map[string]any{"name": "changed", "server": "ss.example.com", "port": 8388, "cipher": "aes-256-gcm", "password": "new-secret-canary", "udp": false}, f.cookie, 200)
	resolve(account, 503)
	after := resolve(other, 200)
	if before.Revision != after.Revision || after.Mode != "account" {
		t.Fatal("unrelated apply failure changed healthy route")
	}
	f.s.config.EgressControlKey = ""
	resolve(other, 503)
	if result := resolve("0000000000000001", 200); result.Mode != "global" {
		t.Fatal("unbound account lost global mode")
	}
	f.s.config.EgressControlKey = strings.Repeat("c", 32)
	// Failed registration keeps the old live binding while staged credentials
	// and the prepared route can be retried after the controller recovers.
	selected := second
	registration, err := f.s.store.CreateEgressRegistration(context.Background(), "codex", &selected, store.EgressActor{UserID: f.owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	registerBody := map[string]any{"registration_id": registration.ID, "provider": "codex", "account_id": account}
	internal("register-egress", registerBody, 503)
	state, err := f.s.store.EgressSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range state.Bindings {
		if binding.AccountID == account && binding.NodeID != first {
			t.Fatal("failed apply switched old binding")
		}
	}
	mu.Lock()
	applyFails = false
	mu.Unlock()
	internal("register-egress", registerBody, 200)
	resolve(account, 200)
	snapshotBefore, _ := f.s.store.EgressSnapshot(context.Background())
	internal("register-egress", registerBody, 200)
	snapshotAfter, _ := f.s.store.EgressSnapshot(context.Background())
	if snapshotBefore.Version != snapshotAfter.Version {
		t.Fatal("registration replay mutated config")
	}
	f.send(t, "PUT", "/admin/upstream-accounts/"+other+"/egress", map[string]any{"egress_server_id": second}, memberCookie, 403)
}

func TestEgressSS2022AndConnectivityProbePostgres(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	_, _, memberCookie := profileHTTPMember(t, f, "egress-probe-member")
	f.s.config.EgressControlKey = strings.Repeat("c", 32)
	f.s.config.APIKeyEncryptionKey = bytes.Repeat([]byte{0x65}, 32)
	const key = "AAECAwQFBgcICQoLDA0ODw=="
	var mu sync.Mutex
	status := egress.Status{InstanceID: "fixture", Ready: true}
	var probes []egress.ProbeRequest
	probeResult := egress.ProbeResult{OK: true, Code: egress.ProbeOK, LatencyMS: 87}
	probeFails, holdApply := false, false
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.s.config.EgressControlKey {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/probe":
			if probeFails {
				w.WriteHeader(500)
				return
			}
			var probe egress.ProbeRequest
			if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
				t.Error(err)
			}
			probes = append(probes, probe)
			_ = json.NewEncoder(w).Encode(probeResult)
			return
		case r.Method == "PUT" && !holdApply:
			var snapshot egress.Snapshot
			if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
				t.Error(err)
			}
			status.AppliedVersion = snapshot.Version
			status.NodeRevisions = map[string]int64{}
			for _, node := range snapshot.Nodes {
				// The runtime and Mihomo reject the whole snapshot for one bad key.
				if !egress.ValidPassword(node.Cipher, node.Password) {
					t.Errorf("gateway sent an invalid %s credential", node.Cipher)
				}
				status.NodeRevisions[node.ID] = node.Revision
			}
		case r.Method == "PUT":
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer control.Close()
	f.s.config.EgressControlURL, _ = url.Parse(control.URL)
	nodeID := func(w *httptest.ResponseRecorder) string {
		var response struct {
			Server egressServerDTO `json:"server"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Server.ID
	}
	node := func(cipher, password string) map[string]any {
		body := map[string]any{"name": "ss2022", "server": "ss.example.com", "port": 8388, "cipher": cipher, "udp": false}
		if password != "" {
			body["password"] = password
		}
		return body
	}
	for _, bad := range []string{"plain-SS-canary", "AAECAwQFBgcICQoLDA0ODw", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="} {
		w := f.send(t, "POST", "/admin/egress-servers", node(egress.SS2022AES128GCM, bad), f.cookie, 400)
		if !strings.Contains(w.Body.String(), "invalid_egress_password") || strings.Contains(w.Body.String(), bad) {
			t.Fatalf("malformed SS2022 key response: %s", w.Body)
		}
	}
	ss2022 := nodeID(f.send(t, "POST", "/admin/egress-servers", node(egress.SS2022AES128GCM, key+":EBESExQVFhcYGRobHB0eHw=="), f.cookie, 201))
	f.send(t, "PUT", "/admin/egress-servers/"+ss2022, node(egress.SS2022AES128GCM, ""), f.cookie, 200)
	legacy := nodeID(f.send(t, "POST", "/admin/egress-servers", node("aes-256-gcm", "legacy-SS-canary"), f.cookie, 201))
	w := f.send(t, "PUT", "/admin/egress-servers/"+legacy, node(egress.SS2022AES128GCM, ""), f.cookie, 400)
	if !strings.Contains(w.Body.String(), "egress_password_required") || strings.Contains(w.Body.String(), "legacy-SS-canary") {
		t.Fatalf("retained legacy password switched to SS2022: %s", w.Body)
	}
	f.send(t, "PUT", "/admin/egress-servers/"+legacy, node(egress.SS2022AES128GCM, key), f.cookie, 200)
	current, err := f.s.store.EgressServer(context.Background(), legacy)
	if err != nil || current.Cipher != egress.SS2022AES128GCM {
		t.Fatal("cipher switch with a new key was not saved")
	}
	// The retained-key guard refuses a write based on an older revision.
	stale := current
	stale.Name, stale.PasswordCiphertext, stale.Revision = "stale", nil, current.Revision-1
	if _, err = f.s.store.SaveEgressServer(context.Background(), stale, false, store.EgressActor{UserID: f.owner.ID}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale retained-key save: %v", err)
	}

	test := func(id string, body any, cookie string, want int) map[string]any {
		w := f.send(t, "POST", "/admin/egress-servers/"+id+"/test", body, cookie, want)
		if strings.Contains(w.Body.String(), "canary") || strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), "ss.example.com") {
			t.Fatal("probe response leaked node configuration")
		}
		var result map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		return result
	}
	test(ss2022, map[string]any{}, "", 401)
	test(ss2022, map[string]any{}, memberCookie, 403)
	result := test(ss2022, map[string]any{}, f.cookie, 200)
	if result["ok"] != true || result["code"] != "ok" || result["target"] != "chatgpt.com" || result["latency_ms"] != float64(87) {
		t.Fatalf("probe result %+v", result)
	}
	snapshot, _ := f.s.store.EgressSnapshot(context.Background())
	var revision int64
	for _, n := range snapshot.Nodes {
		if n.ID == ss2022 {
			revision = n.Revision
		}
	}
	mu.Lock()
	if len(probes) != 1 || probes[0] != (egress.ProbeRequest{NodeID: ss2022, NodeRevision: revision, Target: "chatgpt.com"}) {
		t.Fatalf("runtime probe request %+v", probes)
	}
	probeResult = egress.ProbeResult{Code: "dial tcp 203.0.113.9: secret detail", LatencyMS: -5}
	mu.Unlock()
	result = test(ss2022, map[string]any{"target": "anthropic"}, f.cookie, 200)
	if result["ok"] != false || result["code"] != egress.ProbeHandshakeFailed || result["target"] != "api.anthropic.com" || result["latency_ms"] != float64(0) {
		t.Fatalf("unknown runtime outcome was not normalized: %+v", result)
	}
	for _, body := range []any{map[string]any{"target": "evil"}, map[string]any{"target": "codex", "host": "evil.invalid"}} {
		test(ss2022, body, f.cookie, 400)
	}
	test("00000000-0000-4000-8000-000000000000", map[string]any{}, f.cookie, 404)
	test("not-a-uuid", map[string]any{}, f.cookie, 400)
	mu.Lock()
	probeFails = true
	mu.Unlock()
	test(ss2022, map[string]any{}, f.cookie, 503)
	mu.Lock()
	probeFails, holdApply = false, true
	mu.Unlock()
	// An edit that the runtime has not applied cannot be tested as if it were.
	f.send(t, "PUT", "/admin/egress-servers/"+ss2022, map[string]any{"name": "edited", "server": "ss.example.com", "port": 8389, "cipher": egress.SS2022AES128GCM, "udp": false}, f.cookie, 200)
	test(ss2022, map[string]any{}, f.cookie, 409)
	mu.Lock()
	count := len(probes)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("unapplied node reached the runtime probe: %d", count)
	}
}

func TestEgressSOCKS5HTTPPostgres(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	f.s.config.EgressControlKey = strings.Repeat("c", 32)
	f.s.config.APIKeyEncryptionKey = bytes.Repeat([]byte{0x66}, 32)
	var mu sync.Mutex
	status := egress.Status{InstanceID: "fixture", Ready: true}
	var last egress.Snapshot
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.s.config.EgressControlKey {
			w.WriteHeader(401)
			return
		}
		if r.Method == "PUT" {
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			last = egress.Snapshot{}
			if err := decoder.Decode(&last); err != nil {
				t.Error(err)
			}
			status.AppliedVersion = last.Version
			status.NodeRevisions = map[string]int64{}
			for _, node := range last.Nodes {
				status.NodeRevisions[node.ID] = node.Revision
			}
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer control.Close()
	f.s.config.EgressControlURL, _ = url.Parse(control.URL)
	saved := func(w *httptest.ResponseRecorder) egressServerDTO {
		var response struct {
			Server egressServerDTO `json:"server"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), "SOCKS-canary") || strings.Contains(w.Body.String(), "SS-canary") {
			t.Fatalf("response leaked a password: %s", w.Body)
		}
		return response.Server
	}
	runtimeNode := func(id string) egress.Node {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range last.Nodes {
			if n.ID == id {
				return n
			}
		}
		t.Fatalf("node %s missing from runtime snapshot", id)
		return egress.Node{}
	}
	socks := func(username, password string) map[string]any {
		body := map[string]any{"name": "socks", "protocol": "socks5", "server": "SOCKS.example.com", "port": 1080, "username": username, "udp": true}
		if password != "" {
			body["password"] = password
		}
		return body
	}
	wantCode := func(w *httptest.ResponseRecorder, code string) {
		t.Helper()
		if !strings.Contains(w.Body.String(), code) || strings.Contains(w.Body.String(), "canary") {
			t.Fatalf("want %s, got %s", code, w.Body)
		}
	}

	open := saved(f.send(t, "POST", "/admin/egress-servers", socks("", ""), f.cookie, 201))
	if open.Protocol != "socks5" || open.Server != "socks.example.com" || open.HasPassword || open.Cipher != "" || !open.UDP {
		t.Fatalf("unauthenticated SOCKS5 node: %+v", open)
	}
	if n := runtimeNode(open.ID); n != (egress.Node{ID: open.ID, Revision: open.Revision, Protocol: "socks5", Server: "socks.example.com", Port: 1080, UDP: true}) {
		t.Fatalf("runtime node = %+v", n)
	}
	wantCode(f.send(t, "POST", "/admin/egress-servers", socks("user", ""), f.cookie, 400), "invalid_egress_request")
	wantCode(f.send(t, "POST", "/admin/egress-servers", socks("", "SOCKS-canary"), f.cookie, 400), "invalid_egress_password")
	wantCode(f.send(t, "POST", "/admin/egress-servers", socks("user", "SOCKS-canary\nx"), f.cookie, 400), "invalid_egress_password")
	wantCode(f.send(t, "POST", "/admin/egress-servers", socks("user", strings.Repeat("p", 256)), f.cookie, 400), "invalid_egress_password")
	wantCode(f.send(t, "POST", "/admin/egress-servers", map[string]any{"name": "x", "protocol": "socks5", "server": "socks.example.com", "port": 1080, "cipher": "aes-256-gcm", "udp": false}, f.cookie, 400), "invalid_egress_request")
	wantCode(f.send(t, "POST", "/admin/egress-servers", map[string]any{"name": "x", "protocol": "http", "server": "socks.example.com", "port": 1080, "udp": false}, f.cookie, 400), "invalid_egress_request")
	wantCode(f.send(t, "POST", "/admin/egress-servers", map[string]any{"name": "x", "server": "ss.example.com", "port": 8388, "cipher": "aes-256-gcm", "username": "user", "password": "SS-canary", "udp": false}, f.cookie, 400), "invalid_egress_request")

	// Enabling authentication requires a password; then a blank one keeps it.
	wantCode(f.send(t, "PUT", "/admin/egress-servers/"+open.ID, socks("user", ""), f.cookie, 400), "egress_password_required")
	authed := saved(f.send(t, "PUT", "/admin/egress-servers/"+open.ID, socks("user", "SOCKS-canary"), f.cookie, 200))
	if !authed.HasPassword || authed.Username != "user" {
		t.Fatalf("authenticated SOCKS5 node: %+v", authed)
	}
	authed = saved(f.send(t, "PUT", "/admin/egress-servers/"+open.ID, socks("user", ""), f.cookie, 200))
	if n := runtimeNode(open.ID); n.Username != "user" || n.Password != "SOCKS-canary" || n.Cipher != "" || n.Revision != authed.Revision {
		t.Fatalf("retained SOCKS5 credentials not applied: %+v", n)
	}
	// Switching to SS needs a new password: a SOCKS5 password is not an SS one.
	ss := map[string]any{"name": "socks", "protocol": "ss", "server": "ss.example.com", "port": 8388, "cipher": "aes-256-gcm", "udp": false}
	wantCode(f.send(t, "PUT", "/admin/egress-servers/"+open.ID, ss, f.cookie, 400), "egress_password_required")
	ss["password"] = "SS-canary"
	switched := saved(f.send(t, "PUT", "/admin/egress-servers/"+open.ID, ss, f.cookie, 200))
	if switched.Protocol != "ss" || switched.Username != "" || switched.Cipher != "aes-256-gcm" {
		t.Fatalf("switched node: %+v", switched)
	}
	if n := runtimeNode(open.ID); n.Protocol != "" || n.Password != "SS-canary" {
		t.Fatalf("SS node runtime shape: %+v", n)
	}
	// And back to SOCKS5 with a username also needs a new password.
	wantCode(f.send(t, "PUT", "/admin/egress-servers/"+open.ID, socks("user", ""), f.cookie, 400), "egress_password_required")
	cleared := saved(f.send(t, "PUT", "/admin/egress-servers/"+open.ID, socks("", ""), f.cookie, 200))
	if cleared.HasPassword || cleared.Protocol != "socks5" {
		t.Fatalf("SS to unauthenticated SOCKS5: %+v", cleared)
	}
	if n := runtimeNode(open.ID); n.Password != "" || n.Username != "" {
		t.Fatalf("cleared SOCKS5 node still carries credentials: %+v", n)
	}
	list := f.send(t, "GET", "/admin/egress-servers", nil, f.cookie, 200)
	if strings.Contains(list.Body.String(), "canary") || !strings.Contains(list.Body.String(), `"protocol":"socks5"`) {
		t.Fatalf("list response: %s", list.Body)
	}
}
