package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// AccountAccessProtocolV1 is the capability marker required before an account
// management operation is sent to a CPA sidecar. It is intentionally checked
// on every operation; a sidecar replacement must not inherit a stale probe.
const AccountAccessProtocolV1 = "upstream_account_access_v1"

// AnthropicMessagesProtocolV1 is the additional marker required by the
// Anthropic Messages adapter. It is kept separate from the account protocol so
// a Codex-compatible sidecar cannot accidentally serve Claude traffic.
const AnthropicMessagesProtocolV1 = "anthropic_messages_v1"

// ProviderAccountCapabilities is the small, provider-neutral capability
// projection exposed by the gateway account facade. The booleans are true only
// after the sidecar has returned the current, exact protocol markers.
type ProviderAccountCapabilities struct {
	Provider          string `json:"provider"`
	Protocol          string `json:"protocol"`
	AnthropicMessages string `json:"anthropic_messages,omitempty"`
	List              bool   `json:"list"`
	Concurrency       bool   `json:"concurrency"`
	Status            bool   `json:"status"`
	Quota             bool   `json:"quota"`
}

// AccountProvider is the provider-neutral management contract. Implementations
// must scope every call to one provider; callers must not infer a provider
// from request headers or silently fall back to another provider.
type AccountProvider interface {
	Provider() string
	Capabilities(context.Context) (ProviderAccountCapabilities, error)
	ListAccounts(context.Context) ([]UpstreamAccount, error)
	Concurrency(context.Context) (UpstreamConcurrency, error)
	SetStatus(context.Context, string, bool) (UpstreamAccountStatus, error)
	Quota(context.Context, string) (UpstreamQuota, error)
}

// ClientAccountAdapter reuses a Client's authenticated CPA account APIs. The
// client constructors (New, NewCPAAntigravity and NewCPAAnthropic) retain
// provider-specific sidecar path isolation; this adapter never rewrites paths.
type ClientAccountAdapter struct {
	provider string
	client   *Client
}

// NewClientAccountAdapter wraps one existing client for a named provider. The
// constructor is deliberately non-panicking: a nil client or unknown provider
// is reported by the first operation as a stable InternalAPIError.
func NewClientAccountAdapter(provider string, client *Client) *ClientAccountAdapter {
	return &ClientAccountAdapter{provider: strings.TrimSpace(strings.ToLower(provider)), client: client}
}

func (a *ClientAccountAdapter) Provider() string {
	if a == nil {
		return ""
	}
	return a.provider
}

func (a *ClientAccountAdapter) validate() error {
	if a == nil || a.client == nil {
		return &InternalAPIError{StatusCode: http.StatusServiceUnavailable, Code: "provider_account_unavailable"}
	}
	switch a.provider {
	case "codex", "antigravity", "anthropic":
	default:
		return &InternalAPIError{StatusCode: http.StatusNotFound, Code: "provider_not_configured"}
	}
	// A provider name is bound to the constructor that owns its sidecar
	// namespace. This rejects a legacy Antigravity bridge (or a Codex client)
	// accidentally registered under another provider instead of rewriting the
	// request to a fallback namespace.
	if actual := a.client.AccountProvider(); actual != a.provider {
		return &InternalAPIError{StatusCode: http.StatusServiceUnavailable, Code: "provider_account_isolation"}
	}
	return nil
}

func (a *ClientAccountAdapter) Capabilities(ctx context.Context) (ProviderAccountCapabilities, error) {
	if err := a.validate(); err != nil {
		return ProviderAccountCapabilities{}, err
	}
	capabilities, err := a.client.AccountCapabilities(ctx)
	if err != nil {
		return ProviderAccountCapabilities{}, err
	}
	capabilities.Provider = a.provider
	capabilities.List, capabilities.Concurrency, capabilities.Status, capabilities.Quota = true, true, true, true
	return capabilities, nil
}

func (a *ClientAccountAdapter) ensure(ctx context.Context) error {
	_, err := a.Capabilities(ctx)
	return err
}

