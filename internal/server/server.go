// Package server wires the identity, management and narrow Responses data
// planes into one HTTP service.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const sessionCookieName = "__Host-cg_session"

type Server struct {
	config   config.Config
	store    *store.Store
	identity *identity.Service
	upstream *gatewayproxy.Client
	logger   *slog.Logger
	mux      *http.ServeMux
	attempts *attemptLimiter
}

func New(cfg config.Config, repository *store.Store, logger *slog.Logger) (*Server, error) {
	if repository == nil {
		return nil, errors.New("server: store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	identityService, err := identity.New(repository, identity.Config{
		RPID: cfg.RPID, RPOrigins: cfg.RPOrigins,
		TokenPepper: cfg.TokenPepper,
		SessionIdle: cfg.SessionIdle, SessionMax: cfg.SessionMax,
		SecureCookies: !cfg.DevInsecure,
	})
	if err != nil {
		return nil, err
	}
	s := &Server{
		config: cfg, store: repository, identity: identityService,
		upstream: gatewayproxy.New(cfg.SidecarURL, cfg.SidecarToken),
		logger:   logger, mux: http.NewServeMux(), attempts: newAttemptLimiter(),
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	var handler http.Handler = s.mux
	handler = s.accessLog(handler)
	handler = httpx.Recover(s.logger, handler)
	handler = httpx.SecurityHeaders(handler)
	handler = httpx.RequestContext(s.config.TrustedProxy)(handler)
	return handler
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /", s.page)
	s.mux.HandleFunc("GET /join", s.page)
	s.mux.HandleFunc("GET /recover", s.page)
	s.mux.HandleFunc("GET /static/app.js", s.javascript)
	s.mux.HandleFunc("GET /static/style.css", s.stylesheet)
	s.mux.HandleFunc("GET /setup/configure-codex.sh", s.codexShellSetup)
	s.mux.HandleFunc("GET /setup/configure-codex.bat", s.codexWindowsSetup)

	s.publicPOST("/auth/login/begin", s.beginLogin)
	s.publicPOST("/auth/login/finish", s.finishLogin)
	s.publicPOST("/auth/register/begin", s.beginRegistration)
	s.publicPOST("/auth/register/finish", s.finishRegistration)
	s.publicPOST("/auth/recovery/begin", s.beginRecovery)
	s.publicPOST("/auth/recovery/finish", s.finishRecovery)

	s.browserPOST("/auth/logout", s.requireSession(http.HandlerFunc(s.logout)))
	s.browserPOST("/auth/reauth/begin", s.requireSession(http.HandlerFunc(s.beginReauthentication)))
	s.browserPOST("/auth/reauth/finish", s.requireSession(http.HandlerFunc(s.finishReauthentication)))
	s.browserPOST("/admin/devices", s.requireSession(http.HandlerFunc(s.createDevice)))
	s.browserPOST("/admin/projects", s.requireSession(http.HandlerFunc(s.createProject)))
	s.browserPOST("/admin/api-keys", s.requireRecentVerification(http.HandlerFunc(s.createAPIKey)))
	s.browserPOST("/admin/passkeys/begin", s.requireRecentVerification(http.HandlerFunc(s.beginAddPasskey)))
	s.browserPOST("/admin/passkeys/finish", s.requireRecentVerification(http.HandlerFunc(s.finishAddPasskey)))
	s.browserPOST("/admin/invitations", s.requireRecentVerification(s.ownerOnly(http.HandlerFunc(s.createInvitation))))

	s.mux.Handle("DELETE /admin/api-keys/{id}", s.browserOrigin(s.requireRecentVerification(http.HandlerFunc(s.revokeAPIKey))))
	s.mux.Handle("GET /admin/state", s.requireSession(http.HandlerFunc(s.adminState)))
	s.mux.Handle("GET /admin/usage", s.requireSession(http.HandlerFunc(s.usageJSON)))
	s.mux.Handle("GET /admin/usage.csv", s.requireSession(http.HandlerFunc(s.usageCSV)))
	s.mux.Handle("GET /admin/usage/global", s.requireSession(s.ownerOnly(http.HandlerFunc(s.globalUsageJSON))))
	s.mux.Handle("GET /admin/alerts", s.requireSession(s.ownerOnly(http.HandlerFunc(s.alertsJSON))))
	s.mux.Handle("GET /admin/billing/me", s.requireSession(http.HandlerFunc(s.billingMe)))
	s.mux.Handle("GET /admin/billing/settings", s.requireSession(s.ownerOnly(http.HandlerFunc(s.billingSettings))))
	s.mux.Handle("GET /admin/billing/users", s.requireSession(s.ownerOnly(http.HandlerFunc(s.billingUsers))))
	s.mux.Handle("GET /admin/billing/users/{user_id}", s.requireSession(s.ownerOnly(http.HandlerFunc(s.billingUser))))
	s.mux.Handle("PUT /admin/billing/settings/recharge-rate", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(http.HandlerFunc(s.updateRechargeRate)))))
	s.mux.Handle("POST /admin/billing/users/{user_id}/recharges", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(http.HandlerFunc(s.rechargeBillingUser)))))
	s.mux.Handle("POST /admin/billing/users/{user_id}/adjustments", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(http.HandlerFunc(s.adjustBillingUser)))))
	s.mux.Handle("PUT /admin/billing/users/{user_id}/subscriptions/{tier}", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(http.HandlerFunc(s.putBillingSubscription)))))
	s.mux.Handle("DELETE /admin/billing/users/{user_id}/subscriptions/{tier}", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(http.HandlerFunc(s.deleteBillingSubscription)))))

	s.mux.Handle("GET /v1/models", s.requireAPIKey(http.HandlerFunc(s.proxyModels)))
	s.mux.Handle("POST /v1/responses", s.requireAPIKey(http.HandlerFunc(s.proxyResponses)))
	s.mux.Handle("POST /v1/responses/compact", s.requireAPIKey(http.HandlerFunc(s.proxyCompact)))
}

