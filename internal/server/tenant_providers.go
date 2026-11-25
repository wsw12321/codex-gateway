package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// Tenant providers (aggregation gateway stage 2). Members log in or import
// their own upstream accounts, add API key suppliers and run their own
// egress nodes. The routes reuse the Owner's handlers with the request scoped
// to the member: every store query then applies the member's ownership, and
// the handlers that act on an account directly in CPA check it first.

type accountScope struct {
	// provider overrides the URL-based provider of the Owner's routes.
	provider string
	// tenant is the member the request acts for; empty for the platform.
	tenant string
}

type accountScopeContextKey struct{}
type marketplaceContextKey struct{}

func withAccountScope(r *http.Request, scope accountScope) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), accountScopeContextKey{}, scope))
}

func requestAccountScope(r *http.Request) (accountScope, bool) {
	scope, ok := r.Context().Value(accountScopeContextKey{}).(accountScope)
	return scope, ok
}

// requestTenant is the member a /admin/me/ route acts for; empty elsewhere.
func requestTenant(r *http.Request) string {
	scope, _ := requestAccountScope(r)
	return scope.tenant
}

func marketplaceFrom(ctx context.Context) store.MarketplaceSettings {
	settings, _ := ctx.Value(marketplaceContextKey{}).(store.MarketplaceSettings)
	return settings
}

const (
	credentialOwnershipProtocol = "credential_ownership_v1"
	// maxTenantOAuthAttempts bounds one member's unfinished logins.
	maxTenantOAuthAttempts = 2
)

// tenantRefusals maps the plugin's and the store's refusal codes to what the
// member sees. Each is a 409.
var tenantRefusals = map[string]string{
	"account_owned_elsewhere": "该上游账号已归平台或其他用户所有，不能重复添加",
	"provider_limit_reached":  "已达到可添加的数量上限",
	"account_in_platform_use": "该上游账号仍被平台配置使用，暂不能添加",
}

func writeTenantRefusal(w http.ResponseWriter, r *http.Request, code string) {
	httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", code, tenantRefusals[code])
}

// requireMarketplaceMember admits a member (never the Owner, whose accounts
// are the platform's) while the marketplace is enabled. It must run after
// requireSession. A settings read failure fails closed.
func (s *Server) requireMarketplaceMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r.Context())
		if user.Role != store.UserRoleMember {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "marketplace_member_only", "Owner 的账号即平台账号，请在上游账号页面管理")
			return
		}
		repository := s.marketplaceStorage()
		if repository == nil || s.store == nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "marketplace_unavailable", "商城设置暂不可用，请稍后重试")
			return
		}
		settings, err := repository.MarketplaceSettings(r.Context())
		if err != nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "marketplace_unavailable", "商城设置暂不可用，请稍后重试")
			return
		}
		if !settings.Enabled {
			httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "marketplace_disabled", "商城功能未开启")
			return
		}
		if s.config.EgressControlKey == "" {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "egress_not_configured", "账号出口服务尚未配置")
			return
		}
		ctx := context.WithValue(r.Context(), marketplaceContextKey{}, settings)
		next.ServeHTTP(w, withAccountScope(r.WithContext(ctx), accountScope{tenant: user.ID}))
	})
}

// tenantProvider scopes a member route to its {provider}.
func (s *Server) tenantProvider(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := r.PathValue("provider")
		if !cpaProvider(provider) || provider == store.UpstreamProviderAntigravity && s.antigravity == nil ||
			provider == store.UpstreamProviderAnthropic && s.anthropic == nil {
			httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "provider_not_found", "不支持该协议")
			return
		}
		scope, _ := requestAccountScope(r)
		scope.provider = provider
		next.ServeHTTP(w, withAccountScope(r, scope))
	})
}