func (a *ClientAccountAdapter) ListAccounts(ctx context.Context) ([]UpstreamAccount, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	if err := a.ensure(ctx); err != nil {
		return nil, err
	}
	return a.client.ListUpstreamAccounts(ctx)
}

func (a *ClientAccountAdapter) Concurrency(ctx context.Context) (UpstreamConcurrency, error) {
	if err := a.validate(); err != nil {
		return UpstreamConcurrency{}, err
	}
	if err := a.ensure(ctx); err != nil {
		return UpstreamConcurrency{}, err
	}
	return a.client.UpstreamAccountConcurrency(ctx)
}

func (a *ClientAccountAdapter) SetStatus(ctx context.Context, accountID string, enabled bool) (UpstreamAccountStatus, error) {
	if err := a.validate(); err != nil {
		return UpstreamAccountStatus{}, err
	}
	// Validate before probing capabilities so malformed IDs never cause a
	// sidecar request. The underlying Client repeats this check defensively.
	if !upstreamAccountPattern.MatchString(accountID) {
		return UpstreamAccountStatus{}, &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "invalid_upstream_account"}
	}
	if err := a.ensure(ctx); err != nil {
		return UpstreamAccountStatus{}, err
	}
	return a.client.SetUpstreamAccountStatus(ctx, accountID, enabled)
}

func (a *ClientAccountAdapter) Quota(ctx context.Context, accountID string) (UpstreamQuota, error) {
	if err := a.validate(); err != nil {
		return UpstreamQuota{}, err
	}
	if !upstreamAccountPattern.MatchString(accountID) {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "invalid_upstream_account"}
	}
	if err := a.ensure(ctx); err != nil {
		return UpstreamQuota{}, err
	}
	return a.client.QueryUpstreamAccountQuota(ctx, accountID)
}

// AccountFacade is a provider-keyed view over account adapters. All lookups
// are exact; an unknown or unregistered provider returns an error and never
// falls back to a different adapter.
type AccountFacade interface {
	Resolve(provider string) AccountProvider
	Capabilities(context.Context, string) (ProviderAccountCapabilities, error)
	ListAccounts(context.Context, string) ([]UpstreamAccount, error)
	Concurrency(context.Context, string) (UpstreamConcurrency, error)
	SetStatus(context.Context, string, string, bool) (UpstreamAccountStatus, error)
	Quota(context.Context, string, string) (UpstreamQuota, error)
}

// ProviderAccountFacade is the default implementation of AccountFacade.
type ProviderAccountFacade struct {
	providers map[string]AccountProvider
}

// NewProviderAccountFacade creates an exact provider registry. Nil adapters are
// ignored, while duplicate provider names replace nothing (the first
// registration remains authoritative) to prevent accidental cross-provider
// shadowing during optional-provider wiring.
func NewProviderAccountFacade(adapters ...AccountProvider) *ProviderAccountFacade {
	facade := &ProviderAccountFacade{providers: make(map[string]AccountProvider, len(adapters))}
	for _, adapter := range adapters {
		_ = facade.Register(adapter)
	}
	return facade
}

// NewAccountFacade is an explicit, short alias for NewProviderAccountFacade.
func NewAccountFacade(adapters ...AccountProvider) *ProviderAccountFacade {
	return NewProviderAccountFacade(adapters...)
}

// ProviderAccountRegistry is a descriptive alias for ProviderAccountFacade.
type ProviderAccountRegistry = ProviderAccountFacade

// NewProviderAccountRegistry is an alias for NewProviderAccountFacade.
func NewProviderAccountRegistry(adapters ...AccountProvider) *ProviderAccountFacade {
	return NewProviderAccountFacade(adapters...)
}

func (f *ProviderAccountFacade) Register(adapter AccountProvider) error {
	if f == nil {
		return &InternalAPIError{StatusCode: http.StatusServiceUnavailable, Code: "provider_account_unavailable"}
	}
	if adapter == nil {
		return &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "provider_not_configured"}
	}
	provider := strings.TrimSpace(strings.ToLower(adapter.Provider()))
	if provider == "" || safeIdentifier(provider) != provider {
		return &InternalAPIError{StatusCode: http.StatusNotFound, Code: "provider_not_configured"}
	}
	if f.providers == nil {
		f.providers = make(map[string]AccountProvider)
	}
	if _, exists := f.providers[provider]; exists {
		return &InternalAPIError{StatusCode: http.StatusConflict, Code: "provider_already_registered"}
	}
	f.providers[provider] = adapter
	return nil
}

