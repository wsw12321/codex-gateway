package server

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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

// cashModelsInput updates the single, owner-managed model range that may be
// funded from cash balances. The store validates the model list against the
// current manageable catalog while preserving the billing operation's
// idempotency and audit trail.
type cashModelsInput struct {
	billingOperationInput
	ModelIDs []string `json:"model_ids"`
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
	Name        string   `json:"name"`
	Tier        string   `json:"tier"`
	PeriodDays  int      `json:"period_days"`
	ModelIDs    []string `json:"model_ids"`
	QuotaUSD    string   `json:"quota_usd"`
	PeriodCount *int     `json:"period_count"`
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
		"cash_balance_usd":           state.BalanceUSD,
		"source_disabled":            state.SourceDisabled,
		"cash_preferences":           state.CashPreferences,
		"group":                      state.Group,
		"groups":                     billingGroupsResponse(state.Groups),
		"group_member_used_usd":      state.GroupMemberUsedUSD,
		"group_member_remaining_usd": state.GroupMemberRemainingUSD,
		"subscriptions":              billingSubscriptionsResponse(state.Subscriptions),
		"ledger_entries":             state.Ledger,
		"pagination": map[string]any{
			"limit": limit, "offset": offset, "total": state.LedgerTotal,
			"has_more": hasMore, "next_offset": offset + len(state.Ledger),
		},
	}
}

func billingGroupsResponse(values []store.BillingGroupState) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		item := map[string]any{
			"id": value.ID, "name": value.Name, "limit_usd": value.LimitUSD, "member_limit_usd": value.MemberLimitUSD,
			"used_usd": value.UsedUSD, "remaining_usd": value.RemainingUSD, "period": value.Period, "custom_days": value.CustomDays,
			"starts_at": value.StartsAt, "period_id": value.PeriodID, "period_starts_at": value.PeriodStartsAt, "period_ends_at": value.PeriodEndsAt,
			"period_count": value.PeriodCount, "current_period_number": value.CurrentPeriodNumber, "expires_at": value.ExpiresAt,
			"member_count": value.MemberCount, "archived_at": value.ArchivedAt, "created_at": value.CreatedAt, "updated_at": value.UpdatedAt,
			"model_ids": value.ModelIDs, "priority": value.Priority, "member_used_usd": value.MemberUsedUSD, "member_remaining_usd": value.MemberRemainingUSD,
		}
		result = append(result, item)
	}
	return result
}

func billingSubscriptionsResponse(values []store.BillingSubscriptionState) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]any{
			"id": value.ID, "tier": value.Tier, "enabled": value.Enabled,
			"name": value.Name, "period_days": value.PeriodDays, "model_ids": value.ModelIDs,
			"priority": value.Priority, "charge_enabled": value.ChargeEnabled, "entitlements": value.Entitlements,
			"quota_usd": value.AllowanceUSD, "remaining_usd": value.RemainingUSD,
			"period_count": value.PeriodCount, "current_period_number": value.CurrentPeriodNumber,
			"expires_at": value.ExpiresAt,
			"period_id":  value.PeriodID, "period_started_at": value.PeriodStartsAt,
			"period_ends_at": value.PeriodEndsAt, "updated_at": value.UpdatedAt,
			"plan": value.Plan, "can_renew": value.CanRenew, "config_version": value.ConfigVersion,
		})
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

