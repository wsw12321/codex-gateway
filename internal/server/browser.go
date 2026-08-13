package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

type finishCeremonyRequest struct {
	FlowID     string          `json:"flow_id"`
	Credential json.RawMessage `json:"credential"`
}

func (s *Server) beginLogin(w http.ResponseWriter, r *http.Request) {
	ceremony, err := s.identity.BeginLogin()
	if err != nil {
		internalError(s, w, r, "begin login", err)
		return
	}
	writeJSON(w, http.StatusOK, ceremony)
}

func (s *Server) finishLogin(w http.ResponseWriter, r *http.Request) {
	var input finishCeremonyRequest
	if err := decodeJSON(w, r, &input, 2<<20); err != nil {
		badJSON(w, r, err)
		return
	}
	result, err := s.identity.FinishLogin(r.Context(), input.FlowID, input.Credential, httpx.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		s.authenticationFailure(w, r, "login", err)
		return
	}
	s.setSessionCookie(w, result.SessionToken)
	s.audit(r, result.User.ID, "", "identity.login", true, "user", result.User.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": publicUser(result.User)})
}

func (s *Server) beginRegistration(w http.ResponseWriter, r *http.Request) {
	var input struct {
		InvitationToken string `json:"invitation_token"`
		Username        string `json:"username"`
		DisplayName     string `json:"display_name"`
	}
	if err := decodeJSON(w, r, &input, 64<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	ceremony, err := s.identity.BeginInvitationRegistration(r.Context(), input.InvitationToken, input.Username, input.DisplayName)
	if err != nil {
		s.authenticationFailure(w, r, "registration", err)
		return
	}
	writeJSON(w, http.StatusOK, ceremony)
}

func (s *Server) finishRegistration(w http.ResponseWriter, r *http.Request) {
	var input finishCeremonyRequest
	if err := decodeJSON(w, r, &input, 2<<20); err != nil {
		badJSON(w, r, err)
		return
	}
	result, err := s.identity.FinishInvitationRegistration(r.Context(), input.FlowID, input.Credential, httpx.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		s.authenticationFailure(w, r, "registration", err)
		return
	}
	s.setSessionCookie(w, result.SessionToken)
	s.audit(r, result.User.ID, "", "identity.registration", true, "user", result.User.ID, nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "user": publicUser(result.User), "recovery_codes": result.RecoveryCodes,
		"warning": "恢复码只显示这一次；请立即离线保存。",
	})
}

func (s *Server) beginRecovery(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Code     string `json:"recovery_code"`
	}
	if err := decodeJSON(w, r, &input, 32<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	ceremony, err := s.identity.BeginRecovery(r.Context(), input.Username, input.Code)
	if err != nil {
		s.authenticationFailure(w, r, "recovery", err)
		return
	}
	s.audit(r, "", "", "identity.recovery_started", true, "user", "", nil)
	writeJSON(w, http.StatusOK, ceremony)
}

func (s *Server) finishRecovery(w http.ResponseWriter, r *http.Request) {
	var input finishCeremonyRequest
	if err := decodeJSON(w, r, &input, 2<<20); err != nil {
		badJSON(w, r, err)
		return
	}
	result, err := s.identity.FinishRecovery(r.Context(), input.FlowID, input.Credential, httpx.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		s.authenticationFailure(w, r, "recovery", err)
		return
	}
	s.setSessionCookie(w, result.SessionToken)
	s.audit(r, result.User.ID, "", "identity.account_recovered", true, "user", result.User.ID, nil)
	_, _ = s.store.CreateAlert(r.Context(), store.CreateAlertParams{
		Type: "account_recovery", Severity: "warning", UserID: result.User.ID,
		DedupeKey: "account_recovery:" + result.User.ID, Title: "账号已使用恢复码恢复",
		Details: map[string]any{"request_id": httpx.RequestID(r.Context())},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "recovery_codes": result.RecoveryCodes,
		"warning": "旧会话和旧恢复码均已失效；新恢复码只显示这一次。",
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_ = s.store.RevokeSession(r.Context(), session.ID, "user_logout", time.Now().UTC())
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) beginReauthentication(w http.ResponseWriter, r *http.Request) {
	ceremony, err := s.identity.BeginReauthentication(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		internalError(s, w, r, "begin reauthentication", err)
		return
	}
	writeJSON(w, http.StatusOK, ceremony)
}

func (s *Server) finishReauthentication(w http.ResponseWriter, r *http.Request) {
	var input finishCeremonyRequest
	if err := decodeJSON(w, r, &input, 2<<20); err != nil {
		badJSON(w, r, err)
		return
	}
	session := sessionFrom(r.Context())
	if err := s.identity.FinishReauthentication(r.Context(), input.FlowID, input.Credential, session.ID); err != nil {
		s.authenticationFailure(w, r, "reauthentication", err)
		return
	}
	s.audit(r, session.UserID, session.ID, "identity.reauthenticated", true, "session", session.ID, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) beginAddPasskey(w http.ResponseWriter, r *http.Request) {
	ceremony, err := s.identity.BeginAddCredential(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		internalError(s, w, r, "begin add Passkey", err)
		return
	}
	writeJSON(w, http.StatusOK, ceremony)
}

func (s *Server) finishAddPasskey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		FlowID     string          `json:"flow_id"`
		Credential json.RawMessage `json:"credential"`
		Nickname   string          `json:"nickname"`
	}
	if err := decodeJSON(w, r, &input, 2<<20); err != nil {
		badJSON(w, r, err)
		return
	}
	credential, err := s.identity.FinishAddCredential(r.Context(), input.FlowID, input.Credential, input.Nickname)
	if err != nil {
		s.authenticationFailure(w, r, "add_passkey", err)
		return
	}
	s.audit(r, userFrom(r.Context()).ID, sessionFrom(r.Context()).ID, "passkey.created", true, "webauthn_credential", credential.ID, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "credential_id": credential.ID})
}

func (s *Server) createDevice(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &input, 32<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	device, err := s.store.CreateDevice(r.Context(), store.CreateDeviceParams{UserID: userFrom(r.Context()).ID, Name: input.Name})
	if err != nil {
		s.storeWriteError(w, r, "create device", err)
		return
	}
	writeJSON(w, http.StatusCreated, device)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &input, 32<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	project, err := s.store.CreateProject(r.Context(), store.CreateProjectParams{
		UserID: userFrom(r.Context()).ID, Slug: input.Slug, Name: input.Name,
	})
	if err != nil {
		s.storeWriteError(w, r, "create project", err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name             string   `json:"name"`
		DeviceID         string   `json:"device_id"`
		DefaultProjectID string   `json:"default_project_id"`
		ExpiresDays      int      `json:"expires_days"`
		Models           []string `json:"models"`
	}
	if err := decodeJSON(w, r, &input, 64<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	user := userFrom(r.Context())
	device, err := s.store.GetDevice(r.Context(), user.ID, input.DeviceID)
	if err != nil || device.Status != store.StatusActive {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_device", "设备无效")
		return
	}
	if input.DefaultProjectID != "" {
		project, err := s.store.GetProject(r.Context(), user.ID, input.DefaultProjectID)
		if err != nil || project.Status != store.StatusActive {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_project", "默认项目无效")
			return
		}
	}
	for _, model := range input.Models {
		if !validModel(model) {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_allowlist", "模型白名单包含无效名称")
			return
		}
	}
	if input.ExpiresDays == 0 {
		input.ExpiresDays = 90
	}
	if input.ExpiresDays < 1 || input.ExpiresDays > 365 {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_expiry", "有效期必须为 1 到 365 天")
		return
	}
	generated, err := security.GenerateAPIKey()
	if err != nil {
		internalError(s, w, r, "generate API key", err)
		return
	}
	digest, err := security.HashAPIKey(s.config.KeyPepper, generated.Token)
	if err != nil {
		internalError(s, w, r, "hash API key", err)
		return
	}
	now := time.Now().UTC()
	key, err := s.store.CreateAPIKey(r.Context(), store.CreateAPIKeyParams{
		PublicID: generated.PublicID, KeyPrefix: generated.Prefix, KeyHash: digest[:],
		UserID: user.ID, DeviceID: device.ID, DefaultProjectID: input.DefaultProjectID,
		Name: strings.TrimSpace(input.Name), ModelAllowlist: input.Models,
		CreatedAt: now, ExpiresAt: now.Add(time.Duration(input.ExpiresDays) * 24 * time.Hour),
	})
	if err != nil {
		s.storeWriteError(w, r, "create API key", err)
		return
	}
	s.audit(r, user.ID, sessionFrom(r.Context()).ID, "api_key.created", true, "api_key", key.ID, map[string]any{"prefix": key.KeyPrefix})
	_, _ = s.store.CreateAlert(r.Context(), store.CreateAlertParams{
		Type: "api_key_created", Severity: "info", UserID: user.ID,
		Title: "已创建新的 API Key", Details: map[string]any{"prefix": key.KeyPrefix},
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": key.ID, "api_key": generated.Token, "prefix": generated.Prefix,
		"expires_at": key.ExpiresAt, "warning": "API Key 只显示这一次。",
	})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("id")
	user := userFrom(r.Context())
	if err := s.store.RevokeAPIKey(r.Context(), user.ID, keyID, "user_revoked", time.Now().UTC()); err != nil {
		s.storeWriteError(w, r, "revoke API key", err)
		return
	}
	s.audit(r, user.ID, sessionFrom(r.Context()).ID, "api_key.revoked", true, "api_key", keyID, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Kind           string `json:"kind"`
		TargetUsername string `json:"target_username"`
	}
	if err := decodeJSON(w, r, &input, 32<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	kind := store.InvitationMember
	targetID := ""
	if input.Kind == "recovery" {
		kind = store.InvitationRecovery
		target, err := s.store.GetUserByUsername(r.Context(), input.TargetUsername)
		if err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "unknown_recovery_user", "恢复账号不存在")
			return
		}
		targetID = target.ID
	} else if input.Kind != "" && input.Kind != "member" {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_invitation_kind", "邀请类型无效")
		return
	}
	generated, err := security.GenerateOpaqueToken(security.InvitationToken)
	if err != nil {
		internalError(s, w, r, "generate invitation", err)
		return
	}
	digest, err := security.PepperTokenDigest(s.config.TokenPepper, generated.Digest)
	if err != nil {
		internalError(s, w, r, "hash invitation", err)
		return
	}
	user := userFrom(r.Context())
	invitation, err := s.store.CreateInvitation(r.Context(), store.CreateInvitationParams{
		Kind: kind, TokenHash: digest[:], InviterID: user.ID, TargetUserID: targetID,
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "create invitation", err)
		return
	}
	link := invitationLink(s.config.PublicURL.String(), generated.Token, kind)
	s.audit(r, user.ID, sessionFrom(r.Context()).ID, "invitation.created", true, "invitation", invitation.ID, map[string]any{"kind": kind})
	writeJSON(w, http.StatusCreated, map[string]any{"id": invitation.ID, "link": link, "expires_at": invitation.ExpiresAt})
}

func invitationLink(publicURL, token, kind string) string {
	fragment := "#token=" + token
	if kind == store.InvitationRecovery {
		fragment += "&kind=recovery"
	}
	return strings.TrimRight(publicURL, "/") + "/join" + fragment
}

func (s *Server) adminState(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	devices, err := s.store.ListDevices(r.Context(), user.ID)
	if err != nil {
		internalError(s, w, r, "list devices", err)
		return
	}
	projects, err := s.store.ListProjects(r.Context(), user.ID)
	if err != nil {
		internalError(s, w, r, "list projects", err)
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), user.ID)
	if err != nil {
		internalError(s, w, r, "list API keys", err)
		return
	}
	credentials, err := s.store.ListWebAuthnCredentials(r.Context(), user.ID)
	if err != nil {
		internalError(s, w, r, "list Passkeys", err)
		return
	}
	methods, err := s.store.LoginMethods(r.Context(), user.ID)
	if err != nil {
		internalError(s, w, r, "get login methods", err)
		return
	}
	session := sessionFrom(r.Context())
	now := time.Now().UTC()
	recent := false
	var verificationExpires *time.Time
	if session.RecentlyVerifiedAt != nil {
		value := session.RecentlyVerifiedAt.Add(s.config.ReauthMaxAge)
		if value.After(now) {
			recent = true
			verificationExpires = &value
		}
	}
	writeJSON(w, http.StatusOK, newAdminStateResponse(
		user, devices, projects, keys, credentials, methods, recent, verificationExpires,
	))
}

type adminStateResponse struct {
	User                        adminUser          `json:"user"`
	Devices                     []adminDevice      `json:"devices"`
	Projects                    []adminProject     `json:"projects"`
	APIKeys                     []adminAPIKey      `json:"api_keys"`
	Passkeys                    []adminPasskey     `json:"passkeys"`
	LoginMethods                store.LoginMethods `json:"login_methods"`
	RecentlyVerified            bool               `json:"recently_verified"`
	RecentVerificationExpiresAt *time.Time         `json:"recent_verification_expires_at"`
}

type adminUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type adminDevice struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

type adminProject struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type adminAPIKey struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	KeyPrefix        string     `json:"key_prefix"`
	DeviceID         string     `json:"device_id"`
	DefaultProjectID *string    `json:"default_project_id"`
	Status           string     `json:"status"`
	ModelAllowlist   []string   `json:"model_allowlist"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	LastUsedAt       *time.Time `json:"last_used_at"`
}

type adminPasskey struct {
	ID             string     `json:"id"`
	Nickname       string     `json:"nickname"`
	BackupEligible bool       `json:"backup_eligible"`
	BackupState    bool       `json:"backup_state"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     *time.Time `json:"last_used_at"`
}

