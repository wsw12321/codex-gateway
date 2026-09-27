package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestAntigravityAccountCallbacksHaveIndependentAuthentication(t *testing.T) {
	s := &Server{config: config.Config{SidecarToken: "codex-secret", AntigravityBridgeToken: "agy-secret"}, mux: http.NewServeMux()}
	s.routes()
	for _, endpoint := range []string{"select", "eligible"} {
		for _, provider := range []string{"upstream", "antigravity"} {
			for _, token := range []string{"codex-secret", "agy-secret", ""} {
				r := httptest.NewRequest(http.MethodPost, "/internal/"+provider+"-accounts/"+endpoint, strings.NewReader(`{}`))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				s.mux.ServeHTTP(w, r)
				want := http.StatusUnauthorized
				if (provider == "upstream" && token == "codex-secret") || (provider == "antigravity" && token == "agy-secret") {
					want = http.StatusBadRequest // Authenticated; malformed candidate protocol.
				}
				if w.Code != want {
					t.Fatalf("%s/%s token=%q status=%d want=%d", provider, endpoint, token, w.Code, want)
				}
			}
		}
	}
}

func TestAntigravityAccountRoutesRequireSession(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), config: config.Config{RPOrigins: []string{"https://gateway.test"}}}
	s.routes()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, ""}, {http.MethodGet, "/concurrency"},
		{http.MethodPut, "/0123456789abcdef/status"},
		{http.MethodPut, "/0123456789abcdef/allocation-weight"},
		{http.MethodPut, "/0123456789abcdef/concurrent-limit"},
		{http.MethodPut, "/0123456789abcdef/access"},
	} {
		r := httptest.NewRequest(route.method, "https://gateway.test/admin/antigravity-accounts"+route.path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://gateway.test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d body=%s", route.method, route.path, w.Code, w.Body)
		}
	}
}

func TestAntigravityAccountManagementUnconfigured(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.requireAntigravityAccounts(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unconfigured management reached handler")
	})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/antigravity-accounts", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "antigravity_not_configured") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}