func (s *Server) updateCashModels(w http.ResponseWriter, r *http.Request) {
	var input cashModelsInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	settings, err := s.store.SetCashModelIDs(r.Context(), store.SetCashModelIDsParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		ModelIDs:           input.ModelIDs,
	})
	if err != nil {
		s.billingStoreError(w, r, "set cash model range", err)
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
		item := map[string]any{
			"id": user.UserID, "username": user.Username, "display_name": user.DisplayName,
			"role": user.Role, "status": user.Status, "cash_balance_usd": user.BalanceUSD,
			"group_id":         user.GroupID,
			"group_ids":        user.GroupIDs,
			"subscriptions":    billingSubscriptionsResponse(user.Subscriptions),
			"source_disabled":  user.SourceDisabled,
			"cash_preferences": user.CashPreferences,
		}
		addUserSearchFields(item, user.Username, user.DisplayName)
		items = append(items, item)
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
	subscriptionID := r.PathValue("subscription_id")
	if r.Method == http.MethodPut && !validBillingResourceID(subscriptionID) {
		s.billingInputError(w, r)
		return
	}
	value, err := s.billingSubscriptionStorage().PutSubscription(r.Context(), store.PutSubscriptionParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		UserID:             r.PathValue("user_id"), SubscriptionID: subscriptionID,
		Name: input.Name, Tier: input.Tier, PeriodDays: input.PeriodDays, ModelIDs: input.ModelIDs, AllowanceUSD: input.QuotaUSD, PeriodCount: *input.PeriodCount,
	})
	if err != nil {
		s.billingStoreError(w, r, "put billing subscription", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) deleteBillingSubscription(w http.ResponseWriter, r *http.Request) {
	var input billingOperationInput
	if !s.decodeBillingPlanFields(w, r, map[string]any{"operation_id": &input.OperationID, "reason": &input.Reason}) {
		return
	}
	if validateBillingOperation(input.OperationID, input.Reason) != nil {
		s.billingInputError(w, r)
		return
	}
	subscriptionID := r.PathValue("subscription_id")
	if !validBillingResourceID(subscriptionID) {
		s.billingInputError(w, r)
		return
	}
	value, err := s.billingSubscriptionStorage().DeleteSubscription(r.Context(), store.DeleteSubscriptionParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		UserID:             r.PathValue("user_id"), SubscriptionID: subscriptionID,
	})
	if err != nil {
		s.billingStoreError(w, r, "delete billing subscription", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) decodeBillingWrite(w http.ResponseWriter, r *http.Request, destination any) bool {
	if value, ok := destination.(*subscriptionInput); ok {
		if !s.decodeBillingPlanFields(w, r, map[string]any{"operation_id": &value.OperationID, "reason": &value.Reason, "name": &value.Name, "tier": &value.Tier, "period_days": &value.PeriodDays, "model_ids": &value.ModelIDs, "quota_usd": &value.QuotaUSD, "period_count": &value.PeriodCount}) {
			return false
		}
	} else if err := decodeJSON(w, r, destination, 32<<10); err != nil {
		badJSON(w, r, err)
		return false
	}
	var operation billingOperationInput
	switch value := destination.(type) {
	case *billingOperationInput:
		operation = *value
	case *rechargeRateInput:
		operation = value.billingOperationInput
	case *modelMultiplierInput:
		operation = value.billingOperationInput
	case *modelPriceInput:
		operation = value.billingOperationInput
	case *rechargeUserInput:
		operation = value.billingOperationInput
	case *adjustmentInput:
		operation = value.billingOperationInput
	case *cashModelsInput:
		operation = value.billingOperationInput
		if !s.validManageableBillingModels(value.ModelIDs) {
			s.billingInputError(w, r)
			return false
		}
	case *subscriptionInput:
		operation = value.billingOperationInput
		if value.PeriodCount == nil || *value.PeriodCount < 0 || *value.PeriodCount > 99 ||
			strings.TrimSpace(value.Name) == "" || utf8.RuneCountInString(strings.TrimSpace(value.Name)) > 100 ||
			!validBillingPeriod(value.Tier, value.PeriodDays) || !s.validBillingModels(value.ModelIDs) {
			s.billingInputError(w, r)
			return false
		}
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
	case errors.Is(err, store.ErrBillingOperationCleaned):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "billing_operation_cleaned", "此 operation_id 的历史账务已清理，禁止再次执行；请为新操作使用新的 ID")
	case errors.Is(err, store.ErrBillingPlanChanged):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "billing_plan_changed", "套餐或订阅配置已变更，或续费绑定已失效；请刷新后重新确认")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "billing_operation_conflict", "operation_id 已用于不同的账务操作")
	case errors.Is(err, store.ErrInvalid):
		s.billingInputError(w, r)
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "billing_resource_not_found", "账务用户、套餐或订阅不存在")
	default:
		internalError(s, w, r, operation, err)
	}
}
