package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

// Aggregation gateway stage 4: seller plans, the Owner's review, the plan
// marketplace and sales. Every route here answers 404 while the marketplace
// is off. Sellers never see who bought their plans.

type marketPlanRepository interface {
	MarketPlans(context.Context, string) ([]store.MarketPlan, error)
	SellerPlans(context.Context, string) ([]store.SellerPlan, error)
	SellerPlan(context.Context, string, string) (store.SellerPlan, error)
	CreateSellerPlan(context.Context, store.SellerPlanParams) (store.SellerPlan, error)
	UpdateSellerPlan(context.Context, store.SellerPlanParams) (store.SellerPlan, error)
	SetSellerPlanListing(context.Context, store.SetSellerPlanListingParams) (store.SellerPlan, error)
	SubmitSellerPlan(context.Context, store.PlanReviewWriteParams) (store.SellerPlan, error)
	DeleteBillingPlan(context.Context, store.DeleteBillingPlanParams) (store.BillingPlan, error)
	SellerSales(context.Context, string, int, int) (store.MarketplaceSales, error)
	SellerPlanUsage(context.Context, string, string) (store.SellerPlanUsage, error)
	PendingSellerPlans(context.Context) ([]store.SellerPlan, error)
	AllSellerPlans(context.Context) ([]store.SellerPlan, error)
	ReviewSellerPlan(context.Context, store.ReviewSellerPlanParams) (store.SellerPlan, error)
	SetSellerPlanSuspended(context.Context, store.SetSellerPlanSuspendedParams) (store.SellerPlan, error)
	AllMarketplaceSales(context.Context, int, int) (store.MarketplaceSales, error)
}

func (s *Server) marketPlanStorage() marketPlanRepository {
	if s.marketPlanRepo != nil {
		return s.marketPlanRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

func (s *Server) marketplacePlanRoutes() {
	market := func(next http.HandlerFunc) http.Handler { return s.requireMarketplaceOn(next) }
	read := func(next http.HandlerFunc) http.Handler { return s.requireSession(market(next)) }
	write := func(next http.HandlerFunc) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(market(next)))
	}
	s.mux.Handle("GET /admin/market/plans", read(s.marketPlans))
	s.mux.Handle("GET /admin/me/plans", read(s.sellerOnly(s.mySellerPlans)))
	s.mux.Handle("POST /admin/me/plans", write(s.sellerOnly(s.createMySellerPlan)))
	s.mux.Handle("PUT /admin/me/plans/{plan_id}", write(s.sellerOnly(s.updateMySellerPlan)))
	s.mux.Handle("DELETE /admin/me/plans/{plan_id}", write(s.sellerOnly(s.deleteMySellerPlan)))
	s.mux.Handle("PUT /admin/me/plans/{plan_id}/provider-scope", write(s.sellerOnly(s.setMySellerPlanScope)))
	s.mux.Handle("POST /admin/me/plans/{plan_id}/submit", write(s.sellerOnly(s.submitMySellerPlan)))
	s.mux.Handle("PUT /admin/me/plans/{plan_id}/listing", write(s.sellerOnly(s.setMySellerPlanListing)))
	s.mux.Handle("GET /admin/me/plans/{plan_id}/usage", read(s.sellerOnly(s.mySellerPlanUsage)))
	s.mux.Handle("GET /admin/me/sales", read(s.sellerOnly(s.mySales)))
	s.mux.Handle("GET /admin/market/reviews", s.requireSession(s.ownerOnly(market(s.marketReviews))))
	s.mux.Handle("POST /admin/market/reviews/{plan_id}", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(market(s.reviewSellerPlan)))))
	s.mux.Handle("PUT /admin/market/plans/{plan_id}/suspend", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(market(s.suspendSellerPlan)))))
	s.mux.Handle("GET /admin/market/sales", s.requireSession(s.ownerOnly(market(s.allMarketSales))))
}

