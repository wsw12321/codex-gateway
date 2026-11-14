package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/egress"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

type egressRoundTripper func(*http.Request) (*http.Response, error)

func (f egressRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEgressControlSupportsRuntimeRouteCapacity(t *testing.T) {
	controlURL, _ := url.Parse("http://account-egress:17892")
	s := &Server{config: config.Config{EgressControlKey: strings.Repeat("c", 32), EgressControlURL: controlURL}}
	want := egress.Status{InstanceID: "instance", Ready: true, NodeRevisions: map[string]int64{}}
	for i := 0; i < 65536; i++ {
		want.RouteIDs = append(want.RouteIDs, strings.Repeat("0", 64-len(strconv.Itoa(i)))+strconv.Itoa(i))
	}
	for i := 0; i < 4096; i++ {
		want.NodeRevisions["00000000-0000-4000-8000-"+strings.Repeat("0", 12-len(strconv.Itoa(i)))+strconv.Itoa(i)] = 1
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	s.accountEgressRuntime().client.Transport = egressRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	var got egress.Status
	if err := s.egressControl(context.Background(), http.MethodGet, "/status", nil, &got); err != nil || len(got.RouteIDs) != len(want.RouteIDs) || len(got.NodeRevisions) != len(want.NodeRevisions) {
		t.Fatal("runtime capacity exceeds control status response limit")
	}
}

func TestEgressSnapshotAndExactAppliedRoutes(t *testing.T) {
	s := &Server{config: config.Config{EgressControlKey: strings.Repeat("c", 32), APIKeyEncryptionKey: bytes.Repeat([]byte{1}, 32)}}
	n := store.EgressServer{ID: "node-one", Name: "one", Revision: 3, Server: "ss.example.com", Port: 8388, Cipher: "aes-256-gcm"}
	n.PasswordCiphertext, _ = security.EncryptEgressPassword(s.config.APIKeyEncryptionKey, n.ID, "private-SS-password")
	b := store.EgressBinding{Provider: "codex", AccountID: "0123456789abcdef", NodeID: n.ID, Revision: 4}
	state := store.EgressState{Version: 9, Nodes: []store.EgressServer{n}, Bindings: []store.EgressBinding{b}, Registrations: []store.EgressRegistration{{ID: "onboarding", Provider: "codex", AccountID: b.AccountID, NodeID: n.ID, Revision: 7, State: "prepared"}}}
	snapshot, err := s.runtimeSnapshot(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 3 || snapshot.Nodes[0].Password != "private-SS-password" {
		t.Fatal("runtime lacks account and prepared routes")
	}
	live := snapshot.Routes[0]
	status := egress.Status{Ready: true, InstanceID: "instance", AppliedVersion: 1, RouteIDs: []string{live.ID}, NodeRevisions: map[string]int64{n.ID: n.Revision}}
	result, err := s.appliedRouteResolution(b.Provider, b.AccountID, b.Revision, b.NodeID, state, status)
	if err != nil {
		t.Fatal("unchanged route blocked after unrelated apply failure")
	}
	u, err := url.Parse(result.ProxyURL)
	if err != nil || u.Host != "egress-allowlist:3129" || u.User.Username() != live.ID || strings.Contains(result.ProxyURL, "private-SS-password") {
		t.Fatal("unsafe internal route")
	}
	n.Revision++
	state.Nodes[0] = n
	if _, err = s.appliedRouteResolution(b.Provider, b.AccountID, b.Revision, b.NodeID, state, status); err == nil {
		t.Fatal("edited node reused old applied route")
	}
	if _, err = s.appliedRouteResolution(b.Provider, b.AccountID, b.Revision, b.NodeID, state, egress.Status{}); err == nil {
		t.Fatal("unready runtime resolved account route")
	}
	if result, err = s.appliedRouteResolution(b.Provider, b.AccountID, b.Revision, "", state, egress.Status{}); err != nil || result.Mode != "global" {
		t.Fatal("global route depends on SS runtime")
	}
	public, _ := json.Marshal(map[string]any{"server": egressNodeDTO(n), "runtime": publicEgressStatus(status)})
	if strings.Contains(string(public), "private-SS-password") || strings.Contains(string(public), "password_ciphertext") || strings.Contains(string(public), "route_ids") {
		t.Fatal("private runtime data returned to browser")
	}
}

func TestEgressInternalAuthorizationAndStrictInput(t *testing.T) {
	s := &Server{config: config.Config{SidecarToken: "internal"}, mux: http.NewServeMux()}
	s.egressRoutes()
	for _, path := range []string{"resolve-egress", "register-egress"} {
		r := httptest.NewRequest("POST", "/internal/upstream-accounts/"+path, strings.NewReader(`{"password":"canary"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 401 || strings.Contains(w.Body.String(), "canary") {
			t.Fatal("internal auth did not run first")
		}
	}
	for _, body := range []string{`{"provider":"codex","account_id":"0123456789abcdef","provider":"anthropic"}`, `{"provider":"codex","account_id":"0123456789abcdef","proxy_url":"http://evil"}`, `{"provider":"unknown","account_id":"0123456789abcdef"}`, `{"provider":"codex","account_id":"../../etc/passwd"}`} {
		r := httptest.NewRequest("POST", "/internal/upstream-accounts/resolve-egress", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer internal")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("invalid request status %d", w.Code)
		}
	}
}