func newAdminStateResponse(
	user store.User,
	devices []store.Device,
	projects []store.Project,
	keys []store.APIKey,
	credentials []store.WebAuthnCredential,
	methods store.LoginMethods,
	recent bool,
	verificationExpires *time.Time,
) adminStateResponse {
	response := adminStateResponse{
		User: adminUser{
			ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, Role: user.Role,
		},
		Devices: make([]adminDevice, 0, len(devices)), Projects: make([]adminProject, 0, len(projects)),
		APIKeys: make([]adminAPIKey, 0, len(keys)), Passkeys: make([]adminPasskey, 0, len(credentials)),
		LoginMethods:     methods,
		RecentlyVerified: recent, RecentVerificationExpiresAt: verificationExpires,
	}
	for _, value := range devices {
		response.Devices = append(response.Devices, adminDevice{
			ID: value.ID, Name: value.Name, Status: value.Status,
			CreatedAt: value.CreatedAt, LastSeenAt: value.LastSeenAt,
		})
	}
	for _, value := range projects {
		response.Projects = append(response.Projects, adminProject{
			ID: value.ID, Slug: value.Slug, Name: value.Name,
			Status: value.Status, CreatedAt: value.CreatedAt,
		})
	}
	for _, value := range keys {
		models := append([]string{}, value.ModelAllowlist...)
		response.APIKeys = append(response.APIKeys, adminAPIKey{
			ID: value.ID, Name: value.Name, KeyPrefix: value.KeyPrefix,
			DeviceID: value.DeviceID, DefaultProjectID: value.DefaultProjectID,
			Status: value.Status, ModelAllowlist: models, CreatedAt: value.CreatedAt,
			ExpiresAt: value.ExpiresAt, LastUsedAt: value.LastUsedAt,
		})
	}
	for _, value := range credentials {
		response.Passkeys = append(response.Passkeys, adminPasskey{
			ID: value.ID, Nickname: value.Nickname, BackupEligible: value.BackupEligible,
			BackupState: value.BackupState, CreatedAt: value.CreatedAt, LastUsedAt: value.LastUsedAt,
		})
	}
	return response
}