// requireMemberSuppliers admits API key supplier writes only while the
// deployment runs suppliers and the Owner lets members add them.
func (s *Server) requireMemberSuppliers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.config.SuppliersEnabled || !marketplaceFrom(r.Context()).AllowMemberSuppliers {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "member_suppliers_disabled", "管理员未开放成员添加 API Key 供应商")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitTenant bounds how often one member calls an expensive route.
func (s *Server) limitTenant(name string, limit int, window time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, retry := s.attempts.allow("tenant:"+name+":"+userFrom(r.Context()).ID, limit, window); !ok {
			w.Header().Set("Retry-After", integerString(retry))
			httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", "tenant_rate_limited", "操作过于频繁，请稍后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tenantProviderRoutes() {
	read := func(h http.Handler) http.Handler {
		return s.requireSession(s.requireMarketplaceMember(httpx.NoStore(h)))
	}
	write := func(h http.Handler) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(s.requireMarketplaceMember(s.operationAudit("tenant_provider.operation", h))))
	}
	provider := func(h http.HandlerFunc) http.Handler { return s.tenantProvider(h) }

	s.mux.Handle("GET /admin/me/providers", read(s.limitTenant("providers", 30, time.Minute, http.HandlerFunc(s.myProviders))))
	s.mux.Handle("GET /admin/me/providers/{provider}/concurrency", read(provider(s.upstreamAccountConcurrency)))
	s.mux.Handle("POST /admin/me/providers/{provider}/oauth", write(s.limitTenant("oauth", 10, time.Minute, provider(s.cpaOAuthBegin))))
	s.mux.Handle("GET /admin/me/providers/oauth/{id}/status", read(http.HandlerFunc(s.cpaOAuthStatus)))
	s.mux.Handle("POST /admin/me/providers/oauth/{id}/callback", write(http.HandlerFunc(s.cpaOAuthCallback)))
	s.mux.Handle("POST /admin/me/providers/{provider}/credentials", write(s.limitTenant("import", 10, time.Minute, provider(s.cpaImport))))
	suppliers := func(h http.HandlerFunc) http.Handler {
		return s.tenantProvider(s.requireMemberSuppliers(s.limitTenant("suppliers", 10, time.Minute, h)))
	}
	s.mux.Handle("POST /admin/me/providers/{provider}/suppliers", write(suppliers(s.saveModelSupplier)))
	s.mux.Handle("PUT /admin/me/providers/{provider}/suppliers/{id}", write(suppliers(s.saveModelSupplier)))
	s.mux.Handle("PUT /admin/me/providers/{provider}/accounts/{id}/status", write(provider(s.setUpstreamAccountStatus)))
	s.mux.Handle("PUT /admin/me/providers/{provider}/accounts/{id}/alias", write(provider(s.setUpstreamAccountAlias)))
	s.mux.Handle("PUT /admin/me/providers/{provider}/accounts/{id}/concurrent-limit", write(provider(s.setUpstreamAccountConcurrentLimit)))
	s.mux.Handle("PUT /admin/me/providers/{provider}/accounts/{id}/allocation-weight", write(provider(s.setUpstreamAccountAllocationWeight)))
	s.mux.Handle("PUT /admin/me/providers/{provider}/accounts/{id}/egress", write(provider(s.setAccountEgress)))
	s.mux.Handle("POST /admin/me/providers/{provider}/accounts/{id}/refresh", write(s.limitTenant("refresh", 20, time.Minute, provider(s.cpaRefresh))))
	s.mux.Handle("POST /admin/me/providers/{provider}/accounts/{id}/quota", write(s.limitTenant("quota", 20, time.Minute, provider(s.cpaQuota))))
	s.mux.Handle("DELETE /admin/me/providers/{provider}/accounts/{id}", write(provider(s.cpaDelete)))

	s.mux.Handle("GET /admin/me/egress-servers", read(http.HandlerFunc(s.egressServersJSON)))
	s.mux.Handle("POST /admin/me/egress-servers", write(http.HandlerFunc(s.saveEgressServer)))
	s.mux.Handle("PUT /admin/me/egress-servers/{id}", write(http.HandlerFunc(s.saveEgressServer)))
	s.mux.Handle("DELETE /admin/me/egress-servers/{id}", write(http.HandlerFunc(s.deleteEgressServer)))
	// A connectivity test changes nothing: a session and the Origin check.
	s.mux.Handle("POST /admin/me/egress-servers/{id}/test", s.browserOrigin(s.requireSession(s.requireMarketplaceMember(
		s.limitTenant("egress-test", 10, time.Minute, http.HandlerFunc(s.testEgressServer))))))

	// Unknown member paths answer 404 JSON instead of the dashboard page.
	for _, prefix := range []string{"/admin/me/providers/", "/admin/me/egress-servers/"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			s.mux.Handle(method+" "+prefix, s.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "not_found", "接口不存在")
			})))
		}
	}

	ownerRead := func(h http.HandlerFunc) http.Handler { return s.requireSession(s.ownerOnly(httpx.NoStore(h))) }
	ownerWrite := func(h http.HandlerFunc) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(s.ownerOnly(h)))
	}
	s.mux.Handle("GET /admin/tenant-providers", ownerRead(s.tenantProvidersJSON))
	s.mux.Handle("PUT /admin/tenant-providers/{id}/suspension", ownerWrite(s.setTenantProviderSuspension))
	s.mux.Handle("DELETE /admin/tenant-providers/egress-servers/{id}", ownerWrite(s.deleteTenantEgressServer))
	for _, path := range []string{"upstream-accounts", "antigravity-accounts", "anthropic-accounts"} {
		s.mux.Handle("PUT /admin/"+path+"/{id}/owner", ownerWrite(s.transferUpstreamAccountOwner))
	}
}

