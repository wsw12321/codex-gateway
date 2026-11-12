package gatewayplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The host serves plugin routes below its management base path and matches
// them by exact method and path, so an account id travels as the id query
// parameter instead of a path segment. Every route sits behind the host's
// management key, which only the Gateway holds.
const (
	managementBase   = "/v0/management"
	managementPrefix = "/gateway/"

	protocolAccountAccess = "upstream_account_access_v1"
	protocolGroupScope    = "group_upstream_account_scope_v1"
	protocolFundingLayers = "group_upstream_account_layers_v1"
	protocolAnthropic     = "anthropic_messages_v1"
	protocolAccountEgress = "account_egress_v1"

	maxStatusRequestBytes = 1 << 10
)

// accountNamespaces maps the Gateway's account namespaces to providers. The
// names match the source-patched image's /internal/* routes one to one.
var accountNamespaces = []struct{ name, provider string }{
	{"upstream-accounts", ProviderCodex},
	{"antigravity-accounts", ProviderAntigravity},
	{"anthropic-accounts", ProviderClaude},
}

type managementRegistration struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

// managementRoutes declares the routes. None declares a menu: a menu route is
// also exposed as an unauthenticated browser resource.
func (p *Plugin) managementRoutes() managementRegistration {
	var routes []managementRoute
	for _, namespace := range accountNamespaces {
		base := managementPrefix + namespace.name
		routes = append(routes,
			managementRoute{http.MethodGet, base, "List upstream accounts"},
			managementRoute{http.MethodGet, base + "/capabilities", "Report the account access protocol"},
			managementRoute{http.MethodGet, base + "/funding-layers", "Report the funding layer protocol"},
			managementRoute{http.MethodGet, base + "/concurrency", "Sample active conversations per account"},
			managementRoute{http.MethodPut, base + "/status", "Enable or disable one account"},
			managementRoute{http.MethodPut, base + "/egress", "Apply one account's egress binding"},
		)
	}
	routes = append(routes, managementRoute{http.MethodPost, managementPrefix + "upstream-accounts/quota", "Report one Codex account's quota as CPA observed it"},
		managementRoute{http.MethodGet, managementPrefix + modelIdentificationPrefix + "capabilities", "Report the model identification protocol"},
		managementRoute{http.MethodGet, managementPrefix + modelIdentificationPrefix + "accounts/models", "List the models CPA registered for one Codex account"},
	)
	for _, provider := range facadeProviders {
		base := managementPrefix + facadeNamespace + "/" + provider.name
		routes = append(routes,
			managementRoute{http.MethodPost, base + "/accounts/quota", "Report one account's quota on the credential page"},
			managementRoute{http.MethodPost, base + "/oauth", "Start a login through CPA with a fixed egress"},
			managementRoute{http.MethodPost, base + "/oauth/callback", "Hand an authorization code to CPA"},
			managementRoute{http.MethodPost, base + "/oauth/status", "Report a login and bind its account"},
			managementRoute{http.MethodPost, base + "/credentials", "Import tokens and let CPA refresh them"},
			managementRoute{http.MethodPost, base + "/accounts/refresh", "Refresh one account's credential through CPA"},
			managementRoute{http.MethodDelete, base + "/accounts", "Delete one disabled, idle account's credential"},
		)
	}
	return managementRegistration{Routes: routes}
}

// The credential page reaches CPA under /internal/gateway-management/ in the
// source-patched image; the Gateway maps that prefix to this namespace.
const facadeNamespace = "gateway-management"

// facadeProviders maps the credential page's provider names to providers.
var facadeProviders = []struct{ name, provider string }{
	{"codex", ProviderCodex},
	{"antigravity", ProviderAntigravity},
	{"anthropic", ProviderClaude},
}

