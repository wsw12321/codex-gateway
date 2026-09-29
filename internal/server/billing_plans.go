package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type billingPlanRepository interface {
	ListBillingPlans(context.Context, bool) ([]store.BillingPlan, error)
	CreateBillingPlan(context.Context, store.CreateBillingPlanParams) (store.BillingPlan, error)
	UpdateBillingPlan(context.Context, store.UpdateBillingPlanParams) (store.BillingPlan, error)
	PurchaseBillingPlan(context.Context, store.PurchaseBillingPlanParams) (store.BillingPlanTransaction, error)
	RenewBillingSubscription(context.Context, store.RenewBillingSubscriptionParams) (store.BillingPlanTransaction, error)
}

func (s *Server) billingPlanStorage() billingPlanRepository {
	if s.billingPlanRepo != nil {
		return s.billingPlanRepo
	}
	return s.store
}

func (s *Server) billingPlans(w http.ResponseWriter, r *http.Request) {
	includeInactive := false
	query, err := parseBillingPlanQuery(r)
	if err != nil {
		s.billingInputError(w, r)
		return
	}
	if query {
		if userFrom(r.Context()).Role != store.UserRoleOwner {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "owner_required", "仅 Owner 可查询下架套餐")
			return
		}
		includeInactive = true
	}
	plans, err := s.billingPlanStorage().ListBillingPlans(r.Context(), includeInactive)
	if err != nil {
		s.billingStoreError(w, r, "list billing plans", err)
		return
	}
	if plans == nil {
		plans = []store.BillingPlan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

func parseBillingPlanQuery(r *http.Request) (bool, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false, errInvalidBillingOperation
	}
	if len(query) == 0 && r.URL.RawQuery == "" {
		return false, nil
	}
	values, exists := query["include_inactive"]
	if len(query) != 1 || !exists || len(values) != 1 {
		return false, errInvalidBillingOperation
	}
	switch values[0] {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errInvalidBillingOperation
	}
}

type billingPlanInput struct {
	billingOperationInput
	Name           string
	PriceUSD       string
	Tier           string
	AllowanceUSD   string
	MinPeriodCount *int
	Active         *bool
	Version        *int64
}

func (s *Server) decodeBillingPlan(w http.ResponseWriter, r *http.Request, updating bool) (billingPlanInput, bool) {
	var input billingPlanInput
	fields := map[string]any{
		"operation_id": &input.OperationID, "reason": &input.Reason,
		"name": &input.Name, "price_usd": &input.PriceUSD,
		"tier": &input.Tier, "allowance_usd": &input.AllowanceUSD,
		"min_period_count": &input.MinPeriodCount, "active": &input.Active,
	}
	if updating {
		fields["version"] = &input.Version
	}
	if !s.decodeBillingPlanFields(w, r, fields) {
		return input, false
	}
	if validateBillingOperation(input.OperationID, input.Reason) != nil ||
		(input.MinPeriodCount != nil && (*input.MinPeriodCount < 1 || *input.MinPeriodCount > 99)) ||
		(updating && (input.Version == nil || *input.Version < 1 || input.MinPeriodCount == nil || input.Active == nil)) {
		s.billingInputError(w, r)
		return input, false
	}
	if _, err := parseBillingTier(input.Tier); err != nil {
		s.billingInputError(w, r)
		return input, false
	}
	return input, true
}

