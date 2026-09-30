package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

// Public invitation metadata intentionally excludes token hashes and identity
// recovery targets. Tokens are returned only by creation.
type invitationView struct {
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	GroupID          *string    `json:"group_id"`
	GroupName        string     `json:"group_name"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	MaxUses          int        `json:"max_uses"`
	UsedCount        int        `json:"used_count"`
	RequiresApproval bool       `json:"requires_approval"`
	RevokedAt        *time.Time `json:"revoked_at"`
	Status           string     `json:"status"`
}

func publicInvitation(value store.Invitation, now time.Time) invitationView {
	if value.UsedAt != nil && value.UsedCount == 0 {
		value.UsedCount = 1
	}
	status := "active"
	switch {
	case value.RevokedAt != nil:
		status = "revoked"
	case !value.ExpiresAt.After(now):
		status = "expired"
	case value.UsedCount >= value.MaxUses || value.UsedAt != nil:
		status = "full"
	}
	return invitationView{
		ID: value.ID, Kind: value.Kind, GroupID: value.GroupID, GroupName: value.GroupName,
		CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt, MaxUses: value.MaxUses,
		UsedCount: value.UsedCount, RequiresApproval: value.RequiresApproval,
		RevokedAt: value.RevokedAt, Status: status,
	}
}

func invitationPagination(r *http.Request) (int, int, error) {
	limit, offset := 50, 0
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, store.ErrInvalid
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return 0, 0, store.ErrInvalid
		}
	}
	return limit, offset, nil
}

func (s *Server) invitationsJSON(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := invitationPagination(r)
	if err != nil {
		s.invitationError(w, r, "list invitations", err)
		return
	}
	values, err := s.store.ListInvitations(r.Context(), r.URL.Query().Get("kind"), r.URL.Query().Get("group_id"), limit, offset)
	if err != nil {
		s.invitationError(w, r, "list invitations", err)
		return
	}
	result := make([]invitationView, 0, len(values))
	for _, value := range values {
		result = append(result, publicInvitation(value, time.Now().UTC()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": result, "limit": limit, "offset": offset})
}

func (s *Server) invitationApplicationsJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit, offset, err := invitationPagination(r)
	if err != nil || !validGroupID(id) {
		s.invitationError(w, r, "list invitation applications", store.ErrInvalid)
		return
	}
	applications, err := s.store.ListInvitationApplications(r.Context(), id, limit, offset)
	if err != nil {
		s.invitationError(w, r, "list invitation applications", err)
		return
	}
	if applications == nil {
		applications = []store.InvitationApplication{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": applications, "limit": limit, "offset": offset})
}

func (s *Server) reviewInvitation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ApplicationIDs []string `json:"application_ids"`
		Decision       string   `json:"decision"`
	}
	if err := decodeJSON(w, r, &input, 32<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	id := r.PathValue("id")
	if !validGroupID(id) || len(input.ApplicationIDs) < 1 || len(input.ApplicationIDs) > 100 ||
		(input.Decision != "approve" && input.Decision != "reject") {
		s.invitationError(w, r, "review invitation", store.ErrInvalid)
		return
	}
	seen := make(map[string]bool, len(input.ApplicationIDs))
	for _, applicationID := range input.ApplicationIDs {
		if !validGroupID(applicationID) || seen[applicationID] {
			s.invitationError(w, r, "review invitation", store.ErrInvalid)
			return
		}
		seen[applicationID] = true
	}
	user, session := userFrom(r.Context()), sessionFrom(r.Context())
	count, err := s.store.ReviewInvitationApplications(r.Context(), id, input.ApplicationIDs, input.Decision, user.ID, time.Now().UTC())
	if err != nil {
		s.invitationError(w, r, "review invitation", err)
		return
	}
	s.audit(r, user.ID, session.ID, "invitation.reviewed", true, "invitation", id,
		map[string]any{"decision": input.Decision, "count": count})
	writeJSON(w, http.StatusOK, map[string]any{"updated_count": count})
}

func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validGroupID(id) {
		s.invitationError(w, r, "revoke invitation", store.ErrInvalid)
		return
	}
	user, session := userFrom(r.Context()), sessionFrom(r.Context())
	if err := s.store.RevokeInvitation(r.Context(), id, user.ID, time.Now().UTC()); err != nil {
		s.invitationError(w, r, "revoke invitation", err)
		return
	}
	s.audit(r, user.ID, session.ID, "invitation.revoked", true, "invitation", id, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) invitationDigest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var input struct {
		Token string `json:"invitation_token"`
	}
	if err := decodeJSON(w, r, &input, 8<<10); err != nil {
		badJSON(w, r, err)
		return nil, false
	}
	raw, err := security.DigestOpaqueToken(security.InvitationToken, input.Token)
	if err != nil {
		s.invitationError(w, r, "read invitation", store.ErrInvitationUnavailable)
		return nil, false
	}
	digest, err := security.PepperTokenDigest(s.config.TokenPepper, raw)
	if err != nil {
		internalError(s, w, r, "hash invitation", err)
		return nil, false
	}
	return digest[:], true
}

func (s *Server) inspectInvitation(w http.ResponseWriter, r *http.Request) {
	digest, ok := s.invitationDigest(w, r)
	if !ok {
		return
	}
	invitation, err := s.store.InspectInvitation(r.Context(), digest, time.Now().UTC())
	if err != nil {
		s.invitationError(w, r, "inspect invitation", err)
		return
	}
	writeJSON(w, http.StatusOK, publicInvitation(invitation, time.Now().UTC()))
}

func (s *Server) joinInvitation(w http.ResponseWriter, r *http.Request) {
	digest, ok := s.invitationDigest(w, r)
	if !ok {
		return
	}
	application, err := s.store.JoinInvitation(r.Context(), digest, userFrom(r.Context()).ID)
	if err != nil {
		s.invitationError(w, r, "join invitation", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": application.Status, "application": application})
}

func (s *Server) invitationError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	if errors.Is(err, store.ErrInvitationUnavailable) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invitation_invalid", "邀请码已过期、撤销、名额已满或不适用于此操作")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "invitation_conflict", "申请或群组状态已变化，请刷新后重试")
		return
	}
	s.storeWriteError(w, r, operation, err)
}

func (s *Server) writeRegistrationResult(w http.ResponseWriter, r *http.Request, result identity.RegistrationResult, method string) {
	pending := result.User.Status == store.StatusPending
	status := "approved"
	if pending {
		status = "pending"
		// Rejected identities must be fully deletable. Audit only the invitation
		// and count, without an actor/subject pointing at the pending account.
		_, _ = s.store.AppendAuditEvent(r.Context(), store.AppendAuditEventParams{
			EventType: "invitation.applied", Severity: "info", Success: true,
			SubjectType: "invitation", SubjectID: result.InvitationID, Metadata: map[string]any{"count": 1},
		})
	} else {
		s.setSessionCookie(w, result.SessionToken)
		s.audit(r, result.User.ID, "", "identity.registration", true, "user", result.User.ID, map[string]any{"method": method})
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "user": publicUser(result.User), "recovery_codes": result.RecoveryCodes,
		"status": status, "requires_approval": pending,
		"warning": "恢复码只显示这一次；请立即离线保存。",
	})
}