// sellerOnly keeps the seller routes for members: the Owner's plans are
// platform plans and live on the Owner's plan routes.
func (s *Server) sellerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()).Role != store.UserRoleMember {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "marketplace_member_only", "Owner 的套餐是平台套餐，请在套餐管理中维护")
			return
		}
		next(w, r)
	}
}

func (s *Server) marketplacePlanError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, store.ErrMarketplaceDisabled):
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "marketplace_disabled", "商城功能未开启")
	case errors.Is(err, store.ErrSellerPlanLimit):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "seller_plan_limit", "已达到可发布套餐的数量上限")
	case errors.Is(err, store.ErrPlanNotOnSale):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "plan_not_on_sale", "套餐当前未上架，无法购买或续费")
	case errors.Is(err, store.ErrSelfPurchase):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "self_purchase", "不能购买自己发布的套餐")
	case errors.Is(err, store.ErrThirdPartyAcknowledgement):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "third_party_acknowledgement_required", "该套餐可能由成员的第三方 API 供应商提供服务，对方能看到相应请求内容；请确认后再购买")
	case errors.Is(err, store.ErrSellerPlanSuspended):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "seller_plan_suspended", "套餐已被 Owner 下架，暂不能重新上架")
	case errors.Is(err, store.ErrPlanReviewState):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "plan_review_state", "套餐当前的审核状态不允许此操作")
	default:
		s.billingStoreError(w, r, operation, err)
	}
}

