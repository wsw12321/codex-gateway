package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

// Aggregation gateway stage 3: plan provider scopes and self-use. Every route
// here exists only while the marketplace is enabled; with it off a request
// pays exactly as before the stage.

const planScopeRequestBytes = 64 << 10

type planScopeRepository interface {
	PlanProviderScopes(context.Context, string) (map[string]store.PlanProviderScope, error)
	PlanScopeCandidates(context.Context, string) ([]store.PlanProviderScopeAccount, error)
	SetPlanProviderScope(context.Context, store.SetPlanProviderScopeParams) (store.PlanProviderScope, error)
	SubscriptionProviderScopes(context.Context, string) (map[string]store.SubscriptionProviderScope, error)
	SelfUseEnabled(context.Context, string) (bool, error)
	SetSelfUse(context.Context, store.SetSelfUseParams) (bool, error)
	SetThirdPartyConsent(context.Context, store.SetThirdPartyConsentParams) (bool, error)
}

func (s *Server) planScopeStorage() planScopeRepository {
	if s.planScopeRepo != nil {
		return s.planScopeRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

// marketplaceOn reads the switch for a tenant feature; a read failure counts
// as off (fail closed).
func (s *Server) marketplaceOn(ctx context.Context) bool {
	repository := s.marketplaceStorage()
	if repository == nil {
		return false
	}
	settings, err := repository.MarketplaceSettings(ctx)
	return err == nil && settings.Enabled
}

// requireMarketplaceOn answers 404 while the marketplace is off.
func (s *Server) requireMarketplaceOn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.planScopeStorage() == nil || !s.marketplaceOn(r.Context()) {
			httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "marketplace_disabled", "商城功能未开启")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) planScopeRoutes() {
	s.mux.Handle("GET /admin/billing/plan-provider-scopes", s.requireSession(s.ownerOnly(s.requireMarketplaceOn(http.HandlerFunc(s.platformPlanScopes)))))
	s.mux.Handle("PUT /admin/billing/plans/{plan_id}/provider-scope", s.browserOrigin(s.requireRecentVerification(s.ownerOnly(s.requireMarketplaceOn(http.HandlerFunc(s.setPlatformPlanScope))))))
	s.mux.Handle("PUT /admin/billing/me/self-use", s.browserOrigin(s.requireRecentVerification(s.requireMarketplaceOn(http.HandlerFunc(s.setMySelfUse)))))
	s.mux.Handle("PUT /admin/billing/me/subscriptions/{id}/third-party-consent", s.browserOrigin(s.requireRecentVerification(s.requireMarketplaceOn(http.HandlerFunc(s.setMyThirdPartyConsent)))))
}

func (s *Server) platformPlanScopes(w http.ResponseWriter, r *http.Request) {
	scopes, err := s.planScopeStorage().PlanProviderScopes(r.Context(), "")
	if err != nil {
		internalError(s, w, r, "list plan provider scopes", err)
		return
	}
	candidates, err := s.planScopeStorage().PlanScopeCandidates(r.Context(), "")
	if err != nil {
		internalError(s, w, r, "list plan scope candidates", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes, "candidates": candidates})
}

func (s *Server) setPlatformPlanScope(w http.ResponseWriter, r *http.Request) {
	s.setPlanScope(w, r, "")
}

// setPlanScope writes a plan's provider scope for its seller: "" is the
// Owner writing a platform plan, otherwise the member writing their own plan.
// Another seller's plan reads as not found.
func (s *Server) setPlanScope(w http.ResponseWriter, r *http.Request, seller string) {
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_plan_scope", "供应商范围请求必须为 JSON，且不能包含查询参数")
		return
	}
	var mode string
	var accounts []string
	var revision int64
	if err := decodeExactJSONFields(r, planScopeRequestBytes, map[string]any{"mode": &mode, "account_ids": &accounts, "revision": &revision}); err != nil {
		badJSON(w, r, err)
		return
	}
	if accounts == nil {
		accounts = []string{}
	}
	scope, err := s.planScopeStorage().SetPlanProviderScope(r.Context(), store.SetPlanProviderScopeParams{
		PlanID: r.PathValue("plan_id"), SellerUserID: seller, Mode: mode, AccountIDs: accounts, ExpectedRevision: revision,
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()), At: time.Now().UTC(),
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, scope)
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "billing_plan_not_found", "套餐不存在")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "plan_scope_changed", "供应商范围已被修改，请刷新后重试")
	case errors.Is(err, store.ErrInvalid):
		message := "供应商范围无效：指定时须选择 1 至 256 个（每个协议）仍可用的平台账号"
		if seller != "" {
			message = "供应商范围无效：指定时须选择 1 至 256 个（每个协议）仍可用的自有账号"
		}
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_plan_scope", message)
	default:
		internalError(s, w, r, "set plan provider scope", err)
	}
}