func (s *Server) alertsJSON(w http.ResponseWriter, r *http.Request) {
	alerts, err := s.store.ListAlerts(r.Context(), r.URL.Query().Get("status"), 200, 0)
	if err != nil {
		internalError(s, w, r, "list alerts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}

func (s *Server) authenticationFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	s.audit(r, "", "", "identity."+operation+"_failed", false, "", "", map[string]any{"reason": "verification_failed"})
	status := http.StatusBadRequest
	code := "webauthn_verification_failed"
	message := "Passkey 验证失败，请重新开始"
	if errors.Is(err, store.ErrInvitationUnavailable) {
		code, message = "invitation_invalid", "邀请已失效、已使用或已撤销"
	} else if errors.Is(err, identity.ErrRecoveryRejected) {
		code, message = "recovery_rejected", "用户名或恢复码无效"
	} else if errors.Is(err, store.ErrConflict) {
		status, code, message = http.StatusConflict, "identity_conflict", "用户名或凭证已存在"
	} else if errors.Is(err, identity.ErrInvalidUsername) || errors.Is(err, identity.ErrInvalidDisplayName) {
		code, message = "invalid_profile", "用户名或显示名称格式无效"
	} else if errors.Is(err, identity.ErrInvalidNickname) {
		code, message = "invalid_passkey_nickname", "Passkey 昵称必须为 1 到 80 个字符"
	}
	httpx.WriteError(w, r, status, "authentication_error", code, message)
}

func (s *Server) storeWriteError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "resource_conflict", "名称或资源已存在")
		return
	}
	if errors.Is(err, store.ErrInvalid) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_resource", "资源参数无效")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "resource_not_found", "资源不存在")
		return
	}
	internalError(s, w, r, operation, err)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", MaxAge: int(s.config.SessionMax.Seconds()),
		Secure: !s.config.DevInsecure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		Secure: !s.config.DevInsecure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) audit(r *http.Request, userID, sessionID, eventType string, success bool, subjectType, subjectID string, metadata map[string]any) {
	_, _ = s.store.AppendAuditEvent(r.Context(), store.AppendAuditEventParams{
		ActorUserID: userID, ActorSessionID: sessionID, EventType: eventType,
		Severity: "info", Success: success, SourceIP: safeIP(r.Context()),
		SubjectType: subjectType, SubjectID: subjectID, RequestID: httpx.RequestID(r.Context()), Metadata: metadata,
	})
}

func publicUser(user store.User) map[string]any {
	return map[string]any{"id": user.ID, "username": user.Username, "display_name": user.DisplayName, "role": user.Role}
}
