package server

import (
	"context"
	"encoding/json"
	"fmt"
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
	sampledAt := time.Now().UTC()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		// Match the sidecar wire format rather than the public dashboard DTO.
		_, _ = fmt.Fprintf(w, `{"sampled_at":%q,"accounts":[{"id":"0123456789abcdef","active_requests":0,"concurrent_limit":1},{"id":"fedcba9876543210","active_requests":2,"concurrent_limit":3}]}`, sampledAt.Format(time.RFC3339Nano))
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
		} else {
			if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || calls != 1 {
				t.Fatalf("owner status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "concurrent_limit") {
				t.Fatalf("internal concurrency limit leaked: %s", response.Body.String())
			}
			var snapshot gatewayproxy.UpstreamConcurrency
			if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
				t.Fatal(err)
			}
			if !snapshot.SampledAt.Equal(sampledAt) || len(snapshot.Accounts) != 2 ||
				snapshot.Accounts[0] != (gatewayproxy.UpstreamAccountConcurrency{ID: "0123456789abcdef", ActiveRequests: 0}) ||
				snapshot.Accounts[1] != (gatewayproxy.UpstreamAccountConcurrency{ID: "fedcba9876543210", ActiveRequests: 2}) {
				t.Fatalf("unexpected dashboard snapshot: %+v", snapshot)
			}
		}
	}
}