func (s *Server) marketPlans(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.billingInputError(w, r)
		return
	}
	plans, err := s.marketPlanStorage().MarketPlans(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		internalError(s, w, r, "list market plans", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

func (s *Server) mySellerPlans(w http.ResponseWriter, r *http.Request) {
	seller := userFrom(r.Context()).ID
	plans, err := s.marketPlanStorage().SellerPlans(r.Context(), seller)
	if err != nil {
		internalError(s, w, r, "list seller plans", err)
		return
	}
	scopes, err := s.planScopeStorage().PlanProviderScopes(r.Context(), seller)
	if err != nil {
		internalError(s, w, r, "list seller plan scopes", err)
		return
	}
	candidates, err := s.planScopeStorage().PlanScopeCandidates(r.Context(), seller)
	if err != nil {
		internalError(s, w, r, "list seller plan scope candidates", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans, "scopes": scopes, "candidates": candidates})
}

type sellerPlanInput struct {
	OperationID    string
	Name           string
	Description    string
	PriceUSD       string
	Tier           string
	PeriodDays     int
	AllowanceUSD   string
	ModelIDs       []string
	MinPeriodCount *int
	Active         *bool
	Version        *int64
}

func (s *Server) decodeSellerPlan(w http.ResponseWriter, r *http.Request, updating bool) (sellerPlanInput, bool) {
	var input sellerPlanInput
	fields := map[string]any{
		"operation_id": &input.OperationID, "name": &input.Name, "description": &input.Description,
		"price_usd": &input.PriceUSD, "tier": &input.Tier, "period_days": &input.PeriodDays,
		"allowance_usd": &input.AllowanceUSD, "model_ids": &input.ModelIDs,
		"min_period_count": &input.MinPeriodCount, "active": &input.Active,
	}
	if updating {
		fields["version"] = &input.Version
	}
	if !s.decodeBillingPlanFields(w, r, fields) {
		return input, false
	}
	if validateBillingOperation(input.OperationID, "卖家套餐") != nil || input.MinPeriodCount == nil || input.Active == nil ||
		*input.MinPeriodCount < 1 || *input.MinPeriodCount > 99 || (updating && (input.Version == nil || *input.Version < 1)) ||
		!validBillingPeriod(input.Tier, input.PeriodDays) || !s.validBillingModels(input.ModelIDs) {
		s.billingInputError(w, r)
		return input, false
	}
	return input, true
}

func (input sellerPlanInput) params(write store.BillingWriteParams, planID string) store.SellerPlanParams {
	params := store.SellerPlanParams{BillingWriteParams: write, PlanID: planID, Name: input.Name, Description: input.Description,
		PriceUSD: input.PriceUSD, Tier: input.Tier, PeriodDays: input.PeriodDays, AllowanceUSD: input.AllowanceUSD,
		ModelIDs: input.ModelIDs, MinPeriodCount: *input.MinPeriodCount, Active: *input.Active}
	if input.Version != nil {
		params.Version = *input.Version
	}
	return params
}

func (s *Server) createMySellerPlan(w http.ResponseWriter, r *http.Request) {
	input, ok := s.decodeSellerPlan(w, r, false)
	if !ok {
		return
	}
	plan, err := s.marketPlanStorage().CreateSellerPlan(r.Context(), input.params(s.billingWriteParams(r, input.OperationID, "成员发布套餐"), ""))
	if err != nil {
		s.marketplacePlanError(w, r, "create seller plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) updateMySellerPlan(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	if !validBillingResourceID(planID) {
		s.billingInputError(w, r)
		return
	}
	input, ok := s.decodeSellerPlan(w, r, true)
	if !ok {
		return
	}
	plan, err := s.marketPlanStorage().UpdateSellerPlan(r.Context(), input.params(s.billingWriteParams(r, input.OperationID, "成员修改套餐"), planID))
	if err != nil {
		s.marketplacePlanError(w, r, "update seller plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) deleteMySellerPlan(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	if !validBillingResourceID(planID) {
		s.billingInputError(w, r)
		return
	}
	var operationID string
	var version *int64
	if !s.decodeBillingPlanFields(w, r, map[string]any{"operation_id": &operationID, "version": &version}) {
		return
	}
	if validateBillingOperation(operationID, "卖家套餐") != nil || version == nil || *version < 1 {
		s.billingInputError(w, r)
		return
	}
	user := userFrom(r.Context())
	plan, err := s.marketPlanStorage().DeleteBillingPlan(r.Context(), store.DeleteBillingPlanParams{
		BillingWriteParams: s.billingWriteParams(r, operationID, "成员删除套餐"), PlanID: planID, Version: *version, SellerUserID: user.ID,
	})
	if err != nil {
		s.marketplacePlanError(w, r, "delete seller plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) setMySellerPlanScope(w http.ResponseWriter, r *http.Request) {
	s.setPlanScope(w, r, userFrom(r.Context()).ID)
}

func (s *Server) submitMySellerPlan(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	if !validBillingResourceID(planID) {
		s.billingInputError(w, r)
		return
	}
	var version *int64
	if !strictJSONRequest(r) {
		s.billingInputError(w, r)
		return
	}
	if err := decodeSingleJSONField(r, 256, "version", &version); err != nil || version == nil || *version < 1 {
		if err == nil {
			err = errors.New("version is required")
		}
		badJSON(w, r, err)
		return
	}
	plan, err := s.marketPlanStorage().SubmitSellerPlan(r.Context(), s.planReviewWrite(r, planID, *version))
	if err != nil {
		s.marketplacePlanError(w, r, "submit seller plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) planReviewWrite(r *http.Request, planID string, version int64) store.PlanReviewWriteParams {
	return store.PlanReviewWriteParams{PlanID: planID, Version: version, ActorUserID: userFrom(r.Context()).ID,
		ActorSessionID: sessionFrom(r.Context()).ID, RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
		At: time.Now().UTC()}
}

func (s *Server) setMySellerPlanListing(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	if !validBillingResourceID(planID) {
		s.billingInputError(w, r)
		return
	}
	var operationID string
	var version *int64
	var active *bool
	if !s.decodeBillingPlanFields(w, r, map[string]any{"operation_id": &operationID, "version": &version, "active": &active}) {
		return
	}
	if validateBillingOperation(operationID, "卖家套餐") != nil || version == nil || *version < 1 || active == nil {
		s.billingInputError(w, r)
		return
	}
	reason := "成员下架套餐"
	if *active {
		reason = "成员上架套餐"
	}
	plan, err := s.marketPlanStorage().SetSellerPlanListing(r.Context(), store.SetSellerPlanListingParams{
		BillingWriteParams: s.billingWriteParams(r, operationID, reason), PlanID: planID, Version: *version, Active: *active,
	})
	if err != nil {
		s.marketplacePlanError(w, r, "set seller plan listing", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) mySellerPlanUsage(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	if !validBillingResourceID(planID) || r.URL.RawQuery != "" {
		s.billingInputError(w, r)
		return
	}
	usage, err := s.marketPlanStorage().SellerPlanUsage(r.Context(), userFrom(r.Context()).ID, planID)
	if err != nil {
		s.marketplacePlanError(w, r, "read seller plan usage", err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// salesPage reads limit (1–200, default 50) and offset (default 0).
func salesPage(r *http.Request) (int, int, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, 0, false
	}
	limit, offset := 50, 0
	for key, values := range query {
		if len(values) != 1 {
			return 0, 0, false
		}
		value, err := strconv.Atoi(values[0])
		if err != nil {
			return 0, 0, false
		}
		switch key {
		case "limit":
			limit = value
		case "offset":
			offset = value
		default:
			return 0, 0, false
		}
	}
	return limit, offset, limit >= 1 && limit <= 200 && offset >= 0 && offset <= 1_000_000
}

func (s *Server) mySales(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := salesPage(r)
	if !ok {
		s.billingInputError(w, r)
		return
	}
	sales, err := s.marketPlanStorage().SellerSales(r.Context(), userFrom(r.Context()).ID, limit, offset)
	if err != nil {
		internalError(s, w, r, "list seller sales", err)
		return
	}
	writeJSON(w, http.StatusOK, sales)
}

func (s *Server) marketReviews(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.billingInputError(w, r)
		return
	}
	pending, err := s.marketPlanStorage().PendingSellerPlans(r.Context())
	if err != nil {
		internalError(s, w, r, "list pending seller plans", err)
		return
	}
	plans, err := s.marketPlanStorage().AllSellerPlans(r.Context())
	if err != nil {
		internalError(s, w, r, "list seller plans", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": pending, "plans": plans})
}

func (s *Server) reviewSellerPlan(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	if !validBillingResourceID(planID) || !strictJSONRequest(r) {
		s.billingInputError(w, r)
		return
	}
	var decision, note string
	var version *int64
	if err := decodeExactJSONFields(r, 4<<10, map[string]any{"decision": &decision, "note": &note, "version": &version}); err != nil {
		badJSON(w, r, err)
		return
	}
	if (decision != "approve" && decision != "reject") || version == nil || *version < 1 {
		s.billingInputError(w, r)
		return
	}
	plan, err := s.marketPlanStorage().ReviewSellerPlan(r.Context(), store.ReviewSellerPlanParams{
		PlanReviewWriteParams: s.planReviewWrite(r, planID, *version), Approve: decision == "approve", Note: note,
	})
	if err != nil {
		s.marketplacePlanError(w, r, "review seller plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) suspendSellerPlan(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	if !validBillingResourceID(planID) || !strictJSONRequest(r) {
		s.billingInputError(w, r)
		return
	}
	var suspended *bool
	var reason string
	if err := decodeExactJSONFields(r, 4<<10, map[string]any{"suspended": &suspended, "reason": &reason}); err != nil {
		badJSON(w, r, err)
		return
	}
	if suspended == nil {
		s.billingInputError(w, r)
		return
	}
	plan, err := s.marketPlanStorage().SetSellerPlanSuspended(r.Context(), store.SetSellerPlanSuspendedParams{
		PlanReviewWriteParams: s.planReviewWrite(r, planID, 0), Suspended: *suspended, Reason: reason,
	})
	if err != nil {
		s.marketplacePlanError(w, r, "suspend seller plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) allMarketSales(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := salesPage(r)
	if !ok {
		s.billingInputError(w, r)
		return
	}
	sales, err := s.marketPlanStorage().AllMarketplaceSales(r.Context(), limit, offset)
	if err != nil {
		internalError(s, w, r, "list marketplace sales", err)
		return
	}
	writeJSON(w, http.StatusOK, sales)
}
