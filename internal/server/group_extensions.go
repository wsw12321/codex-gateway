package server

import (
	"net/http"
	"strings"

	"github.com/wsw/codex-gateway/internal/store"
)

type groupPriorityInput struct {
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
	Priority    int    `json:"priority"`
}
type groupUpstreamInput struct {
	OperationID string   `json:"operation_id"`
	Reason      string   `json:"reason"`
	Strategy    string   `json:"strategy"`
	Mode        string   `json:"mode"`
	AccountIDs  []string `json:"account_ids"`
}

func (s *Server) setMyGroupPreference(w http.ResponseWriter, r *http.Request) {
	var in groupPriorityInput
	if err := decodeJSON(w, r, &in, 16<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	if validateBillingOperation(in.OperationID, in.Reason) != nil || in.Priority < 1 || in.Priority > 100 {
		s.billingInputError(w, r)
		return
	}
	value, err := s.store.SetGroupMembershipPriority(r.Context(), store.SetGroupMembershipPreferenceParams{BillingWriteParams: s.billingWriteParams(r, in.OperationID, strings.TrimSpace(in.Reason)), GroupID: r.PathValue("group_id"), UserID: userFrom(r.Context()).ID, Priority: in.Priority})
	if err != nil {
		s.billingStoreError(w, r, "set group preference", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) setGroupUpstreamPolicy(w http.ResponseWriter, r *http.Request) {
	var in groupUpstreamInput
	if err := decodeJSON(w, r, &in, 64<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	if validateBillingOperation(in.OperationID, in.Reason) != nil {
		s.billingInputError(w, r)
		return
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = in.Mode
	}
	value, err := s.store.SetGroupUpstreamAccountPolicy(r.Context(), store.SetGroupUpstreamAccountPolicyParams{BillingWriteParams: s.billingWriteParams(r, in.OperationID, strings.TrimSpace(in.Reason)), GroupID: r.PathValue("id"), Provider: r.PathValue("provider"), Strategy: strategy, AccountIDs: in.AccountIDs})
	if err != nil {
		s.groupStoreError(w, r, "set group upstream policy", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

type groupUpstreamAccessProvider struct {
	store.GroupUpstreamAccountPolicy
	Accounts    []userUpstreamAccountOption `json:"accounts"`
	SyncWarning string                      `json:"sync_warning,omitempty"`
}

type groupUpstreamAccessResponse struct {
	GroupID   string                        `json:"group_id"`
	Providers []groupUpstreamAccessProvider `json:"providers"`
}

// groupUpstreamAccessJSON returns every provider's group account pool with
// the selectable accounts in the same shape as user upstream access, so the
// dashboard edits both with one scope editor.
func (s *Server) groupUpstreamAccessJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validGroupID(id) {
		s.groupStoreError(w, r, "read group upstream access", store.ErrNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		s.groupStoreError(w, r, "read group upstream access", store.ErrInvalid)
		return
	}
	response := groupUpstreamAccessResponse{GroupID: id, Providers: make([]groupUpstreamAccessProvider, 0, 3)}
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		policy, err := s.store.GetGroupUpstreamAccounts(r.Context(), id, provider)
		if err != nil {
			s.groupStoreError(w, r, "read group upstream access", err)
			return
		}
		response.Providers = append(response.Providers, groupUpstreamAccessProvider{GroupUpstreamAccountPolicy: policy})
	}

	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	for i := range response.Providers {
		entry := &response.Providers[i]
		accounts, warning, err := s.upstreamAccessAccountOptionsLocked(r.Context(), entry.Provider)
		if err != nil {
			internalError(s, w, r, "list group upstream account options", err)
			return
		}
		entry.Accounts, entry.SyncWarning = accounts, warning
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) groupUpstreamPolicy(w http.ResponseWriter, r *http.Request) {
	value, err := s.store.GetGroupUpstreamAccounts(r.Context(), r.PathValue("id"), r.PathValue("provider"))
	if err != nil {
		s.groupStoreError(w, r, "get group upstream policy", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) groupPeriods(w http.ResponseWriter, r *http.Request) {
	values, err := s.store.ListGroupUsagePeriods(r.Context(), r.PathValue("id"), 50)
	if err != nil {
		s.groupStoreError(w, r, "list group periods", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"periods": values})
}

func (s *Server) groupAccountStats(w http.ResponseWriter, r *http.Request) {
	ids := append([]string(nil), r.URL.Query()["period_id"]...)
	if len(ids) == 0 {
		if raw := r.URL.Query().Get("period_ids"); raw != "" {
			ids = strings.Split(raw, ",")
		}
	}
	values, err := s.store.GroupAccountStats(r.Context(), r.PathValue("id"), ids)
	if err != nil {
		s.groupStoreError(w, r, "group account statistics", err)
		return
	}
	complete := true
	for _, v := range values {
		if !v.DataComplete {
			complete = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"group_id": r.PathValue("id"), "stats": values, "accounts": values, "data_complete": complete})
}