// accountInScope answers 404 unless the account belongs to this request's
// owner: the member's own account on /admin/me/ routes, a platform account
// (or one the Gateway has no row for) on the Owner's routes. Handlers that
// act on an account directly in CPA call it first; store queries apply the
// same scope themselves.
func (s *Server) accountInScope(w http.ResponseWriter, r *http.Request, id string) (store.UpstreamAccountOwnership, bool) {
	if s.store == nil {
		return store.UpstreamAccountOwnership{}, true
	}
	ownership, err := s.store.UpstreamAccountOwnership(r.Context(), id)
	if err != nil {
		internalError(s, w, r, "read upstream account owner", err)
		return ownership, false
	}
	if !ownership.InScope(requestTenant(r)) || ownership.Exists && ownership.Provider != attemptProvider(r) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return ownership, false
	}
	return ownership, true
}

// requireTenantCredentialOwnership confirms, before a member's login or
// import starts, that CPA runs a plugin which refuses a conflicting login
// without touching the account it reached. An older plugin would disable the
// owner's account, so members cannot log in through it.
func (s *Server) requireTenantCredentialOwnership(w http.ResponseWriter, r *http.Request, provider string) bool {
	if requestTenant(r) == "" {
		return true
	}
	var probe struct {
		Protocol string `json:"protocol"`
	}
	if err := s.cpaCall(r.Context(), http.MethodGet, provider+"/ownership", nil, &probe); err != nil || probe.Protocol != credentialOwnershipProtocol {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "tenant_providers_unavailable", "账号服务版本过旧，暂不能添加供应商")
		return false
	}
	return true
}

// tenantAccountSlotAvailable is the early answer to a member at the account
// limit; the egress registration enforces the limit again when it commits.
func (s *Server) tenantAccountSlotAvailable(w http.ResponseWriter, r *http.Request) bool {
	tenant := requestTenant(r)
	if tenant == "" {
		return true
	}
	count, err := s.store.CountTenantAccounts(r.Context(), tenant)
	if err != nil {
		internalError(s, w, r, "count tenant accounts", err)
		return false
	}
	if count >= marketplaceFrom(r.Context()).MaxAccountsPerUser {
		writeTenantRefusal(w, r, "provider_limit_reached")
		return false
	}
	return true
}

type tenantAccountDTO struct {
	ID                  string                 `json:"id"`
	Provider            string                 `json:"provider"`
	CredentialKind      string                 `json:"credential_kind"`
	DisplayName         string                 `json:"display_name,omitempty"`
	Alias               string                 `json:"alias"`
	EmailMasked         string                 `json:"email_masked"`
	Plan                string                 `json:"plan"`
	Status              string                 `json:"status"`
	CliproxyStatus      string                 `json:"cliproxy_status"`
	GatewayManualStatus string                 `json:"gateway_manual_status"`
	GatewayQuotaStatus  string                 `json:"gateway_quota_status"`
	AdminSuspended      bool                   `json:"admin_suspended"`
	CanManage           bool                   `json:"can_manage"`
	ConcurrentLimit     int                    `json:"concurrent_limit"`
	AllocationWeight    int                    `json:"allocation_weight"`
	EgressServerID      string                 `json:"egress_server_id"`
	EgressServerName    string                 `json:"egress_server_name"`
	SupplierModels      []string               `json:"supplier_models,omitempty"`
	QuotaGuard          *upstreamQuotaGuardDTO `json:"quota_guard"`
	LastSyncedAt        *time.Time             `json:"last_synced_at"`
	upstreamAccountUsageDTO
}

