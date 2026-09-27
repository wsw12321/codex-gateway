package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func isAntigravityAccountRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/admin/antigravity-accounts") ||
		strings.HasPrefix(r.URL.Path, "/internal/antigravity-accounts/")
}

func (s *Server) accountStore(r *http.Request) *store.Store {
	if isAntigravityAccountRequest(r) {
		return s.store.WithUpstreamProvider(store.UpstreamProviderAntigravity)
	}
	return s.store
}

func (s *Server) accountClient(r *http.Request) *gatewayproxy.Client {
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
	s.mux.Handle("PUT /admin/antigravity-accounts/{id}/access", write(s.setUpstreamAccountAccess))
}

func (s *Server) requireAntigravityAccounts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.antigravity == nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "antigravity_not_configured", "尚未配置 Antigravity 桥接服务")
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
	ctx, cancel := context.WithTimeout(ctx, upstreamAccountSyncTimeout)
	defer cancel()
	accounts, err := s.antigravity.ListUpstreamAccounts(ctx)
	if err != nil {
		return err
	}
	snapshots := make([]store.UpstreamAccountSnapshot, 0, len(accounts))
	for _, account := range accounts {
		status := store.UpstreamAccountStatusUnavailable
		if upstreamAccountSourceStatusKnown(account) && account.Status == "available" {
			status = store.UpstreamAccountStatusAvailable
		}
		snapshots = append(snapshots, store.UpstreamAccountSnapshot{
			ID: account.ID, DisplayName: account.DisplayName, MaskedEmail: account.MaskedEmail,
			Plan: account.Plan, Status: status, LastSyncedAt: account.LastSyncedAt,
		})
	}
	return s.store.WithUpstreamProvider(store.UpstreamProviderAntigravity).SyncUpstreamAccounts(ctx, snapshots, time.Now().UTC())
}
