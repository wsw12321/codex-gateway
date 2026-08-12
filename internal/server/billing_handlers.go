package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type billingOperationInput struct {
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
}

type rechargeRateInput struct {
	billingOperationInput
	USDPerCNY string `json:"usd_per_cny"`
}

type rechargeUserInput struct {
	billingOperationInput
	CNYAmount string `json:"cny_amount"`
}

type adjustmentInput struct {
	billingOperationInput
	USDAmount string `json:"usd_amount"`
}

type subscriptionInput struct {
	billingOperationInput
	QuotaUSD string `json:"quota_usd"`
}

func (s *Server) billingMe(w http.ResponseWriter, r *http.Request) {
	s.writeBillingState(w, r, userFrom(r.Context()).ID)
}

func (s *Server) billingUser(w http.ResponseWriter, r *http.Request) {
	s.writeBillingState(w, r, r.PathValue("user_id"))
}

func (s *Server) writeBillingState(w http.ResponseWriter, r *http.Request, userID string) {
	limit, offset, err := parseBillingPagination(r.URL.Query())
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_pagination", "分页参数无效")
		return
	}
	state, err := s.store.GetBillingState(r.Context(), userID, limit, offset)
	if err != nil {
		s.billingStoreError(w, r, "get billing state", err)
		return
	}
	writeJSON(w, http.StatusOK, billingStateResponse(state, limit, offset))
}

func billingStateResponse(state store.BillingState, limit, offset int) map[string]any {
	hasMore := int64(offset+len(state.Ledger)) < state.LedgerTotal
	return map[string]any{
		"user": map[string]any{
			"id": state.UserID, "username": state.Username, "display_name": state.DisplayName,
		},
		"cash_balance_usd": state.BalanceUSD,
		"subscriptions":    billingSubscriptionsResponse(state.Subscriptions),
		"ledger_entries":   state.Ledger,
		"pagination": map[string]any{
			"limit": limit, "offset": offset, "total": state.LedgerTotal,
			"has_more": hasMore, "next_offset": offset + len(state.Ledger),
		},
	}
}

func billingSubscriptionsResponse(values []store.BillingSubscriptionState) map[string]map[string]any {
	result := make(map[string]map[string]any, 3)
	for _, value := range values {
		result[value.Tier] = map[string]any{
			"id": value.ID, "tier": value.Tier, "enabled": value.Enabled,
			"quota_usd": value.AllowanceUSD, "remaining_usd": value.RemainingUSD,
			"period_id": value.PeriodID, "period_started_at": value.PeriodStartsAt,
			"period_ends_at": value.PeriodEndsAt, "updated_at": value.UpdatedAt,
		}
	}
	return result
}

func (s *Server) billingSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetBillingSettings(r.Context())
	if err != nil {
		s.billingStoreError(w, r, "get billing settings", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) billingUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListBillingUsers(r.Context())
	if err != nil {
		s.billingStoreError(w, r, "list billing users", err)
		return
	}
	items := make([]map[string]any, 0, len(users))
	for _, user := range users {
		items = append(items, map[string]any{
			"id": user.UserID, "username": user.Username, "display_name": user.DisplayName,
			"role": user.Role, "status": user.Status, "cash_balance_usd": user.BalanceUSD,
			"subscriptions": billingSubscriptionsResponse(user.Subscriptions),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": items})
}

func (s *Server) updateRechargeRate(w http.ResponseWriter, r *http.Request) {
	var input rechargeRateInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	writeParams := s.billingWriteParams(r, input.OperationID, input.Reason)
	settings, err := s.store.SetRechargeRate(r.Context(), store.SetRechargeRateParams{
		BillingWriteParams: writeParams, USDPerCNY: input.USDPerCNY,
	})
	if err != nil {
		s.billingStoreError(w, r, "set recharge rate", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) rechargeBillingUser(w http.ResponseWriter, r *http.Request) {
	var input rechargeUserInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	entry, err := s.store.RechargeUser(r.Context(), store.RechargeUserParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		UserID:             r.PathValue("user_id"), CNYAmount: input.CNYAmount,
	})
	if err != nil {
		s.billingStoreError(w, r, "recharge billing user", err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) adjustBillingUser(w http.ResponseWriter, r *http.Request) {
	var input adjustmentInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	entry, err := s.store.AdjustUserBalance(r.Context(), store.AdjustUserBalanceParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		UserID:             r.PathValue("user_id"), USDAmount: input.USDAmount,
	})
	if err != nil {
		s.billingStoreError(w, r, "adjust billing user", err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) putBillingSubscription(w http.ResponseWriter, r *http.Request) {
	var input subscriptionInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	tier, err := parseBillingTier(r.PathValue("tier"))
	if err != nil {
		s.billingInputError(w, r)
		return
	}
	value, err := s.store.PutSubscription(r.Context(), store.PutSubscriptionParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		UserID:             r.PathValue("user_id"), Tier: tier, AllowanceUSD: input.QuotaUSD,
	})
	if err != nil {
		s.billingStoreError(w, r, "put billing subscription", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) deleteBillingSubscription(w http.ResponseWriter, r *http.Request) {
	var input billingOperationInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	tier, err := parseBillingTier(r.PathValue("tier"))
	if err != nil {
		s.billingInputError(w, r)
		return
	}
	value, err := s.store.DeleteSubscription(r.Context(), store.DeleteSubscriptionParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		UserID:             r.PathValue("user_id"), Tier: tier,
	})
	if err != nil {
		s.billingStoreError(w, r, "delete billing subscription", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) decodeBillingWrite(w http.ResponseWriter, r *http.Request, destination any) bool {
	if err := decodeJSON(w, r, destination, 32<<10); err != nil {
		badJSON(w, r, err)
		return false
	}
	var operation billingOperationInput
	switch value := destination.(type) {
	case *billingOperationInput:
		operation = *value
	case *rechargeRateInput:
		operation = value.billingOperationInput
	case *rechargeUserInput:
		operation = value.billingOperationInput
	case *adjustmentInput:
		operation = value.billingOperationInput
	case *subscriptionInput:
		operation = value.billingOperationInput
	default:
		internalError(s, w, r, "decode billing operation", errInvalidBillingOperation)
		return false
	}
	if err := validateBillingOperation(operation.OperationID, operation.Reason); err != nil {
		s.billingInputError(w, r)
		return false
	}
	return true
}

func (s *Server) billingWriteParams(r *http.Request, operationID, reason string) store.BillingWriteParams {
	return store.BillingWriteParams{
		OperationID: operationID, Reason: reason, ActorUserID: userFrom(r.Context()).ID,
		ActorSessionID: sessionFrom(r.Context()).ID, RequestID: httpx.RequestID(r.Context()),
		SourceIP: safeIP(r.Context()), At: time.Now().UTC(),
	}
}

func (s *Server) billingInputError(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_billing_operation", "账务操作参数无效")
}

func (s *Server) billingStoreError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var insufficient *store.InsufficientFundsError
	switch {
	case errors.As(err, &insufficient):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "insufficient_balance", "现金余额不足")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "billing_operation_conflict", "operation_id 已用于不同的账务操作")
	case errors.Is(err, store.ErrInvalid):
		s.billingInputError(w, r)
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "billing_resource_not_found", "账务用户或订阅不存在")
	default:
		internalError(s, w, r, operation, err)
	}
}
