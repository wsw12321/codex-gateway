package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/store"
)

func (s *Server) patchProfile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username    optionalString `json:"username"`
		DisplayName optionalString `json:"display_name"`
	}
	if err := decodeJSON(w, r, &input, 4<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	username, displayName := input.Username.value, input.DisplayName.value
	if username == nil && displayName == nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_profile", "请至少提交用户名或显示名称")
		return
	}
	session := sessionFrom(r.Context())
	if username != nil {
		now := time.Now().UTC()
		if session.RecentlyVerifiedAt == nil || session.RecentlyVerifiedAt.After(now) || !session.RecentlyVerifiedAt.Add(s.config.ReauthMaxAge).After(now) {
			httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "recent_identity_verification_required", "此操作需要在 5 分钟内再次验证身份")
			return
		}
		value, err := identity.ValidateUsername(*username)
		if err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_profile", "用户名必须为 3 到 32 位，以英文字母开头，仅允许字母、数字、下划线和连字符")
			return
		}
		username = &value
	}
	if displayName != nil {
		value, err := identity.ValidateDisplayName(*displayName)
		if err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_profile", "显示名称必须为 1 到 80 个字符")
			return
		}
		displayName = &value
	}
	user, err := s.store.UpdateUserProfile(r.Context(), store.UpdateUserProfileParams{
		UserID: session.UserID, SessionID: session.ID, Username: username, DisplayName: displayName,
		VerificationMaxAge: s.config.ReauthMaxAge, SourceIP: safeIP(r.Context()), RequestID: httpx.RequestID(r.Context()),
	})
	if errors.Is(err, store.ErrUsernameTaken) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "username_taken", "用户名已被使用")
		return
	}
	if errors.Is(err, store.ErrProfileVerificationRequired) {
		httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "recent_identity_verification_required", "此操作需要在 5 分钟内再次验证身份")
		return
	}
	if err != nil {
		s.storeWriteError(w, r, "update profile", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(user)})
}
