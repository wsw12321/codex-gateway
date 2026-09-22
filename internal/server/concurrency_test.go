package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestUpstreamConcurrencyOwnerOnlyAndNoStore(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayproxy.UpstreamConcurrency{SampledAt: time.Now().UTC(), Accounts: []gatewayproxy.UpstreamAccountConcurrency{{ID: "0123456789abcdef", ActiveRequests: 2}}})
	}))
	defer remote.Close()
	base, _ := url.Parse(remote.URL)
	server := &Server{upstream: gatewayproxy.NewWithHTTPClient(base, "secret", remote.Client())}
	for _, role := range []string{store.UserRoleMember, store.UserRoleOwner} {
		request := httptest.NewRequest(http.MethodGet, "/admin/upstream-accounts/concurrency", nil)
		request = request.WithContext(context.WithValue(request.Context(), userContextKey, store.User{ID: "user-1", Role: role}))
		response := httptest.NewRecorder()
		server.ownerOnly(http.HandlerFunc(server.upstreamAccountConcurrency)).ServeHTTP(response, request)
		if role == store.UserRoleMember {
			if response.Code != http.StatusForbidden || calls != 0 {
				t.Fatalf("member status=%d calls=%d", response.Code, calls)
			}
		} else if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"active_requests":2`) || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("owner status=%d body=%s", response.Code, response.Body.String())
		}
	}
}
