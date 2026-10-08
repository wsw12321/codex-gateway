package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

func isAnthropicAccountRequest(r *http.Request) bool {
	return r.URL.Path == "/admin/anthropic-accounts" || strings.HasPrefix(r.URL.Path, "/admin/anthropic-accounts/") || strings.HasPrefix(r.URL.Path, "/internal/anthropic-accounts/")
}

func (s *Server) anthropicAccountRoutes() {
	s.mux.HandleFunc("POST /internal/anthropic-accounts/select", s.selectUpstreamAccount)
	s.mux.HandleFunc("POST /internal/anthropic-accounts/eligible", s.eligibleUpstreamAccounts)
	read := func(h http.HandlerFunc) http.Handler {
		return s.requireSession(s.ownerOnly(s.requireAnthropicAccounts(h)))
	}
	write := func(h http.HandlerFunc) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(s.ownerOnly(s.requireAnthropicAccounts(h))))
	}
	s.mux.Handle("GET /admin/anthropic-accounts", read(s.upstreamAccountsJSON))
	s.mux.Handle("GET /admin/anthropic-accounts/concurrency", read(s.upstreamAccountConcurrency))
	s.mux.Handle("PUT /admin/anthropic-accounts/{id}/status", write(s.setUpstreamAccountStatus))
	s.mux.Handle("PUT /admin/anthropic-accounts/{id}/allocation-weight", write(s.setUpstreamAccountAllocationWeight))
	s.mux.Handle("PUT /admin/anthropic-accounts/{id}/concurrent-limit", write(s.setUpstreamAccountConcurrentLimit))
	s.mux.Handle("PUT /admin/anthropic-accounts/{id}/access", write(s.setUpstreamAccountAccess))
}

func (s *Server) requireAnthropicAccounts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.anthropic == nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "anthropic_not_configured", "Claude 账号服务尚未就绪")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) syncAnthropicAccounts(ctx context.Context) error {
	// Waiting for an unrelated account refresh must remain cancellable; a
	// disconnected client must not keep its request and concurrency lease alive.
	locked := s.upstreamAccountSyncMu.TryLock()
	if !locked {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for !locked {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				locked = s.upstreamAccountSyncMu.TryLock()
			}
		}
	}
	defer s.upstreamAccountSyncMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.syncUpstreamAccountsLocked(ctx, store.UpstreamProviderAnthropic)
}
