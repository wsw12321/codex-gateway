package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/store"
)

func (s *Server) beginOIDCReauthentication(w http.ResponseWriter, r *http.Request) {
	if !s.oidcAvailable(w, r) {
		return
	}
	session := sessionFrom(r.Context())
	link, err := s.oidcRepository().GetExternalIdentity(r.Context(), session.UserID)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, r, http.StatusConflict, "authentication_error", "oidc_not_linked", "当前账号未绑定吾水阁账号，请选择其他验证方式。")
		return
	}
	if err != nil {
		internalError(s, w, r, "read reauthentication binding", err)
		return
	}
	s.beginOIDC(w, r, oidcFlow{Kind: "reauth", UserID: session.UserID, SessionID: session.ID,
		ExternalIdentityID: link.ID, Identity: identity.OIDCIdentity{Issuer: link.Issuer, Subject: link.Subject}})
}

// Unlike linking, reauthentication starts after the old verification expires.
// Require the same active user, session, and exact binding throughout instead.
func (s *Server) oidcReauthenticationSession(r *http.Request, flow oidcFlow) bool {
	session, err := s.oidcCurrentSession(r)
	if err != nil || session.ID == "" || session.ID != flow.SessionID || session.UserID != flow.UserID {
		return false
	}
	user, err := s.oidcRepository().GetUser(r.Context(), flow.UserID)
	if err != nil || user.Status != store.StatusActive {
		return false
	}
	link, err := s.oidcRepository().GetExternalIdentity(r.Context(), flow.UserID)
	return err == nil && link.ID == flow.ExternalIdentityID && link.Issuer == flow.Identity.Issuer && link.Subject == flow.Identity.Subject
}

// Caller holds the lifecycle lock. Store rechecks the exact binding and active
// session atomically with the write, serialized with unlink and revocation.
func (s *Server) completeOIDCReauthentication(w http.ResponseWriter, r *http.Request, flow oidcFlow, external identity.OIDCIdentity) {
	if external.Issuer != flow.Identity.Issuer || external.Subject != flow.Identity.Subject || !s.oidcReauthenticationSession(r, flow) {
		s.oidcAudit(r, flow, "reauth_rejected", false)
		s.rejectOIDC(w, r, flow)
		return
	}
	verifiedAt, err := s.oidcRepository().CompleteExternalReauthentication(r.Context(), store.CompleteExternalReauthenticationParams{
		UserID: flow.UserID, SessionID: flow.SessionID, ExternalIdentityID: flow.ExternalIdentityID,
		Issuer: external.Issuer, Subject: external.Subject, VerifiedAt: flow.VerifiedAt, At: time.Now().UTC(),
	})
	if err != nil {
		s.oidcAudit(r, flow, "reauth_rejected", false)
		s.rejectOIDC(w, r, flow)
		return
	}
	flow.lifecycle.verifiedUserID, flow.lifecycle.verifiedSessionID, flow.lifecycle.verifiedAt = flow.UserID, flow.SessionID, verifiedAt
	s.oidcFlows.retainVerificationCancellation(flow, verifiedAt.Add(oidcVerificationAge))
	if !flow.active(time.Now()) || r.Context().Err() != nil {
		if err := s.clearOIDCEffects(r, flow.lifecycle.cancel()); err != nil {
			internalError(s, w, r, "clear cancelled OIDC verification", err)
			return
		}
		s.rejectOIDC(w, r, flow)
		return
	}
	s.setOIDCCookie(w, "", -1)
	s.oidcAudit(r, flow, "reauthenticated", true)
	writeJSON(w, http.StatusOK, map[string]string{"result": "reauthenticated"})
}