type myProvidersResponse struct {
	MaxAccounts      int                `json:"max_accounts_per_user"`
	MaxEgressServers int                `json:"max_egress_servers_per_user"`
	MemberSuppliers  bool               `json:"member_suppliers"`
	AccountCount     int                `json:"account_count"`
	Providers        []string           `json:"providers"`
	From             *time.Time         `json:"from"`
	Until            time.Time          `json:"until"`
	All              bool               `json:"all"`
	SyncWarning      string             `json:"sync_warning,omitempty"`
	Accounts         []tenantAccountDTO `json:"accounts"`
}

// myProviders lists the member's own accounts of every provider, with the
// same live status and usage the Owner sees for platform accounts. It never
// shows platform concepts (allocation shares, exclusive grants).
func (s *Server) myProviders(w http.ResponseWriter, r *http.Request) {
	query, err := parseUpstreamAccountQuery(time.Now(), r.URL.Query())
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_account_filter", "统计区间无效")
		return
	}
	settings := marketplaceFrom(r.Context())
	tenant := requestTenant(r)
	response := myProvidersResponse{
		MaxAccounts: settings.MaxAccountsPerUser, MaxEgressServers: settings.MaxEgressServersPerUser,
		MemberSuppliers: s.config.SuppliersEnabled && settings.AllowMemberSuppliers,
		Until:           query.Until, All: query.All, Accounts: []tenantAccountDTO{},
	}
	if !query.All {
		from := query.From
		response.From = &from
	}
	egressState, err := s.store.EgressSnapshot(r.Context())
	if err != nil {
		internalError(s, w, r, "read account egress bindings", err)
		return
	}
	bindings := map[string]string{}
	for _, binding := range egressState.Bindings {
		bindings[binding.AccountID] = binding.NodeID
	}
	nodeNames := map[string]string{}
	for _, node := range egressState.Nodes {
		if node.OwnerUserID == tenant {
			nodeNames[node.ID] = node.Name
		}
	}
	_, guardStates, err := s.store.ListUpstreamQuotaGuards(r.Context(), time.Now().UTC())
	if err != nil {
		internalError(s, w, r, "list upstream quota guards", err)
		return
	}
	guards := make(map[string]store.UpstreamQuotaGuardState, len(guardStates))
	for _, state := range guardStates {
		guards[state.AccountID] = state
	}
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAnthropic, store.UpstreamProviderAntigravity} {
		client := s.upstream
		switch provider {
		case store.UpstreamProviderAnthropic:
			client = s.anthropic
		case store.UpstreamProviderAntigravity:
			client = s.antigravity
		}
		if client == nil {
			continue
		}
		response.Providers = append(response.Providers, provider)
		scoped := withAccountScope(r, accountScope{provider: provider, tenant: tenant})
		remote, warning := s.syncProviderAccounts(r.Context(), scoped, client)
		if warning != "" {
			response.SyncWarning = warning
		}
		repository := s.accountStore(scoped)
		accounts, err := repository.ListUpstreamAccounts(r.Context())
		if err != nil {
			internalError(s, w, r, "list tenant accounts", err)
			return
		}
		byID := make(map[string]store.UpstreamAccount, len(accounts))
		for _, account := range accounts {
			byID[account.ID] = account
		}
		rows, err := repository.SummarizeUpstreamAccounts(r.Context(), store.UpstreamAccountSummaryFilter{
			From: query.From, Until: query.Until, All: query.All, LiveFrom: query.LiveFrom,
		})
		if err != nil {
			internalError(s, w, r, "summarize tenant accounts", err)
			return
		}
		suppliers := map[string]store.ModelSupplier{}
		if s.config.SuppliersEnabled {
			values, err := repository.ModelSuppliers(r.Context())
			if err != nil {
				internalError(s, w, r, "list tenant suppliers", err)
				return
			}
			for _, value := range values {
				suppliers[value.ID] = value
			}
		}
		for _, row := range rows {
			if row.AccountID == nil {
				continue
			}
			account := byID[*row.AccountID]
			dto := tenantAccountDTO{
				ID: *row.AccountID, Provider: provider, CredentialKind: row.CredentialKind, DisplayName: row.DisplayName,
				Alias: row.Alias, EmailMasked: row.MaskedEmail, Plan: row.Plan, Status: "unknown",
				CliproxyStatus: "unknown", GatewayManualStatus: "unknown", GatewayQuotaStatus: "unknown",
				AdminSuspended: account.AdminSuspended, ConcurrentLimit: row.ConcurrentLimit, AllocationWeight: account.AllocationWeight,
				EgressServerID: bindings[*row.AccountID], LastSyncedAt: row.LastSyncedAt, upstreamAccountUsageDTO: upstreamUsageDTO(row),
			}
			dto.EgressServerName = nodeNames[dto.EgressServerID]
			if supplier, ok := suppliers[dto.ID]; ok && row.CredentialKind == "supplier" {
				dto.SupplierModels, dto.CanManage, dto.Status = supplier.Models, true, row.Status
				dto.GatewayManualStatus, dto.CliproxyStatus, dto.GatewayQuotaStatus = supplierManualStatus(supplier.Enabled), "disabled", "available"
				if supplier.Enabled {
					dto.CliproxyStatus = "active"
				}
			} else if live, ok := remote[dto.ID]; ok && upstreamAccountSourceStatusKnown(live) {
				dto.CanManage, dto.Status = true, row.Status
				dto.CliproxyStatus, dto.GatewayManualStatus, dto.GatewayQuotaStatus = live.CliproxyStatus, live.GatewayManualStatus, live.GatewayQuotaStatus
			}
			if row.CredentialKind != "supplier" && provider != store.UpstreamProviderAntigravity {
				state, present := guards[dto.ID]
				dto.QuotaGuard = upstreamQuotaGuardView(state, present)
			}
			response.Accounts = append(response.Accounts, dto)
		}
	}
	response.AccountCount, err = s.store.CountTenantAccounts(r.Context(), tenant)
	if err != nil {
		internalError(s, w, r, "count tenant accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// syncProviderAccounts applies CPA's account list for one provider, under the
// same lock as the Owner's list, and returns its live status by account.
func (s *Server) syncProviderAccounts(ctx context.Context, r *http.Request, client *gatewayproxy.Client) (map[string]gatewayproxy.UpstreamAccount, string) {
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	syncCtx, cancel := context.WithTimeout(ctx, upstreamAccountSyncTimeout)
	accounts, err := client.ListUpstreamAccounts(syncCtx)
	cancel()
	if err != nil {
		return nil, "upstream_account_sync_unavailable"
	}
	if err := s.accountStore(r).SyncUpstreamAccounts(ctx, upstreamAccountSnapshots(accounts), time.Now().UTC()); err != nil {
		return nil, "upstream_account_sync_unavailable"
	}
	live := make(map[string]gatewayproxy.UpstreamAccount, len(accounts))
	for _, account := range accounts {
		live[account.ID] = account
	}
	return live, ""
}

// tenantProvidersJSON is the Owner's view of every tenant account and node.
func (s *Server) tenantProvidersJSON(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		internalError(s, w, r, "list tenant providers", errors.New("store unavailable"))
		return
	}
	accounts, err := s.store.ListTenantUpstreamAccounts(r.Context())
	if err != nil {
		internalError(s, w, r, "list tenant providers", err)
		return
	}
	nodes, err := s.store.ListTenantEgressServers(r.Context())
	if err != nil {
		internalError(s, w, r, "list tenant egress nodes", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts, "egress_servers": nodes})
}

func (s *Server) setTenantProviderSuspension(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_suspension_request", "请求必须为 JSON，且不能包含查询参数")
		return
	}
	var suspended *bool
	if err := decodeSingleJSONField(r, 256, "suspended", &suspended); err != nil || suspended == nil {
		badJSON(w, r, errors.New("suspended must be a boolean"))
		return
	}
	account, err := s.store.SetUpstreamAccountSuspended(r.Context(), store.SetUpstreamAccountSuspendedParams{
		AccountID: id, Suspended: *suspended, Actor: egressActor(r),
	})
	if err != nil {
		s.storeWriteError(w, r, "suspend tenant account", err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// deleteTenantEgressServer removes a member's egress node for the Owner, for
// example before permanently deleting a disabled member. A bound node is
// refused: the Owner first transfers or deletes the accounts that use it.
func (s *Server) deleteTenantEgressServer(w http.ResponseWriter, r *http.Request) {
	if !s.egressEnabled(w, r) {
		return
	}
	var input struct{}
	if !cpaJSON(w, r, &input, 128) {
		return
	}
	err := s.store.DeleteTenantEgressServer(r.Context(), r.PathValue("id"), egressActor(r))
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "tenant_egress_in_use",
			"节点仍被成员账号绑定，或有进行中的登录选择了它；请先转移或删除这些账号")
		return
	}
	if err != nil {
		egressError(w, r, err)
		return
	}
	_, status, _ := s.syncAccountEgress(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "runtime": publicEgressStatus(status)})
}

