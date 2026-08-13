package server

import (
	"errors"
	"net/http"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/store"
)

type passwordLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) passwordRegister(w http.ResponseWriter, r *http.Request) {
	var input struct {
		InvitationToken string `json:"invitation_token"`
		Username        string `json:"username"`
		DisplayName     string `json:"display_name"`
		Password        string `json:"password"`
	}
	if err := decodeJSON(w, r, &input, 64<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	result, err := s.identity.RegisterPassword(r.Context(), input.InvitationToken, input.Username, input.DisplayName, input.Password, httpx.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		s.passwordFailure(w, r, "password_registration", err)
		return
	}
	s.setSessionCookie(w, result.SessionToken)
	s.audit(r, result.User.ID, "", "identity.registration", true, "user", result.User.ID, map[string]any{"method": "password"})
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "user": publicUser(result.User), "recovery_codes": result.RecoveryCodes,
		"warning": "恢复码只显示这一次；请立即离线保存。",
	})
}

func (s *Server) passwordLogin(w http.ResponseWriter, r *http.Request) {
	input, _ := r.Context().Value(passwordLoginContextKey).(passwordLoginRequest)
	result, err := s.identity.PasswordLogin(r.Context(), input.Username, input.Password, httpx.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		s.passwordFailure(w, r, "password_login", err)
		return
	}
	s.setSessionCookie(w, result.SessionToken)
	s.audit(r, result.User.ID, "", "identity.login", true, "user", result.User.ID, map[string]any{"method": "password"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": publicUser(result.User)})
}

func (s *Server) passwordRecovery(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username        string `json:"username"`
		RecoveryCode    string `json:"recovery_code"`
		InvitationToken string `json:"invitation_token"`
		Password        string `json:"password"`
	}
	if err := decodeJSON(w, r, &input, 64<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	result, err := s.identity.RecoverWithPassword(r.Context(), input.Username, input.RecoveryCode, input.InvitationToken, input.Password, httpx.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		s.passwordFailure(w, r, "password_recovery", err)
		return
	}
	s.setSessionCookie(w, result.SessionToken)
	s.audit(r, result.User.ID, "", "identity.account_recovered", true, "user", result.User.ID, map[string]any{"method": "password"})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "recovery_codes": result.RecoveryCodes,
		"warning": "旧会话和旧恢复码均已失效；未选择的登录方式保持有效。",
	})
}

func (s *Server) passwordReauthenticate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input, 64<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	user, session := userFrom(r.Context()), sessionFrom(r.Context())
	if err := s.identity.ReauthenticatePassword(r.Context(), user.ID, session.ID, input.Password); err != nil {
		s.passwordFailure(w, r, "password_reauthentication", err)
		return
	}
	s.audit(r, user.ID, session.ID, "identity.reauthenticated", true, "session", session.ID, map[string]any{"method": "password"})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) putPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input, 64<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	user, session := userFrom(r.Context()), sessionFrom(r.Context())
	if err := s.identity.SetPassword(r.Context(), user.ID, session.ID, input.Password); err != nil {
		s.passwordFailure(w, r, "password_changed", err)
		return
	}
	s.audit(r, user.ID, session.ID, "password.updated", true, "user", user.ID, map[string]any{"method": "password"})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) passwordFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	actorUserID, actorSessionID := "", ""
	if operation == "password_reauthentication" || operation == "password_changed" {
		actorUserID = userFrom(r.Context()).ID
		actorSessionID = sessionFrom(r.Context()).ID
	}
	s.audit(r, actorUserID, actorSessionID, "identity."+operation+"_failed", false, "", "", map[string]any{"method": "password", "result": "rejected"})
	if errors.Is(err, identity.ErrHashBusy) {
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", "password_hashing_busy", "认证服务繁忙，请稍后重试")
		return
	}
	if errors.Is(err, identity.ErrInvalidPassword) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_password", "密码必须为 8 到 128 个 Unicode 字符且不超过 512 字节")
		return
	}
	if errors.Is(err, identity.ErrInvalidCredentials) {
		httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "invalid_credentials", "用户名或密码无效")
		return
	}
	if errors.Is(err, identity.ErrRecoveryRejected) {
		httpx.WriteError(w, r, http.StatusBadRequest, "authentication_error", "recovery_rejected", "恢复凭据无效")
		return
	}
	if errors.Is(err, store.ErrInvitationUnavailable) || errors.Is(err, store.ErrConflict) ||
		errors.Is(err, identity.ErrInvalidUsername) || errors.Is(err, identity.ErrInvalidDisplayName) {
		s.authenticationFailure(w, r, operation, err)
		return
	}
	internalError(s, w, r, operation, err)
}
