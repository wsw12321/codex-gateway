package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func isAntigravityAccountRequest(r *http.Request) bool {
	if scope, ok := requestAccountScope(r); ok {
		return scope.provider == store.UpstreamProviderAntigravity
	}
	return strings.HasPrefix(r.URL.Path, "/admin/antigravity-accounts") ||
		strings.HasPrefix(r.URL.Path, "/internal/antigravity-accounts/")
}

// accountStore is the store for this request's provider, scoped to the
// tenant of a /admin/me/ route; Owner routes manage platform accounts.
func (s *Server) accountStore(r *http.Request) *store.Store {
	repository := s.store
	if isAnthropicAccountRequest(r) {
		repository = s.store.WithUpstreamProvider(store.UpstreamProviderAnthropic)
	} else if isAntigravityAccountRequest(r) {
		repository = s.store.WithUpstreamProvider(store.UpstreamProviderAntigravity)
	}
	if scope, ok := requestAccountScope(r); ok && scope.tenant != "" {
		repository = repository.WithUpstreamOwner(scope.tenant)
	}
	return repository
}

// egressStore manages egress nodes and registrations of this request's
// owner: a tenant on /admin/me/ routes, the platform otherwise.
func (s *Server) egressStore(r *http.Request) *store.Store {
	if scope, ok := requestAccountScope(r); ok && scope.tenant != "" {
		return s.store.WithUpstreamOwner(scope.tenant)
	}
	return s.store
}

func (s *Server) accountClient(r *http.Request) *gatewayproxy.Client {
	if isAnthropicAccountRequest(r) {
		return s.anthropic
	}
	if isAntigravityAccountRequest(r) {
		return s.antigravity
	}
	return s.upstream
}

func (s *Server) antigravityAccountRoutes() {
	read := func(next http.HandlerFunc) http.Handler {
		return s.requireSession(s.ownerOnly(s.requireAntigravityAccounts(next)))
	}
	write := func(next http.HandlerFunc) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(s.ownerOnly(s.requireAntigravityAccounts(next))))
	}
	s.mux.Handle("GET /admin/antigravity-accounts", read(s.upstreamAccountsJSON))
	s.mux.Handle("GET /admin/antigravity-accounts/concurrency", read(s.upstreamAccountConcurrency))
	s.mux.Handle("PUT /admin/antigravity-accounts/{id}/status", write(s.setUpstreamAccountStatus))
	s.mux.Handle("PUT /admin/antigravity-accounts/{id}/allocation-weight", write(s.setUpstreamAccountAllocationWeight))
	s.mux.Handle("PUT /admin/antigravity-accounts/{id}/concurrent-limit", write(s.setUpstreamAccountConcurrentLimit))
	s.mux.Handle("PUT /admin/antigravity-accounts/{id}/alias", write(s.setUpstreamAccountAlias))
	s.mux.Handle("PUT /admin/antigravity-accounts/{id}/access", write(s.setUpstreamAccountAccess))
}

func (s *Server) requireAntigravityAccounts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.antigravity == nil && !s.config.SuppliersEnabled {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "antigravity_not_configured", "尚未配置 Antigravity 上游")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Register accounts before dispatch so a freshly added account can be selected
// without opening the dashboard first. Only non-secret, validated metadata
// crosses this boundary. Failed synchronization never falls back to Codex.
func (s *Server) syncAntigravityAccounts(ctx context.Context) error {
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	return s.syncAntigravityAccountsLocked(ctx)
}

// Catalog requests may omit Gemini while another request refreshes accounts.
// Never wait for that refresh: a context deadline cannot cancel Mutex.Lock.
func (s *Server) trySyncAntigravityAccounts(ctx context.Context) error {
	if !s.upstreamAccountSyncMu.TryLock() {
		return errors.New("Antigravity account synchronization is busy")
	}
	defer s.upstreamAccountSyncMu.Unlock()
	return s.syncAntigravityAccountsLocked(ctx)
}

// syncAntigravityAccountsLocked requires upstreamAccountSyncMu to be held.
func (s *Server) syncAntigravityAccountsLocked(ctx context.Context) error {
	return s.syncUpstreamAccountsLocked(ctx, store.UpstreamProviderAntigravity)
}
