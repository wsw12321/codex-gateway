package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/store"
)

// The client can only consume server-held identities, never supply account
// metadata or an identity to create. A failed attempt is not retryable.
func (s *Server) takeOIDCRegistration(w http.ResponseWriter, r *http.Request) (oidcFlow, bool) {
	var input struct {
		FlowID string `json:"flow_id"`
	}
	if err := decodeJSON(w, r, &input, 1024); err != nil {
		badJSON(w, r, err)
		return oidcFlow{}, false
	}
	flow, ok := s.oidcFlows.take(input.FlowID, oidcBrowser(r), time.Now())
	if !ok || flow.Kind != "register" {
		s.rejectOIDC(w, r, flow)
		return oidcFlow{}, false
	}
	return flow, true
}

func (s *Server) registerOIDC(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) {
		return
	}
	flow, ok := s.takeOIDCRegistration(w, r)
	if !ok {
		return
	}
	flow.lifecycle.mu.Lock()
	defer flow.lifecycle.mu.Unlock()
	if !flow.active(time.Now()) {
		s.rejectOIDC(w, r, flow)
		return
	}
	if !s.oidcRequireLoggedOut(w, r) {
		return
	}
	result, err := s.identity.RegisterExternal(r.Context(), flow.Identity, flow.VerifiedAt, httpx.ClientIP(r.Context()), r.UserAgent())
	if errors.Is(err, store.ErrConflict) {
		s.oidcAudit(r, flow, "registration_rejected", false)
		httpx.WriteError(w, r, http.StatusConflict, "authentication_error", "oidc_registration_conflict", "此吾水阁账号的绑定状态已改变，请重新使用吾水阁账号登录。")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		s.rejectOIDC(w, r, flow)
		return
	}
	if err != nil {
		s.oidcAudit(r, flow, "registration_rejected", false)
		internalError(s, w, r, "register external account", err)
		return
	}
	s.finishOIDCLogin(w, r, flow, result, "registered")
}

// Caller holds the lifecycle lock through the response, allowing cancellation
// to revoke issuance even if a login response arrives after local login/logout.
func (s *Server) finishOIDCLogin(w http.ResponseWriter, r *http.Request, flow oidcFlow, result identity.ExternalLoginResult, event string) {
	flow.lifecycle.issuedSessionID = result.SessionID
	flow.UserID, flow.SessionID = result.User.ID, result.SessionID
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
	s.oidcAudit(r, flow, event, true)
	writeJSON(w, http.StatusOK, map[string]string{"result": "login"})
}

func (s *Server) cancelOIDCRegistration(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) {
		return
	}
	flow, ok := s.takeOIDCRegistration(w, r)
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
	s.oidcAudit(r, flow, "registration_cancelled", true)
	s.setOIDCCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// This also handles page departure while exchange is pending. It accepts only
// the initiating browser and exact flow, including consumed cancellation data.
func (s *Server) cancelOIDC(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) {
		return
	}
	var input struct {
		FlowID string `json:"flow_id"`
	}
	if err := decodeJSON(w, r, &input, 1024); err != nil {
		badJSON(w, r, err)
		return
	}
	if len(input.FlowID) != 43 {
		s.rejectOIDC(w, r, oidcFlow{})
		return
	}
	effects, ok := s.oidcFlows.cancel(input.FlowID, oidcBrowser(r), time.Now())
	if !ok {
		session, err := s.oidcCurrentSession(r)
		if err != nil {
			internalError(s, w, r, "check cancellation session", err)
			return
		}
		matched := s.oidcFlows.cancelSessionReauthentication(input.FlowID, session, time.Now())
		if len(matched) != 1 {
			s.rejectOIDC(w, r, oidcFlow{})
			return
		}
		effects = matched[0]
	}
	if err := s.clearOIDCEffects(r, effects); err != nil {
		internalError(s, w, r, "cancel OIDC flow", err)
		return
	}
	// Do not alter cookies: a late keepalive cancellation response must never
	// overwrite the cookie of a newer browser transaction or local login.
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