func (s *Server) publicPOST(path string, handler http.HandlerFunc) {
	s.mux.Handle("POST "+path, s.browserOrigin(s.limitBrowserAttempts(http.HandlerFunc(handler))))
}

func (s *Server) browserPOST(path string, handler http.Handler) {
	s.mux.Handle("POST "+path, s.browserOrigin(handler))
}

func (s *Server) browserOrigin(next http.Handler) http.Handler {
	return httpx.RequireBrowserOrigin(s.config.RPOrigins, httpx.NoStore(next))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "database_unavailable", "数据库尚未就绪")
		return
	}
	s.health(w, r)
}

type attemptEntry struct {
	window time.Time
	count  int
}

type attemptLimiter struct {
	mu      sync.Mutex
	entries map[string]attemptEntry
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{entries: make(map[string]attemptEntry)}
}

func (l *attemptLimiter) allow(key string, limit int, window time.Duration) (bool, int) {
	now := time.Now()
	bucket := now.Truncate(window)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if !entry.window.Equal(bucket) {
		entry = attemptEntry{window: bucket}
	}
	if entry.count >= limit {
		retry := int(time.Until(bucket.Add(window)).Seconds()) + 1
		return false, max(retry, 1)
	}
	entry.count++
	l.entries[key] = entry
	if len(l.entries) > 20_000 {
		for existingKey, existing := range l.entries {
			if existing.window.Before(bucket) {
				delete(l.entries, existingKey)
			}
		}
	}
	return true, 0
}

func (s *Server) limitBrowserAttempts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := httpx.ClientIP(r.Context())
		key := r.URL.Path + ":unknown"
		if ip != nil {
			key = r.URL.Path + ":" + ip.String()
		}
		if ok, retry := s.attempts.allow(key, 20, time.Minute); !ok {
			w.Header().Set("Retry-After", integerString(retry))
			httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", "authentication_rate_limited", "尝试次数过多，请稍后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}