func (s *Server) setMySelfUse(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user.Role != store.UserRoleMember {
		httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "marketplace_member_only", "Owner 的账号即平台账号，没有自用设置")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_self_use", "自用设置请求必须为 JSON，且不能包含查询参数")
		return
	}
	var enabled *bool
	if err := decodeSingleJSONField(r, 256, "enabled", &enabled); err != nil || enabled == nil {
		if err == nil {
			err = errors.New("enabled is required")
		}
		badJSON(w, r, err)
		return
	}
	value, err := s.planScopeStorage().SetSelfUse(r.Context(), store.SetSelfUseParams{
		UserID: user.ID, Enabled: *enabled, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()), At: time.Now().UTC(),
	})
	if err != nil {
		s.billingStoreError(w, r, "set self-use", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"self_use_enabled": value})
}

// setMyThirdPartyConsent gives or withdraws the buyer's consent that the
// seller's API key suppliers serve one subscription of a seller plan. They
// see the requests they serve; without consent they do not serve it, even if
// the seller adds them to the plan after the purchase.
func (s *Server) setMyThirdPartyConsent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validBillingResourceID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "not_found_error", "subscription_not_found", "订阅不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_third_party_consent", "确认请求必须为 JSON，且不能包含查询参数")
		return
	}
	var acknowledged *bool
	if err := decodeSingleJSONField(r, 256, "acknowledged", &acknowledged); err != nil || acknowledged == nil {
		if err == nil {
			err = errors.New("acknowledged is required")
		}
		badJSON(w, r, err)
		return
	}
	user := userFrom(r.Context())
	value, err := s.planScopeStorage().SetThirdPartyConsent(r.Context(), store.SetThirdPartyConsentParams{
		UserID: user.ID, SubscriptionID: id, Acknowledged: *acknowledged, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()), At: time.Now().UTC(),
	})
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, "not_found_error", "subscription_not_found", "订阅不存在或不是成员套餐的订阅")
		return
	}
	if err != nil {
		s.billingStoreError(w, r, "set third-party consent", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_id": id, "third_party_acknowledged": value})
}

// addMarketplaceBillingState adds the stage 3 view to a billing state while
// the marketplace is on: the self-use switch and, for each subscription
// bought from a plan, which accounts it pays for. A failed read leaves both
// out rather than failing the page.
func (s *Server) addMarketplaceBillingState(ctx context.Context, userID string, response map[string]any) {
	repository := s.planScopeStorage()
	if repository == nil || !s.marketplaceOn(ctx) {
		return
	}
	if enabled, err := repository.SelfUseEnabled(ctx, userID); err == nil {
		response["self_use_enabled"] = enabled
	}
	scopes, err := repository.SubscriptionProviderScopes(ctx, userID)
	if err != nil {
		return
	}
	subscriptions, _ := response["subscriptions"].([]map[string]any)
	for _, subscription := range subscriptions {
		id, _ := subscription["id"].(string)
		if scope, ok := scopes[id]; ok {
			subscription["provider_scope"] = scope
		}
	}
}
