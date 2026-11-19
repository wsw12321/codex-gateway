package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
)

func TestDiscoverSupplierModels(t *testing.T) {
	for _, test := range []struct {
		provider, path, header, secret, listing string
		status                                  int
		fallback, failed                        bool
	}{
		{"codex", "/v1/models", "Authorization", "Bearer test-key", `{"data":[{"id":"expensive-unpriced"},{"id":"model-a"}]}`, 200, false, false},
		{"anthropic", "/v1/models", "X-Api-Key", "test-key", `{"data":[{"id":"model-a"}]}`, 200, false, false},
		{"antigravity", "/v1beta/models", "X-Goog-Api-Key", "test-key", `{"models":[{"name":"models/model-a","supportedGenerationMethods":["generateContent"]}]}`, 200, false, false},
		{"codex", "/v1/models", "Authorization", "Bearer test-key", `{"data":[{"id":"not-priced"}]}`, 200, true, false},
		{"codex", "/v1/models", "Authorization", "Bearer test-key", `{}`, 404, true, false},
		{"codex", "/v1/models", "Authorization", "Bearer test-key", `{}`, 401, false, true},
		{"codex", "/v1/models", "Authorization", "Bearer test-key", `{}`, 302, false, true},
	} {
		t.Run(fmt.Sprint(test.provider, "-", test.status, "-", test.fallback), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path || r.Header.Get(test.header) != test.secret {
					t.Error("incorrect discovery request")
				}
				w.Header().Set("Content-Type", "application/json")
				if test.status == 302 {
					w.Header().Set("Location", "/never")
				}
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.listing)
			}))
			defer remote.Close()
			base, _ := url.Parse(remote.URL + "/" + supplierVersion(test.provider))
			models, fallback, err := discoverSupplierModels(t.Context(), supplierHTTP(remote.Client().Transport.(*http.Transport)), base, test.provider, "test-key", []string{"model-a"})
			if fallback != test.fallback || (err != nil) != test.failed || !test.failed && !test.fallback && !slices.Equal(models, []string{"model-a"}) {
				t.Fatalf("models=%v fallback=%v err=%v", models, fallback, err)
			}
		})
	}
}

func TestSupplierSlotsShareRootAndDrain(t *testing.T) {
	now := time.Now()
	runtime := supplierRuntime{slots: map[string]map[string]supplierSlot{"account": {"root": {active: 2, until: now.Add(time.Minute)}}}}
	if !runtime.slotAvailableLocked("account", "root", 1, now) || runtime.slotAvailableLocked("account", "other", 1, now) {
		t.Fatal("root concurrency limit not applied")
	}
	runtime.dropIdleLocked("account")
	runtime.release("account", "root", true, nil)
	if runtime.active("account") != 1 {
		t.Fatal("drain cancelled an active request")
	}
	runtime.release("account", "root", true, nil)
	if runtime.active("account") != 0 || !runtime.slotAvailableLocked("account", "other", 1, now) {
		t.Fatal("drained slot was retained")
	}
}

func TestSupplierUsesAuthenticatedConnectWithoutSendingProviderKey(t *testing.T) {
	const account, host = "1234567890abcdef", "api.example.com"
	const secret = "supplier-control-key-is-at-least-32-bytes"
	var called atomic.Bool
	parent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.Method != http.MethodConnect || r.Host != host+":443" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("unexpected supplier CONNECT")
		}
		r.Header.Set("Authorization", r.Header.Get("Proxy-Authorization"))
		username, key, ok := r.BasicAuth()
		id, route, verified := egress.VerifySupplierTicket(secret, key, host, time.Now())
		if !ok || !verified || username != egress.SupplierProxyUser || id != account || route != "global" {
			t.Error("invalid authenticated CONNECT scope")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer parent.Close()
	var server Server
	server.config.EgressControlURL, _ = url.Parse(parent.URL)
	server.config.EgressControlKey = secret
	transport, err := server.supplierTransport(account, host, egress.Resolution{Mode: "global"})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/v1/models", nil)
	request.Header.Set("Authorization", "Bearer provider-private-key")
	if _, err := supplierHTTP(transport).Do(request); err == nil || !called.Load() {
		t.Fatalf("CONNECT result: called=%v err=%v", called.Load(), err)
	}
}

func TestMixedConversationRootIsCallerScoped(t *testing.T) {
	parent := http.Header{"Thread-Id": {"root"}}
	child := http.Header{"Thread-Id": {"child"}, "X-Codex-Parent-Thread-Id": {"root"}}
	root := mixedConversation(parent, nil, "caller-key")
	if root == "" || root != mixedConversation(child, nil, "caller-key") || root == mixedConversation(parent, nil, "other-key") {
		t.Fatal("unexpected root scope")
	}
	payload := `{"prompt_cache_key":"root"}`
	body := &countingBody{replayBytes: int64(len(payload)), replay: func() io.Reader { return strings.NewReader(payload) }}
	if root != mixedConversation(nil, body, "caller-key") {
		t.Fatal("body affinity key lost")
	}
}

func TestMixedOfficialScopeUsesSingleAuthorizedAccount(t *testing.T) {
	const group, first, second = "e3c35297-dba1-4eef-bf43-e3f55fd385e4", "1234567890abcdef", "abcdef1234567890"
	layers := []gatewayproxy.AllocationGroup{{GroupID: group, AccountIDs: []string{first}}, {GroupID: gatewayproxy.PersonalLayerID, AllAccounts: true}}
	for _, test := range []struct{ account, scope string }{{first, group}, {second, mixedDispatchScopeID}} {
		pinned := mixedOfficialScope(layers, test.account)
		if len(pinned) != 1 || pinned[0].AllAccounts || pinned[0].GroupID != test.scope || !slices.Equal(pinned[0].AccountIDs, []string{test.account}) {
			t.Fatalf("unexpected dispatch scope: %+v", pinned)
		}
	}
}