// transferUpstreamAccountOwner is the only way an account with a credential
// changes hands. A node of the previous owner is dropped and the account
// returns to the global egress.
func (s *Server) transferUpstreamAccountOwner(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) || s.store == nil {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_owner_request", "请求必须为 JSON，且不能包含查询参数")
		return
	}
	var owner, expected *string
	if err := decodeExactJSONFields(r, 1<<10, map[string]any{"owner_user_id": &owner, "expected_owner_user_id": &expected}); err != nil {
		badJSON(w, r, err)
		return
	}
	value := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	binding, err := s.accountStore(r).TransferUpstreamAccountOwner(r.Context(), store.TransferUpstreamAccountParams{
		AccountID: id, NewOwnerUserID: value(owner), ExpectedOwnerUserID: value(expected), Actor: egressActor(r),
	})
	switch {
	case errors.Is(err, store.ErrAccountInPlatformUse):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "account_in_platform_use", "请先把账号移出群组账号池、专属授权和用户账号权限名单")
		return
	case errors.Is(err, store.ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "upstream_account_owner_changed", "账号归属已变化或凭据已删除，请刷新后重试")
		return
	case errors.Is(err, store.ErrInvalid):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_account_owner", "新主人必须是正常状态的成员")
		return
	case err != nil:
		s.storeWriteError(w, r, "transfer upstream account", err)
		return
	}
	if binding != nil {
		_, _, _ = s.syncAccountEgress(r.Context())
		if ownership, err := s.store.UpstreamAccountOwnership(r.Context(), id); err == nil && ownership.CredentialKind == "oauth" {
			s.pushPluginEgress(r.Context(), []store.EgressBinding{*binding})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "owner_user_id": value(owner), "egress_reset": binding != nil})
}

