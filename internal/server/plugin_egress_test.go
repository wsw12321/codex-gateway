package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestPushPluginEgressReachesEachProvidersNamespace(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		id := r.URL.Query().Get("id")
		w.Header().Set("Content-Type", "application/json")
		if id == "ffffffffffffffff" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"account_egress_unavailable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+id+`","revision":"r"}`)
	}))
	defer cpa.Close()
	base, _ := url.Parse(cpa.URL)
	s := &Server{config: config.Config{CPAIntegration: "plugin"},
		upstream: gatewayproxy.New(base, "data"), anthropic: gatewayproxy.NewCPAAnthropic(base, "data"), antigravity: gatewayproxy.NewCPAAntigravity(base, "data")}
	for _, client := range []*gatewayproxy.Client{s.upstream, s.anthropic, s.antigravity} {
		client.UsePluginManagement("manage")
	}
	state := store.EgressState{Bindings: []store.EgressBinding{
		{AccountID: "0000000000000001", Provider: store.UpstreamProviderCodex, NodeID: "node-1"},
		{AccountID: "0000000000000002", Provider: store.UpstreamProviderAnthropic, NodeID: "node-1"},
		{AccountID: "0000000000000003", Provider: store.UpstreamProviderAntigravity, NodeID: "node-1"},
		{AccountID: "ffffffffffffffff", Provider: store.UpstreamProviderCodex, NodeID: "node-1"},
		{AccountID: "0000000000000004", Provider: store.UpstreamProviderCodex, NodeID: "node-2"},
	}}
	// One failure does not stop the others: the plugin catches it on use.
	s.pushPluginEgress(context.Background(), nodeBindings(state, "node-1"))
	sort.Strings(seen)
	want := []string{
		"PUT /v0/management/gateway/anthropic-accounts/egress?id=0000000000000002 Bearer manage",
		"PUT /v0/management/gateway/antigravity-accounts/egress?id=0000000000000003 Bearer manage",
		"PUT /v0/management/gateway/upstream-accounts/egress?id=0000000000000001 Bearer manage",
		"PUT /v0/management/gateway/upstream-accounts/egress?id=ffffffffffffffff Bearer manage",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pushed:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
	// The source-patched image resolves routes per operation and has no
	// such route, so nothing is pushed there.
	seen = nil
	s.config.CPAIntegration = "patch"
	s.pushPluginEgress(context.Background(), nodeBindings(state, "node-1"))
	if len(seen) != 0 {
		t.Fatalf("pushed in patch mode: %v", seen)
	}
}
