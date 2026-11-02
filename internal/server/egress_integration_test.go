//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
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
	if err := f.s.store.WithUpstreamProvider("antigravity").EnsureUpstreamAccount(context.Background(), "aaaabbbbccccdddd", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.WithUpstreamProvider("antigravity").SetEgressBinding(context.Background(), "aaaabbbbccccdddd", second, store.EgressActor{UserID: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	f.s.config.AntigravityTransport = "legacy-bridge"
	w := httptest.NewRecorder()
	if f.s.legacyEgressAllowed(w, httptest.NewRequest("POST", "/v1/responses", nil)) || w.Code != 503 {
		t.Fatal("rollback bridge accepted bound AGY account")
	}
}
