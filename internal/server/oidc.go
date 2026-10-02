package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

const oidcCookieName = "__Host-cg_oidc"
const oidcFlowTTL = 10 * time.Minute
const oidcVerificationAge = 5 * time.Minute
const maxOIDCFlows = 1024

// All flow credentials live only in this bounded, process-local store. The
// cookie binds both login and linking to the browser that initiated the flow.
type oidcFlow struct {
	Kind                   string
	Browser                [32]byte
	UserID, SessionID      string
	Nonce, Verifier        string
	Identity               identity.OIDCIdentity
	Expires, VerifiedUntil time.Time
	key                    [32]byte
	consumed               bool
	lifecycle              *oidcFlowLifecycle
}

// Retain cancellation and issued-session metadata until the original flow TTL,
// including after take. A late response can otherwise restore a login after
// another tab logs in or out. No session token is retained here.
type oidcFlowLifecycle struct {
	mu              sync.Mutex
	cancelled       atomic.Bool
	issuedSessionID string
}

func (flow oidcFlow) active(now time.Time) bool {
	return flow.lifecycle != nil && !flow.lifecycle.cancelled.Load() && flow.Expires.After(now)
}

type oidcFlowStore struct {
	mu       sync.Mutex
	entries  map[[32]byte]oidcFlow
	capacity int
}

func newOIDCFlowStore(capacity int) *oidcFlowStore {
	return &oidcFlowStore{entries: make(map[[32]byte]oidcFlow), capacity: capacity}
}
func oidcRandom() string {
	var b [32]byte
	// crypto/rand.Read cannot fail on supported Go platforms.
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (f *oidcFlowStore) put(flow oidcFlow, now time.Time) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, entry := range f.entries {
		if !entry.Expires.After(now) {
			delete(f.entries, key)
		}
	}
	if len(f.entries) >= f.capacity || !flow.Expires.After(now) {
		return "", false
	}
	key := oidcRandom()
	flow.key = sha256.Sum256([]byte(key))
	if flow.lifecycle == nil {
		flow.lifecycle = &oidcFlowLifecycle{}
	}
	f.entries[flow.key] = flow
	return key, true
}
func (f *oidcFlowStore) take(key, browser string, now time.Time) (oidcFlow, bool) {
	if len(key) != 43 || len(browser) != 43 {
		return oidcFlow{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	digest := sha256.Sum256([]byte(key))
	flow, ok := f.entries[digest]
	if !ok {
		return oidcFlow{}, false
	}
	if !flow.Expires.After(now) {
		delete(f.entries, digest)
		return oidcFlow{}, false
	}
	if flow.consumed || !flow.active(now) || flow.Browser != sha256.Sum256([]byte(browser)) {
		return oidcFlow{}, false
	}
	flow.consumed = true
	// The handler keeps its one-use copy; the retained tombstone does not need
	// the provider's exchange secrets.
	retained := flow
	retained.Nonce, retained.Verifier = "", ""
	f.entries[digest] = retained
	return flow, true
}
func (f *oidcFlowStore) discardBrowser(browser string) []string {
	if browser == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(browser))
	f.mu.Lock()
	var lifecycles []*oidcFlowLifecycle
	for _, flow := range f.entries {
		if flow.Browser == digest {
			lifecycles = append(lifecycles, flow.lifecycle)
		}
	}
	f.mu.Unlock()
	var sessions []string
	for _, lifecycle := range lifecycles {
		// Final issuance holds this per-flow lock until its response headers
		// are set. Cancellation then sees and revokes any issued session.
		lifecycle.mu.Lock()
		lifecycle.cancelled.Store(true)
		if lifecycle.issuedSessionID != "" {
			sessions = append(sessions, lifecycle.issuedSessionID)
		}
		lifecycle.mu.Unlock()
	}
	return sessions
}

// confirmation rotates the authorization key without losing its cancellation
// state or extending its lifetime. Caller holds the flow's lifecycle lock.
func (f *oidcFlowStore) confirmation(flow oidcFlow, now time.Time) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prior, ok := f.entries[flow.key]
	if !ok || !prior.consumed || prior.lifecycle != flow.lifecycle || !flow.active(now) {
		return "", false
	}
	delete(f.entries, flow.key)
	key := oidcRandom()
	flow.key, flow.consumed = sha256.Sum256([]byte(key)), false
	f.entries[flow.key] = flow
	return key, true
}