// marketplaceView is what every signed-in page needs to show or hide the
// member features; a read failure hides them.
type adminMarketplaceState struct {
	Enabled          bool `json:"enabled"`
	MemberSuppliers  bool `json:"member_suppliers"`
	MaxAccounts      int  `json:"max_accounts_per_user"`
	MaxEgressServers int  `json:"max_egress_servers_per_user"`
	MaxPlans         int  `json:"max_plans_per_user"`
	// PendingReviews is the Owner's count of seller plans awaiting review.
	PendingReviews int `json:"pending_reviews,omitempty"`
}

func (s *Server) marketplaceState(ctx context.Context) adminMarketplaceState {
	repository := s.marketplaceStorage()
	if repository == nil {
		return adminMarketplaceState{}
	}
	settings, err := repository.MarketplaceSettings(ctx)
	if err != nil || !settings.Enabled {
		return adminMarketplaceState{}
	}
	return adminMarketplaceState{
		Enabled: true, MemberSuppliers: s.config.SuppliersEnabled && settings.AllowMemberSuppliers,
		MaxAccounts: settings.MaxAccountsPerUser, MaxEgressServers: settings.MaxEgressServersPerUser,
		MaxPlans: settings.MaxPlansPerUser,
	}
}

func tenantRefusalCode(err error) string {
	for code, sentinel := range map[string]error{
		"account_owned_elsewhere": store.ErrAccountOwnedElsewhere,
		"provider_limit_reached":  store.ErrProviderLimitReached,
		"account_in_platform_use": store.ErrAccountInPlatformUse,
	} {
		if errors.Is(err, sentinel) {
			return code
		}
	}
	return ""
}

func knownTenantRefusal(code string) bool {
	_, ok := tenantRefusals[code]
	return ok
}