func (f *ProviderAccountFacade) Resolve(provider string) AccountProvider {
	if f == nil {
		return nil
	}
	return f.providers[strings.TrimSpace(strings.ToLower(provider))]
}

func (f *ProviderAccountFacade) provider(provider string) (AccountProvider, error) {
	if adapter := f.Resolve(provider); adapter != nil {
		return adapter, nil
	}
	return nil, &InternalAPIError{StatusCode: http.StatusNotFound, Code: "provider_not_configured"}
}

func (f *ProviderAccountFacade) Capabilities(ctx context.Context, provider string) (ProviderAccountCapabilities, error) {
	adapter, err := f.provider(provider)
	if err != nil {
		return ProviderAccountCapabilities{}, err
	}
	return adapter.Capabilities(ctx)
}

func (f *ProviderAccountFacade) ListAccounts(ctx context.Context, provider string) ([]UpstreamAccount, error) {
	adapter, err := f.provider(provider)
	if err != nil {
		return nil, err
	}
	return adapter.ListAccounts(ctx)
}

func (f *ProviderAccountFacade) Concurrency(ctx context.Context, provider string) (UpstreamConcurrency, error) {
	adapter, err := f.provider(provider)
	if err != nil {
		return UpstreamConcurrency{}, err
	}
	return adapter.Concurrency(ctx)
}

func (f *ProviderAccountFacade) SetStatus(ctx context.Context, provider, accountID string, enabled bool) (UpstreamAccountStatus, error) {
	adapter, err := f.provider(provider)
	if err != nil {
		return UpstreamAccountStatus{}, err
	}
	return adapter.SetStatus(ctx, accountID, enabled)
}

func (f *ProviderAccountFacade) Quota(ctx context.Context, provider, accountID string) (UpstreamQuota, error) {
	adapter, err := f.provider(provider)
	if err != nil {
		return UpstreamQuota{}, err
	}
	return adapter.Quota(ctx, accountID)
}

// AccountCapabilities probes the exact sidecar account protocol for this
// client. It is intentionally uncached so a sidecar restart or downgrade is
// observed before the next management operation.
func (c *Client) AccountProvider() string {
	if c == nil {
		return ""
	}
	if c.anthropic {
		return "anthropic"
	}
	if c.cpaNative {
		return "antigravity"
	}
	if c.antigravity {
		// The legacy bridge does not implement the CPA management namespace.
		return ""
	}
	return "codex"
}

func (c *Client) AccountCapabilities(ctx context.Context) (ProviderAccountCapabilities, error) {
	if c == nil {
		return ProviderAccountCapabilities{}, &InternalAPIError{StatusCode: http.StatusServiceUnavailable, Code: "provider_account_unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var response struct {
		Protocol          string `json:"protocol"`
		AnthropicMessages string `json:"anthropic_messages,omitempty"`
	}
	if err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts/capabilities", &response); err != nil {
		return ProviderAccountCapabilities{}, err
	}
	if response.Protocol != AccountAccessProtocolV1 || (c.anthropic && response.AnthropicMessages != AnthropicMessagesProtocolV1) {
		return ProviderAccountCapabilities{}, &InternalAPIError{StatusCode: http.StatusServiceUnavailable, Code: "upstream_access_protocol_unavailable"}
	}
	return ProviderAccountCapabilities{Protocol: response.Protocol, AnthropicMessages: response.AnthropicMessages}, nil
}

// RequireAccountAccessCapability is the error-only form used by callers that
// need a probe without retaining capability details.
func (c *Client) RequireAccountAccessCapability(ctx context.Context) error {
	_, err := c.AccountCapabilities(ctx)
	return err
}