// facadeManagement serves the credential page's routes: quota, login,
// import, refresh and delete. Each operation is carried out by CPA's own
// management API; see credentials.go.
func (p *Plugin) facadeManagement(method, path string, query url.Values, body []byte) []byte {
	name, action, _ := strings.Cut(path, "/")
	provider := ""
	for _, candidate := range facadeProviders {
		if candidate.name == name {
			provider = candidate.provider
		}
	}
	if provider == "" {
		return managementError(http.StatusNotFound, "not_found")
	}
	if provider == ProviderAntigravity && !p.cfg.AntigravityEnabled {
		// During a bridge rollback this CPA must not touch Google credentials.
		return facadeError(http.StatusServiceUnavailable)
	}
	switch {
	case method == http.MethodPost && action == "accounts/quota":
		switch provider {
		case ProviderCodex:
			return p.codexQuotaFacade(query, body)
		case ProviderAntigravity:
			return p.antigravityQuotaFacade(query, body)
		default:
			return p.anthropicQuotaFacade(query, body)
		}
	case method == http.MethodPost && action == "oauth" && len(query) == 0:
		return p.oauthBegin(provider, body)
	case method == http.MethodPost && action == "oauth/callback" && len(query) == 0:
		return p.oauthCallback(provider, body)
	case method == http.MethodPost && action == "oauth/status" && len(query) == 0:
		return p.oauthStatus(provider, body)
	case method == http.MethodPost && action == "credentials" && len(query) == 0:
		return p.importCredential(provider, body)
	case method == http.MethodPost && action == "accounts/refresh":
		return p.refreshCredential(provider, query, body)
	case method == http.MethodDelete && action == "accounts":
		return p.deleteCredential(provider, query, body)
	}
	return managementError(http.StatusNotFound, "not_found")
}