type oidcProvider interface {
	AuthorizationURL(context.Context, string, string, string) (string, error)
	Exchange(context.Context, string, string, string) (identity.OIDCIdentity, error)
}
type oidcRepository interface {
	GetActiveSession(context.Context, []byte, time.Time) (store.Session, error)
	GetUser(context.Context, string) (store.User, error)
	GetExternalIdentity(context.Context, string) (store.ExternalIdentity, error)
	LinkExternalIdentity(context.Context, store.LinkExternalIdentityParams) (store.ExternalIdentity, error)
	UnlinkExternalIdentity(context.Context, string, string, time.Time) (store.ExternalIdentity, error)
	RevokeSession(context.Context, string, string, time.Time) error
	AppendAuditEvent(context.Context, store.AppendAuditEventParams) (store.AuditEvent, error)
}

func (s *Server) oidcRepository() oidcRepository {
	if s.oidcRepo != nil {
		return s.oidcRepo
	}
	return s.store
}
func (s *Server) oidcRoutes() {
	s.mux.HandleFunc("GET /auth/oidc/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": s.config.OIDCEnabled})
	})
	s.mux.HandleFunc("GET /auth/oidc/callback", s.oidcCallback)
	s.mux.HandleFunc("GET /static/oidc-callback.js", s.oidcCallbackJavascript)
	s.publicPOST("/auth/oidc/login", s.beginOIDCLogin)
	s.publicPOST("/auth/oidc/complete", s.completeOIDC)
	s.browserPOST("/admin/identity-link/begin", s.requireRecentVerification(http.HandlerFunc(s.beginIdentityLink)))
	s.browserPOST("/admin/identity-link/confirm", s.requireRecentVerification(http.HandlerFunc(s.confirmIdentityLink)))
	s.browserPOST("/admin/identity-link/cancel", s.requireSession(http.HandlerFunc(s.cancelIdentityLink)))
	s.mux.Handle("DELETE /admin/identity-link", s.browserOrigin(s.requireRecentVerification(http.HandlerFunc(s.unlinkIdentity))))
}
func (s *Server) oidcAvailable(w http.ResponseWriter, r *http.Request) bool {
	if !s.config.OIDCEnabled || s.oidc == nil {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "authentication_error", "oidc_disabled", "吾水阁账号登录暂未启用")
		return false
	}
	return true
}
func oidcBrowser(r *http.Request) string {
	cookie, err := r.Cookie(oidcCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}
func (s *Server) setOIDCCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: oidcCookieName, Value: value, Path: "/", MaxAge: maxAge,
		Secure: !s.config.DevInsecure, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) cancelOIDCFlows(r *http.Request) error {
	if s.oidcFlows == nil {
		return nil
	}
	for _, sessionID := range s.oidcFlows.discardBrowser(oidcBrowser(r)) {
		if err := s.revokeOIDCSession(r, sessionID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) revokeOIDCSession(r *http.Request, sessionID string) error {
	// A disconnected browser must not abandon revocation of a late response.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	err := s.oidcRepository().RevokeSession(ctx, sessionID, "oidc_browser_changed", time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
func (s *Server) oidcAudit(r *http.Request, flow oidcFlow, operation string, success bool) {
	_, _ = s.oidcRepository().AppendAuditEvent(r.Context(), store.AppendAuditEventParams{
		ActorUserID: flow.UserID, ActorSessionID: flow.SessionID, EventType: "identity." + operation,
		Severity: "info", Success: success, SourceIP: safeIP(r.Context()), RequestID: httpx.RequestID(r.Context()),
		Metadata: map[string]any{"method": "oidc"},
	})
}
func (s *Server) rejectOIDC(w http.ResponseWriter, r *http.Request, flow oidcFlow) {
	s.oidcAudit(r, flow, "oidc_rejected", false)
	httpx.WriteError(w, r, http.StatusBadRequest, "authentication_error", "oidc_rejected", "授权流程无效或已过期，请返回网关重新开始。绑定需要保持原账号登录，且在本地身份验证后 5 分钟内确认。")
}

// Optional authentication is fail-closed on storage errors. An old or malformed
// cookie is not a logged-in account, and can be replaced by a successful login.
func (s *Server) oidcCurrentSession(r *http.Request) (store.Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if errors.Is(err, http.ErrNoCookie) {
		return store.Session{}, nil
	}
	if err != nil {
		return store.Session{}, err
	}
	raw, err := security.DigestOpaqueToken(security.SessionToken, cookie.Value)
	if err != nil {
		return store.Session{}, nil
	}
	digest, err := security.PepperTokenDigest(s.config.TokenPepper, raw)
	if err != nil {
		return store.Session{}, err
	}
	session, err := s.oidcRepository().GetActiveSession(r.Context(), digest[:], time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, nil
	}
	return session, err
}
func (s *Server) oidcRequireLoggedOut(w http.ResponseWriter, r *http.Request) bool {
	session, err := s.oidcCurrentSession(r)
	if err != nil {
		internalError(s, w, r, "check OIDC login session", err)
		return false
	}
	if session.ID != "" {
		httpx.WriteError(w, r, http.StatusConflict, "authentication_error", "oidc_logout_required", "请先退出当前网关账号，再使用吾水阁账号登录")
		return false
	}
	return true
}
func (s *Server) beginOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) || !s.oidcRequireLoggedOut(w, r) {
		return
	}
	s.beginOIDC(w, r, oidcFlow{Kind: "login"})
}
func (s *Server) beginIdentityLink(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) {
		return
	}
	session := sessionFrom(r.Context())
	if !oidcRecent(session, time.Now()) {
		s.rejectOIDC(w, r, oidcFlow{})
		return
	}
	_, err := s.oidcRepository().GetExternalIdentity(r.Context(), session.UserID)
	if err == nil {
		httpx.WriteError(w, r, http.StatusConflict, "authentication_error", "identity_already_linked", "该网关账号已绑定吾水阁账号，请先解绑")
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		internalError(s, w, r, "read external identity", err)
		return
	}
	s.beginOIDC(w, r, oidcFlow{Kind: "link", UserID: session.UserID, SessionID: session.ID, VerifiedUntil: session.RecentlyVerifiedAt.Add(oidcVerificationAge)})
}
func (s *Server) beginOIDC(w http.ResponseWriter, r *http.Request, flow oidcFlow) {
	if err := s.cancelOIDCFlows(r); err != nil {
		internalError(s, w, r, "cancel previous OIDC login", err)
		return
	}
	browser := oidcRandom()
	flow.lifecycle = &oidcFlowLifecycle{}
	flow.Browser = sha256.Sum256([]byte(browser))
	flow.Nonce, flow.Verifier = oidcRandom(), oidcRandom()
	flow.Expires = time.Now().UTC().Add(oidcFlowTTL)
	// One outstanding browser transaction; opening another tab invalidates the old one.
	key, ok := s.oidcFlows.put(flow, time.Now())
	if !ok {
		httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", "oidc_busy", "授权流程繁忙，请稍后重试")
		return
	}
	target, err := s.oidc.AuthorizationURL(r.Context(), key, flow.Nonce, flow.Verifier)
	if err != nil {
		s.oidcFlows.discardBrowser(browser)
		s.oidcAudit(r, flow, "oidc_unavailable", false)
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "authentication_error", "oidc_unavailable", "账号中心暂时不可用，请稍后重试；仍可使用密码或 Passkey 登录")
		return
	}
	flow.lifecycle.mu.Lock()
	defer flow.lifecycle.mu.Unlock()
	if !flow.active(time.Now()) {
		s.rejectOIDC(w, r, flow)
		return
	}
	s.setOIDCCookie(w, browser, int(oidcFlowTTL.Seconds()))
	writeJSON(w, http.StatusOK, map[string]string{"authorization_url": target})
}
func oidcRecent(session store.Session, now time.Time) bool {
	return session.RecentlyVerifiedAt != nil && !session.RecentlyVerifiedAt.After(now) && session.RecentlyVerifiedAt.Add(oidcVerificationAge).After(now)
}
func (s *Server) oidcOriginalSession(r *http.Request, flow oidcFlow) (store.User, bool) {
	session, err := s.oidcCurrentSession(r)
	now := time.Now().UTC()
	if err != nil || session.ID != flow.SessionID || session.UserID != flow.UserID || !oidcRecent(session, now) || !flow.VerifiedUntil.After(now) {
		return store.User{}, false
	}
	user, err := s.oidcRepository().GetUser(r.Context(), flow.UserID)
	return user, err == nil && user.Status == store.StatusActive
}
func (s *Server) completeOIDC(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) {
		return
	}
	var input struct {
		State string `json:"state"`
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := decodeJSON(w, r, &input, 16<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	browser := oidcBrowser(r)
	flow, ok := s.oidcFlows.take(input.State, browser, time.Now())
	if !ok || (flow.Kind != "login" && flow.Kind != "link") || input.Error != "" || input.Code == "" || len(input.Code) > 8192 {
		s.rejectOIDC(w, r, flow)
		return
	}
	if flow.Kind == "login" {
		if !s.oidcRequireLoggedOut(w, r) {
			return
		}
	} else if _, ok := s.oidcOriginalSession(r, flow); !ok {
		s.rejectOIDC(w, r, flow)
		return
	}
	external, err := s.oidc.Exchange(r.Context(), input.Code, flow.Nonce, flow.Verifier)
	if err != nil {
		s.rejectOIDC(w, r, flow)
		return
	}
	// Provider I/O runs without a lock. Final database mutation and response
	// serialize with local login/logout for this browser transaction only.
	flow.lifecycle.mu.Lock()
	defer flow.lifecycle.mu.Unlock()
	if !flow.active(time.Now()) {
		s.rejectOIDC(w, r, flow)
		return
	}
	if flow.Kind == "login" {
		if !s.oidcRequireLoggedOut(w, r) {
			return
		}
		result, err := s.identity.LoginExternal(r.Context(), external, httpx.ClientIP(r.Context()), r.UserAgent())
		if errors.Is(err, store.ErrNotFound) {
			s.oidcAudit(r, flow, "oidc_login_rejected", false)
			httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "oidc_account_unavailable", "此吾水阁账号尚未绑定可用的网关账号。请先使用原密码或 Passkey 登录网关，并在账号安全中绑定。")
			return
		}
		if err != nil {
			internalError(s, w, r, "create external login session", err)
			return
		}
		flow.lifecycle.issuedSessionID = result.SessionID
		if !flow.active(time.Now()) || r.Context().Err() != nil {
			flow.lifecycle.cancelled.Store(true)
			if err := s.revokeOIDCSession(r, result.SessionID); err != nil {
				internalError(s, w, r, "revoke expired OIDC login", err)
				return
			}
			s.rejectOIDC(w, r, flow)
			return
		}
		s.setOIDCCookie(w, "", -1)
		s.writeSessionCookie(w, result.SessionToken)
		flow.UserID = result.User.ID
		s.oidcAudit(r, flow, "login", true)
		writeJSON(w, http.StatusOK, map[string]string{"result": "login"})
		return
	}
	user, ok := s.oidcOriginalSession(r, flow)
	if !ok {
		s.rejectOIDC(w, r, flow)
		return
	}
	flow.Kind, flow.Identity = "confirm", external
	flow.Nonce, flow.Verifier = "", ""
	key, ok := s.oidcFlows.confirmation(flow, time.Now())
	if !ok {
		s.rejectOIDC(w, r, flow)
		return
	}
	s.setOIDCCookie(w, browser, int(time.Until(flow.Expires).Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"result": "confirm", "flow_id": key, "user": publicUser(user), "masked_email": external.MaskedEmail, "expires_at": flow.VerifiedUntil})
}
func (s *Server) takeIdentityConfirmation(w http.ResponseWriter, r *http.Request) (oidcFlow, bool) {
	var input struct {
		FlowID string `json:"flow_id"`
	}
	if err := decodeJSON(w, r, &input, 1024); err != nil {
		badJSON(w, r, err)
		return oidcFlow{}, false
	}
	flow, ok := s.oidcFlows.take(input.FlowID, oidcBrowser(r), time.Now())
	session := sessionFrom(r.Context())
	if !ok || flow.Kind != "confirm" || flow.UserID != session.UserID || flow.SessionID != session.ID {
		s.rejectOIDC(w, r, flow)
		return oidcFlow{}, false
	}
	return flow, true
}
func (s *Server) confirmIdentityLink(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) {
		return
	}
	flow, ok := s.takeIdentityConfirmation(w, r)
	if !ok {
		return
	}
	flow.lifecycle.mu.Lock()
	defer flow.lifecycle.mu.Unlock()
	if !flow.active(time.Now()) {
		s.rejectOIDC(w, r, flow)
		return
	}
	if _, ok := s.oidcOriginalSession(r, flow); !ok {
		s.rejectOIDC(w, r, flow)
		return
	}
	link, err := s.oidcRepository().LinkExternalIdentity(r.Context(), store.LinkExternalIdentityParams{
		UserID: flow.UserID, SessionID: flow.SessionID, Issuer: flow.Identity.Issuer, Subject: flow.Identity.Subject, MaskedEmail: flow.Identity.MaskedEmail, At: time.Now().UTC(), VerificationExpiresAt: flow.VerifiedUntil,
	})
	if errors.Is(err, store.ErrConflict) {
		s.oidcAudit(r, flow, "link_rejected", false)
		httpx.WriteError(w, r, http.StatusConflict, "authentication_error", "identity_already_linked", "网关账号或吾水阁账号已绑定，无法重复绑定")
		return
	}
	if err != nil {
		s.rejectOIDC(w, r, flow)
		return
	}
	s.oidcAudit(r, flow, "linked", true)
	s.setOIDCCookie(w, "", -1)
	writeJSON(w, http.StatusOK, externalIdentityView(true, link))
}
func (s *Server) cancelIdentityLink(w http.ResponseWriter, r *http.Request) {
	flow, ok := s.takeIdentityConfirmation(w, r)
	if !ok {
		return
	}
	flow.lifecycle.mu.Lock()
	defer flow.lifecycle.mu.Unlock()
	if !flow.active(time.Now()) {
		s.rejectOIDC(w, r, flow)
		return
	}
	flow.lifecycle.cancelled.Store(true)
	s.oidcAudit(r, flow, "link_cancelled", true)
	s.setOIDCCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
func (s *Server) unlinkIdentity(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	link, err := s.oidcRepository().UnlinkExternalIdentity(r.Context(), session.UserID, session.ID, time.Now().UTC())
	if err != nil {
		s.rejectOIDC(w, r, oidcFlow{UserID: session.UserID, SessionID: session.ID})
		return
	}
	if err := s.cancelOIDCFlows(r); err != nil {
		internalError(s, w, r, "cancel OIDC login after unlink", err)
		return
	}
	s.setOIDCCookie(w, "", -1)
	loggedOut := session.ExternalIdentityID != nil && *session.ExternalIdentityID == link.ID
	if loggedOut {
		s.clearSessionCookie(w)
	}
	s.oidcAudit(r, oidcFlow{UserID: session.UserID, SessionID: session.ID}, "unlinked", true)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "logged_out": loggedOut})
}

type adminExternalIdentity struct {
	Enabled     bool       `json:"enabled"`
	Linked      bool       `json:"linked"`
	MaskedEmail string     `json:"masked_email"`
	LinkedAt    *time.Time `json:"linked_at"`
}

func externalIdentityView(enabled bool, link store.ExternalIdentity) adminExternalIdentity {
	view := adminExternalIdentity{Enabled: enabled, Linked: link.ID != "", MaskedEmail: link.MaskedEmail}
	if view.Linked {
		view.LinkedAt = &link.LinkedAt
	}
	return view
}