func (s *Server) createBillingPlan(w http.ResponseWriter, r *http.Request) {
	input, ok := s.decodeBillingPlan(w, r, false)
	if !ok {
		return
	}
	minPeriodCount, active := 1, false
	if input.MinPeriodCount != nil {
		minPeriodCount = *input.MinPeriodCount
	}
	if input.Active != nil {
		active = *input.Active
	}
	plan, err := s.billingPlanStorage().CreateBillingPlan(r.Context(), store.CreateBillingPlanParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		Name:               input.Name, PriceUSD: input.PriceUSD, Tier: input.Tier, AllowanceUSD: input.AllowanceUSD,
		MinPeriodCount: minPeriodCount, Active: active,
	})
	if err != nil {
		s.billingStoreError(w, r, "create billing plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) updateBillingPlan(w http.ResponseWriter, r *http.Request) {
	input, ok := s.decodeBillingPlan(w, r, true)
	if !ok {
		return
	}
	plan, err := s.billingPlanStorage().UpdateBillingPlan(r.Context(), store.UpdateBillingPlanParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		PlanID:             r.PathValue("plan_id"), Version: *input.Version,
		Name: input.Name, PriceUSD: input.PriceUSD, Tier: input.Tier, AllowanceUSD: input.AllowanceUSD,
		MinPeriodCount: *input.MinPeriodCount, Active: *input.Active,
	})
	if err != nil {
		s.billingStoreError(w, r, "update billing plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type billingPlanTransactionInput struct {
	OperationID               string
	PlanID                    string
	PlanVersion               *int64
	SubscriptionConfigVersion *int64
	PeriodCount               *int
}

func (s *Server) decodeBillingPlanTransaction(w http.ResponseWriter, r *http.Request, purchasing bool) (billingPlanTransactionInput, bool) {
	var input billingPlanTransactionInput
	fields := map[string]any{
		"operation_id": &input.OperationID, "plan_version": &input.PlanVersion,
		"subscription_config_version": &input.SubscriptionConfigVersion, "period_count": &input.PeriodCount,
	}
	if purchasing {
		fields["plan_id"] = &input.PlanID
	}
	if !s.decodeBillingPlanFields(w, r, fields) {
		return input, false
	}
	if validateBillingOperation(input.OperationID, "套餐交易") != nil ||
		input.PlanVersion == nil || *input.PlanVersion < 1 ||
		input.SubscriptionConfigVersion == nil || *input.SubscriptionConfigVersion < 0 ||
		(!purchasing && *input.SubscriptionConfigVersion == 0) ||
		input.PeriodCount == nil || *input.PeriodCount < 1 || *input.PeriodCount > 99 ||
		(purchasing && input.PlanID == "") {
		s.billingInputError(w, r)
		return input, false
	}
	return input, true
}

func (s *Server) purchaseBillingPlan(w http.ResponseWriter, r *http.Request) {
	input, ok := s.decodeBillingPlanTransaction(w, r, true)
	if !ok {
		return
	}
	result, err := s.billingPlanStorage().PurchaseBillingPlan(r.Context(), store.PurchaseBillingPlanParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, "用户购买套餐并重开订阅"),
		UserID:             userFrom(r.Context()).ID, PlanID: input.PlanID, PlanVersion: *input.PlanVersion,
		SubscriptionConfigVersion: *input.SubscriptionConfigVersion, PeriodCount: *input.PeriodCount,
	})
	if err != nil {
		s.billingStoreError(w, r, "purchase billing plan", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) renewBillingSubscription(w http.ResponseWriter, r *http.Request) {
	tier, err := parseBillingTier(r.PathValue("tier"))
	if err != nil {
		s.billingInputError(w, r)
		return
	}
	input, ok := s.decodeBillingPlanTransaction(w, r, false)
	if !ok {
		return
	}
	result, err := s.billingPlanStorage().RenewBillingSubscription(r.Context(), store.RenewBillingSubscriptionParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, "用户续费已绑定套餐"),
		UserID:             userFrom(r.Context()).ID, Tier: tier, PlanVersion: *input.PlanVersion,
		SubscriptionConfigVersion: *input.SubscriptionConfigVersion, PeriodCount: *input.PeriodCount,
	})
	if err != nil {
		s.billingStoreError(w, r, "renew billing subscription", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Exact names and duplicate rejection prevent alternate account, amount or
// confirmation fields from being silently accepted on financial writes.
func (s *Server) decodeBillingPlanFields(w http.ResponseWriter, r *http.Request, fields map[string]any) bool {
	if !strictJSONRequest(r) {
		s.billingInputError(w, r)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	decoder := json.NewDecoder(r.Body)
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		badJSON(w, r, err)
		return false
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || fields[name] == nil || seen[name] {
			badJSON(w, r, err)
			return false
		}
		seen[name] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			badJSON(w, r, err)
			return false
		}
		if string(raw) == "null" {
			badJSON(w, r, errors.New("null field"))
			return false
		}
		if err := json.Unmarshal(raw, fields[name]); err != nil {
			badJSON(w, r, err)
			return false
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		badJSON(w, r, err)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		badJSON(w, r, err)
		return false
	}
	return true
}
