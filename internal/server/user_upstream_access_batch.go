package server

import (
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type userUpstreamAccessBatchProvider struct {
	Provider    string                      `json:"provider"`
	Accounts    []userUpstreamAccountOption `json:"accounts"`
	SyncWarning string                      `json:"sync_warning,omitempty"`
}

type userUpstreamAccessBatchResponse struct {
	Providers []userUpstreamAccessBatchProvider `json:"providers"`
	Rules     []store.UserUpstreamAccess        `json:"rules"`
}

// validUniqueStrings reports whether values are all valid and distinct.
func validUniqueStrings(values []string, valid func(string) bool) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !valid(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

// userUpstreamAccessBatchJSON lists every provider's account choices and all
// explicit user scopes, so the batch editor needs a single read.
func (s *Server) userUpstreamAccessBatchJSON(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.storeWriteError(w, r, "read user upstream access batch", store.ErrInvalid)
		return
	}
	response := userUpstreamAccessBatchResponse{Providers: make([]userUpstreamAccessBatchProvider, 0, 3)}
	// Share the metadata synchronization lock with the per-user editor, and
	// read rules after synchronization so they match the listed accounts.
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		accounts, warning, err := s.upstreamAccessAccountOptionsLocked(r.Context(), provider)
		if err != nil {
			internalError(s, w, r, "list user upstream account options", err)
			return
		}
		response.Providers = append(response.Providers, userUpstreamAccessBatchProvider{Provider: provider, Accounts: accounts, SyncWarning: warning})
	}
	rules, err := s.store.ListUserUpstreamAccessRules(r.Context())
	if err != nil {
		s.storeWriteError(w, r, "read user upstream access batch", err)
		return
	}
	response.Rules = rules
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) setUserUpstreamAccessBatch(w http.ResponseWriter, r *http.Request) {
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_user_upstream_access_request", "账号权限请求必须为 JSON，且不能包含查询参数")
		return
	}
	var action, reason string
	var userIDs, accountIDs, providers []string
	if err := decodeExactJSONFields(r, userUpstreamAccessRequestBytes, map[string]any{
		"user_ids": &userIDs, "action": &action, "account_ids": &accountIDs, "providers": &providers, "reason": &reason,
	}); err != nil {
		badJSON(w, r, err)
		return
	}
	reason, valid := validModelAccessReason(reason)
	valid = valid && userIDs != nil && accountIDs != nil && providers != nil &&
		len(userIDs) > 0 && len(userIDs) <= 1000 && len(accountIDs) <= 1000 &&
		validUniqueStrings(userIDs, validUserUpstreamAccessTarget) &&
		validUniqueStrings(accountIDs, validUpstreamAccountID) &&
		validUniqueStrings(providers, validUserUpstreamAccessProvider)
	switch action {
	case store.UserUpstreamAccessBatchEnable, store.UserUpstreamAccessBatchDisable:
		valid = valid && len(accountIDs) > 0 && len(providers) == 0
	case store.UserUpstreamAccessBatchAll:
		valid = valid && len(accountIDs) == 0 && len(providers) > 0
	default:
		valid = false
	}
	if !valid {
		s.storeWriteError(w, r, "set user upstream access batch", store.ErrInvalid)
		return
	}
	result, err := s.store.SetUserUpstreamAccessBatch(r.Context(), store.SetUserUpstreamAccessBatchParams{
		UserIDs: userIDs, Action: action, AccountIDs: accountIDs, Providers: providers, Reason: reason, At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set user upstream access batch", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
