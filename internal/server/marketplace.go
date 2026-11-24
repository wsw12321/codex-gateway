package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const marketplaceSettingsRequestBytes = 1 << 10

type marketplaceRepository interface {
	MarketplaceSettings(context.Context) (store.MarketplaceSettings, error)
	SetMarketplaceSettings(context.Context, store.SetMarketplaceSettingsParams) (store.MarketplaceSettings, error)
}

func (s *Server) marketplaceStorage() marketplaceRepository {
	if s.marketplaceRepo != nil {
		return s.marketplaceRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

func (s *Server) marketplaceSettings(w http.ResponseWriter, r *http.Request) {
	repository := s.marketplaceStorage()
	if repository == nil {
		internalError(s, w, r, "read marketplace settings", errors.New("marketplace storage unavailable"))
		return
	}
	settings, err := repository.MarketplaceSettings(r.Context())
	if err != nil {
		internalError(s, w, r, "read marketplace settings", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) setMarketplaceSettings(w http.ResponseWriter, r *http.Request) {
	repository := s.marketplaceStorage()
	if repository == nil {
		internalError(s, w, r, "set marketplace settings", errors.New("marketplace storage unavailable"))
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_marketplace_request", "商城设置请求必须为 JSON，且不能包含查询参数")
		return
	}
	var enabled, allowSuppliers *bool
	var rate *string
	var maxAccounts, maxPlans *int
	var revision *int64
	if err := decodeExactJSONFields(r, marketplaceSettingsRequestBytes, map[string]any{
		"enabled": &enabled, "commission_rate": &rate, "max_accounts_per_user": &maxAccounts,
		"max_plans_per_user": &maxPlans, "allow_member_suppliers": &allowSuppliers, "revision": &revision,
	}); err != nil {
		badJSON(w, r, err)
		return
	}
	if enabled == nil || allowSuppliers == nil || rate == nil || maxAccounts == nil || maxPlans == nil ||
		revision == nil || *revision < 1 {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_marketplace_settings", "请提供完整的商城设置和当前设置版本")
		return
	}
	if _, err := store.NormalizeCommissionRate(*rate); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_commission_rate", "抽成比例必须是 0 到 1 之间、最多 6 位小数的数字")
		return
	}
	if *maxAccounts < 0 || *maxAccounts > store.MaxMarketplaceAccountsPerUser ||
		*maxPlans < 0 || *maxPlans > store.MaxMarketplacePlansPerUser {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_marketplace_limits", "每人供应商上限须为 0–256，每人套餐上限须为 0–100")
		return
	}
	settings, err := repository.SetMarketplaceSettings(r.Context(), store.SetMarketplaceSettingsParams{
		Enabled: *enabled, CommissionRate: *rate, MaxAccountsPerUser: *maxAccounts, MaxPlansPerUser: *maxPlans,
		AllowMemberSuppliers: *allowSuppliers, ExpectedRevision: *revision, At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "marketplace_revision_changed", "商城设置已被修改，请刷新后重试")
		return
	}
	if err != nil {
		s.storeWriteError(w, r, "set marketplace settings", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
