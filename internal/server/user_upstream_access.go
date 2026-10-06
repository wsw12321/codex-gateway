package server

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const userUpstreamAccessRequestBytes = 512 << 10

type userUpstreamAccountOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	EmailMasked string `json:"email_masked"`
	Status      string `json:"status"`
}

type userUpstreamAccessProvider struct {
	store.UserUpstreamAccess
	Accounts    []userUpstreamAccountOption `json:"accounts"`
	SyncWarning string                      `json:"sync_warning,omitempty"`
}

type userUpstreamAccessResponse struct {
	UserID    string                       `json:"user_id"`
	Providers []userUpstreamAccessProvider `json:"providers"`
}

func validUserUpstreamAccessTarget(userID string) bool {
	parsed, err := uuid.Parse(userID)
	return err == nil && parsed.String() == userID
}

func validUserUpstreamAccessProvider(provider string) bool {
	return provider == store.UpstreamProviderCodex || provider == store.UpstreamProviderAntigravity
}

func (s *Server) userUpstreamAccessJSON(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if !validUserUpstreamAccessTarget(userID) || r.URL.RawQuery != "" {
		s.storeWriteError(w, r, "read user upstream access", store.ErrInvalid)
		return
	}
	response := userUpstreamAccessResponse{UserID: userID, Providers: make([]userUpstreamAccessProvider, 0, 2)}
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity} {
		access, err := s.store.WithUpstreamProvider(provider).GetUserUpstreamAccess(r.Context(), userID)
		if err != nil {
			s.storeWriteError(w, r, "read user upstream access", err)
			return
		}
		response.Providers = append(response.Providers, userUpstreamAccessProvider{UserUpstreamAccess: access})
	}

	// Share the existing metadata synchronization lock with account management
	// and request discovery so a late snapshot cannot overwrite newer metadata.
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	for i := range response.Providers {
		entry := &response.Providers[i]
		if err := s.syncUpstreamAccountsLocked(r.Context(), entry.Provider); err != nil {
			entry.SyncWarning = "upstream_account_sync_unavailable"
			if s.logger != nil {
				s.logger.Warn("user upstream account metadata sync failed", "provider", entry.Provider)
			}
		}
		accounts, err := s.store.WithUpstreamProvider(entry.Provider).ListUpstreamAccounts(r.Context())
		if err != nil {
			internalError(s, w, r, "list user upstream account options", err)
			return
		}
		entry.Accounts = make([]userUpstreamAccountOption, 0, len(accounts))
		for _, account := range accounts {
			entry.Accounts = append(entry.Accounts, userUpstreamAccountOption{
				ID: account.ID, DisplayName: account.DisplayName, EmailMasked: account.MaskedEmail, Status: account.Status,
			})
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) setUserUpstreamAccess(w http.ResponseWriter, r *http.Request) {
	userID, provider := r.PathValue("user_id"), r.PathValue("provider")
	if !validUserUpstreamAccessTarget(userID) || !validUserUpstreamAccessProvider(provider) {
		s.storeWriteError(w, r, "set user upstream access", store.ErrInvalid)
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_user_upstream_access_request", "账号权限请求必须为 JSON，且不能包含查询参数")
		return
	}
	var mode, reason string
	var accountIDs []string
	if err := decodeExactJSONFields(r, userUpstreamAccessRequestBytes, map[string]any{"mode": &mode, "account_ids": &accountIDs, "reason": &reason}); err != nil {
		badJSON(w, r, err)
		return
	}
	reason, validReason := validModelAccessReason(reason)
	valid := validReason && accountIDs != nil && len(accountIDs) <= 10000 &&
		(mode == "all" || mode == "selected") && (mode != "all" || len(accountIDs) == 0)
	seen := make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		if !validUpstreamAccountID(id) || seen[id] {
			valid = false
		}
		seen[id] = true
	}
	if !valid {
		s.storeWriteError(w, r, "set user upstream access", store.ErrInvalid)
		return
	}
	access, err := s.store.WithUpstreamProvider(provider).SetUserUpstreamAccess(r.Context(), store.SetUserUpstreamAccessParams{
		UserID: userID, Mode: mode, AccountIDs: accountIDs, Reason: reason, At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set user upstream access", err)
		return
	}
	writeJSON(w, http.StatusOK, access)
}