func managementJSON(status int, value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"internal_error"}`)
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		Body:       body,
	})
}

func managementError(status int, code string) []byte {
	return managementJSON(status, map[string]string{"error": code})
}

type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (p *Plugin) handleManagement(raw []byte) []byte {
	var request managementRequest
	if json.Unmarshal(raw, &request) != nil {
		return managementError(http.StatusBadRequest, "invalid_request")
	}
	return p.Management(request.Method, request.Path, request.Query, request.Body)
}

// Management serves one authenticated management request.
func (p *Plugin) Management(method, path string, query url.Values, body []byte) []byte {
	if p.unavailable != nil {
		return managementError(http.StatusServiceUnavailable, "account_registry_unavailable")
	}
	path = strings.TrimPrefix(strings.TrimPrefix(path, managementBase), managementPrefix)
	if facade, ok := strings.CutPrefix(path, facadeNamespace+"/"); ok {
		return p.facadeManagement(method, facade, query, body)
	}
	if action, ok := strings.CutPrefix(path, modelIdentificationPrefix); ok {
		return p.modelIdentification(method, action, query)
	}
	name, action, _ := strings.Cut(path, "/")
	provider := ""
	for _, namespace := range accountNamespaces {
		if namespace.name == name {
			provider = namespace.provider
		}
	}
	if provider == "" {
		return managementError(http.StatusNotFound, "not_found")
	}
	if provider == ProviderAntigravity && !p.cfg.AntigravityEnabled {
		// During a bridge rollback this CPA must not touch Google credentials.
		return managementError(http.StatusServiceUnavailable, "provider_disabled")
	}
	switch {
	case method == http.MethodGet && action == "capabilities":
		return p.capabilities(provider)
	case method == http.MethodGet && action == "funding-layers":
		return managementJSON(http.StatusOK, map[string]string{"protocol": protocolFundingLayers})
	case method == http.MethodGet && action == "":
		return p.listAccounts(provider)
	case method == http.MethodGet && action == "concurrency":
		return p.concurrencySnapshot(provider)
	case method == http.MethodPut && action == "status":
		return p.setAccountStatus(provider, query, body)
	case method == http.MethodPut && action == "egress":
		return p.syncAccountEgress(provider, query, body)
	case method == http.MethodPost && action == "quota" && provider == ProviderCodex:
		return p.codexQuota(query, body)
	}
	return managementError(http.StatusNotFound, "not_found")
}

// capabilities is what the Gateway probes before every dispatch. A CPA without
// this plugin answers 404 and the Gateway refuses to dispatch, so the probe is
// a second fail-closed barrier behind the plugin-only selector.
//
// account_egress is advertised only when this plugin can enforce bindings,
// which needs the management key to write a credential's proxy. Without it a
// Gateway that requires bindings refuses to dispatch.
func (p *Plugin) capabilities(provider string) []byte {
	if _, err := p.inventory.Accounts(provider); err != nil {
		return managementError(http.StatusServiceUnavailable, "account_registry_unavailable")
	}
	response := map[string]string{"protocol": protocolAccountAccess, "group_upstream_account_scope": protocolGroupScope}
	if p.cfg.ManagementKey != "" {
		response["account_egress"] = protocolAccountEgress
	}
	if provider == ProviderClaude {
		response["anthropic_messages"] = protocolAnthropic
	}
	return managementJSON(http.StatusOK, response)
}

type accountView struct {
	ID                  string `json:"id"`
	MaskedEmail         string `json:"masked_email"`
	Plan                string `json:"plan"`
	Status              string `json:"status"`
	CliproxyStatus      string `json:"cliproxy_status"`
	GatewayManualStatus string `json:"gateway_manual_status"`
	GatewayQuotaStatus  string `json:"gateway_quota_status"`
	LastSyncedAt        string `json:"last_synced_at"`
}

// hostStatus reduces CPA's credential state to the Gateway's vocabulary.
func hostStatus(account Account) string {
	status := strings.ToLower(strings.TrimSpace(account.Status))
	switch {
	case account.Disabled || status == "disabled":
		return "disabled"
	case status == "error" || status == "unknown" || status == "":
		return "error"
	case account.Unavailable || status == "pending" || status == "refreshing":
		return "unavailable"
	case status == "active":
		return "active"
	}
	return "error"
}

// statuses returns the three independent sources and the merged result. An
// account is available only when all three agree.
func (p *Plugin) statuses(account Account) (host, manual, quota, final string) {
	host = hostStatus(account)
	manual, quota = p.control.Statuses(account.ID)
	final = "unavailable"
	if host == "active" && manual == "enabled" && quota == "available" {
		final = "available"
	}
	return host, manual, quota, final
}

func (p *Plugin) listAccounts(provider string) []byte {
	accounts, err := p.inventory.Accounts(provider)
	if err != nil {
		return managementError(http.StatusServiceUnavailable, "account_registry_unavailable")
	}
	now := p.now().UTC()
	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		host, manual, quota, final := p.statuses(account)
		synced := account.CreatedAt
		for _, candidate := range []time.Time{account.UpdatedAt, account.LastRefresh} {
			if candidate.After(synced) {
				synced = candidate
			}
		}
		if synced.IsZero() {
			synced = now
		}
		plan := account.Plan
		if plan == "" {
			plan = "unknown"
		}
		views = append(views, accountView{
			ID: account.ID, MaskedEmail: maskEmail(account.Email), Plan: plan, Status: final,
			CliproxyStatus: host, GatewayManualStatus: manual, GatewayQuotaStatus: quota,
			LastSyncedAt: synced.UTC().Format(time.RFC3339),
		})
	}
	return managementJSON(http.StatusOK, map[string]any{"accounts": views})
}

// maskEmail keeps the first character and the domain. Anything that is not a
// plain ASCII address becomes a fixed placeholder, so no free-form text from a
// credential file can reach the Gateway through this field.
func maskEmail(email string) string {
	const unknown = "u***@unknown.invalid"
	email = strings.TrimSpace(email)
	if strings.Count(email, "@") != 1 {
		return unknown
	}
	local, domain, _ := strings.Cut(email, "@")
	if local == "" || !asciiAlphanumeric(local[0]) || !safeDomain(domain) {
		return unknown
	}
	return string(local[0]) + "***@" + domain
}

func asciiAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func safeDomain(domain string) bool {
	if len(domain) < 2 || len(domain) > 249 || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !asciiAlphanumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func (p *Plugin) concurrencySnapshot(provider string) []byte {
	accounts, err := p.inventory.Accounts(provider)
	if err != nil {
		return managementError(http.StatusServiceUnavailable, "account_registry_unavailable")
	}
	known := make([]string, 0, len(accounts))
	owned := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		known = append(known, account.ID)
		owned[account.ID] = true
	}
	rows := make([]AccountConcurrency, 0, len(known))
	for _, row := range p.concurrency.Snapshot(known) {
		// The tracker is shared by every provider; report only this one's.
		if owned[row.ID] {
			rows = append(rows, row)
		}
	}
	return managementJSON(http.StatusOK, map[string]any{"sampled_at": p.now().UTC().Format(time.RFC3339Nano), "accounts": rows})
}

// decodeStatusRequest accepts exactly {"enabled":true} or {"enabled":false}.
func decodeStatusRequest(body []byte) (enabled, ok bool) {
	if len(body) > maxStatusRequestBytes {
		return false, false
	}
	fields, err := decodeObject(bytes.TrimSpace(body), "enabled")
	if err != nil {
		return false, false
	}
	switch string(fields["enabled"]) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func (p *Plugin) setAccountStatus(provider string, query url.Values, body []byte) []byte {
	ids := query["id"]
	if len(ids) != 1 || len(query) != 1 || !validAccountID(ids[0]) {
		return managementError(http.StatusNotFound, "upstream_account_not_found")
	}
	id := ids[0]
	enabled, ok := decodeStatusRequest(body)
	if !ok {
		return managementError(http.StatusBadRequest, "account_status_request_invalid")
	}
	account, found, err := p.inventory.Find(provider, id)
	if err != nil {
		return managementError(http.StatusServiceUnavailable, "account_registry_unavailable")
	}
	if !found {
		return managementError(http.StatusNotFound, "upstream_account_not_found")
	}
	if err = p.control.SetEnabled(id, enabled); err != nil {
		return managementError(http.StatusServiceUnavailable, "account_status_persistence_failed")
	}
	if enabled {
		// Re-enabling starts clean: the account is retried at once and locks
		// again only if the provider still reports exhaustion.
		if errReset := p.host.ResetCooldown(account.AuthIndex); errReset != nil {
			logf("cooldown of account %s was not cleared: %T", id, errReset)
		}
	} else {
		// Requests already running finish; idle holds must not delay the drain.
		p.concurrency.DropIdleHolds(id)
	}
	if refreshed, stillFound, errFind := p.inventory.Find(provider, id); errFind == nil && stillFound {
		account = refreshed
	}
	host, manual, quota, final := p.statuses(account)
	return managementJSON(http.StatusOK, map[string]string{
		"id": id, "status": final, "cliproxy_status": host, "gateway_manual_status": manual, "gateway_quota_status": quota,
	})
}

// syncAccountEgress applies one account's current egress binding. The Gateway
// calls it after the Owner changes the binding; the route itself is resolved
// from the Gateway again rather than taken from this request.
func (p *Plugin) syncAccountEgress(provider string, query url.Values, body []byte) []byte {
	ids := query["id"]
	if len(ids) != 1 || len(query) != 1 || !validAccountID(ids[0]) {
		return managementError(http.StatusNotFound, "upstream_account_not_found")
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) != 0 && string(trimmed) != "{}" {
		return managementError(http.StatusBadRequest, "account_egress_request_invalid")
	}
	account, found, err := p.inventory.Find(provider, ids[0])
	if err != nil {
		return managementError(http.StatusServiceUnavailable, "account_registry_unavailable")
	}
	if !found {
		return managementError(http.StatusNotFound, "upstream_account_not_found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*p.cfg.CallbackTimeout)
	defer cancel()
	revision, err := p.syncEgress(ctx, provider, account)
	if err != nil {
		return managementError(http.StatusServiceUnavailable, "account_egress_unavailable")
	}
	return managementJSON(http.StatusOK, map[string]string{"id": account.ID, "revision": revision})
}
