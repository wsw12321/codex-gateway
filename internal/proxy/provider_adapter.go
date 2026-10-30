package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ProviderRoute describes one exact provider/model/HTTP route capability.
// Method and Path are matched literally; no prefix or wildcard matching is
// performed. An empty Model denotes a route that is independent of a model
// (for example GET /v1/models).
type ProviderRoute struct {
	Provider string
	Model    string
	Method   string
	Path     string
}

// ProviderCapabilities is the immutable capability contract exposed by an
// adapter. Routes, when present, are authoritative. Models and Paths are
// convenient exact-match sets for providers whose methods share one route
// table. An empty set means "not constrained" for that dimension.
type ProviderCapabilities struct {
	Provider string
	Models   []string
	Paths    []string
	Routes   []ProviderRoute
}

// Supports reports whether model, method and path are an exact capability
// match. The two-argument form is retained for callers that only have a path;
// SupportsRequest should be preferred when the HTTP method is available.
func (c ProviderCapabilities) Supports(model, path string) bool {
	return c.SupportsRequest(http.MethodPost, model, path)
}

// SupportsRequest reports whether an exact provider/model/method/path route is
// supported. It deliberately does not normalize paths or accept prefixes.
func (c ProviderCapabilities) SupportsRequest(method, model, path string) bool {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" || path == "" {
		return false
	}
	if len(c.Routes) > 0 {
		for _, route := range c.Routes {
			if route.Provider != "" && c.Provider != "" && route.Provider != c.Provider {
				continue
			}
			if route.Method != "" && strings.ToUpper(route.Method) != method {
				continue
			}
			if route.Path != path || (route.Model != "" && route.Model != model) {
				continue
			}
			return true
		}
		return false
	}
	if len(c.Models) > 0 && model != "" && !containsExact(c.Models, model) {
		return false
	}
	if len(c.Paths) > 0 && !containsExact(c.Paths, path) && !containsExact(c.Paths, method+" "+path) {
		return false
	}
	return true
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TrustedRequestMetadata is the gateway-owned portion of a request. It must
// be constructed from authenticated/session state, never copied from request
// headers. Adapters validate UserID and AffinityScope before dispatching.
type TrustedRequestMetadata struct {
	UserID        string
	AffinityScope string
	Groups        []AllocationGroup

	// The callbacks receive validated provider attribution. They are optional
	// and intentionally carry no caller-provided header values.
	OnUpstreamAccount func(accountID string)
	OnConversation    func(conversationHash string)
	OnAttribution     func(Attribution)
}

// NewTrustedRequestMetadata validates and copies trusted request metadata.
func NewTrustedRequestMetadata(userID, affinityScope string, groups []AllocationGroup) (TrustedRequestMetadata, error) {
	metadata := TrustedRequestMetadata{UserID: userID, AffinityScope: affinityScope, Groups: cloneAllocationGroups(groups)}
	if err := metadata.Validate(); err != nil {
		return TrustedRequestMetadata{}, err
	}
	return metadata, nil
}

// ValidateTrustedRequestMetadata validates metadata without dispatching a
// request. Empty user and affinity values are permitted.
func ValidateTrustedRequestMetadata(metadata TrustedRequestMetadata) error {
	return metadata.Validate()
}

// Validate applies the same strict forms used at the provider boundary.
func (m TrustedRequestMetadata) Validate() error {
	if m.UserID != "" && !gatewayUserPattern.MatchString(m.UserID) {
		return errors.New("invalid trusted user identity")
	}
	if m.AffinityScope != "" && !affinityScopePattern.MatchString(m.AffinityScope) {
		return errors.New("invalid trusted affinity scope")
	}
	if len(m.Groups) > 0 {
		encoded, err := json.Marshal(m.Groups)
		if err != nil || len(encoded) > 8192 {
			return errors.New("trusted group context is too large")
		}
	}
	return nil
}

func cloneAllocationGroups(groups []AllocationGroup) []AllocationGroup {
	if len(groups) == 0 {
		return nil
	}
	copyGroups := make([]AllocationGroup, len(groups))
	for i, group := range groups {
		copyGroups[i] = group
		copyGroups[i].AccountIDs = append([]string(nil), group.AccountIDs...)
	}
	return copyGroups
}

func (m TrustedRequestMetadata) options() (ForwardOptions, error) {
	if err := m.Validate(); err != nil {
		return ForwardOptions{}, err
	}
	return ForwardOptions{
		UserID:            m.UserID,
		AffinityScope:     m.AffinityScope,
		Groups:            cloneAllocationGroups(m.Groups),
		OnUpstreamAccount: m.OnUpstreamAccount,
		OnConversation:    m.OnConversation,
	}, nil
}

// Attribution is the stable, provider-neutral attribution emitted by an
// adapter. Account and conversation values are populated only after strict
// response-header validation.
type Attribution struct {
	Provider          string
	Model             string
	UpstreamAccountID string
	ConversationHash  string
	UpstreamRequestID string
}

// AttributionFromResult converts the existing forwarding result to the
// adapter contract without exposing internal response headers.
func AttributionFromResult(provider, model string, result Result) Attribution {
	if model == "" {
		model = result.Model
	}
	return Attribution{
		Provider:          provider,
		Model:             model,
		UpstreamAccountID: result.UpstreamAccountID,
		ConversationHash:  result.ConversationHash,
		UpstreamRequestID: result.UpstreamRequestID,
	}
}

// NormalizedFailure is an alias for Failure so existing callers can consume
// adapter failures without conversion. NormalizeFailure returns a defensive
// copy with stable status/type/code/message fields and preserves Cause for
// errors.Is compatibility.
type NormalizedFailure = Failure

// NormalizeFailure makes a provider failure safe and stable at the adapter
// boundary. It does not mutate the failure supplied by an existing caller.
func NormalizeFailure(failure *Failure) *NormalizedFailure {
	if failure == nil {
		return nil
	}
	normalized := *failure
	if normalized.Status < 0 || normalized.Status > 599 {
		normalized.Status = http.StatusBadGateway
	}
	if normalized.Type == "" || safeIdentifier(normalized.Type) == "" {
		normalized.Type = "upstream_error"
	}
	if normalized.Code == "" || safeIdentifier(normalized.Code) == "" {
		normalized.Code = "upstream_error"
	}
	if normalized.Message == "" {
		normalized.Message = "upstream request failed"
	} else if safeUpstreamMessage(normalized.Message) == "" {
		normalized.Message = "upstream request failed"
	}
	if normalized.RetryAfter < 0 || normalized.RetryAfter > 86400 {
		normalized.RetryAfter = 0
	}
	return &normalized
}

// ProviderAdapter is the additive forwarding contract. Existing Client and
// Router constructors remain unchanged; use NewClientAdapter or
// NewRouterAdapter at new integration boundaries.
type ProviderAdapter interface {
	Capabilities() ProviderCapabilities
	Supports(model, path string) bool
	Forward(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, path string, metadata TrustedRequestMetadata) (Result, *NormalizedFailure)
	ForwardModels(ctx context.Context, w http.ResponseWriter, incoming *http.Request, allowedModels map[string]struct{}, metadata TrustedRequestMetadata) (Result, *NormalizedFailure)
}

// ClientAdapter adapts one existing Client to ProviderAdapter.
type ClientAdapter struct {
	client       *Client
	capabilities ProviderCapabilities
}

// NewClientAdapter wraps client without changing the client's existing
// behavior. Capabilities are copied so later caller mutations cannot alter
// dispatch decisions.
func NewClientAdapter(client *Client, capabilities ProviderCapabilities) *ClientAdapter {
	return &ClientAdapter{client: client, capabilities: cloneCapabilities(capabilities)}
}

// NewProviderAdapter is a descriptive alias for NewClientAdapter.
func NewProviderAdapter(client *Client, capabilities ProviderCapabilities) *ClientAdapter {
	return NewClientAdapter(client, capabilities)
}

func (a *ClientAdapter) Capabilities() ProviderCapabilities {
	if a == nil {
		return ProviderCapabilities{}
	}
	return cloneCapabilities(a.capabilities)
}

func (a *ClientAdapter) Supports(model, path string) bool {
	if a == nil {
		return false
	}
	return a.capabilities.SupportsRequest(http.MethodPost, model, path)
}

func (a *ClientAdapter) Forward(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, path string, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	if a == nil || a.client == nil {
		return Result{}, NormalizeFailure(&Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_unavailable", Message: "provider adapter is unavailable"})
	}
	if incoming == nil || !a.capabilities.SupportsRequest(incoming.Method, model, path) {
		return Result{}, unsupportedAdapterRoute()
	}
	options, err := metadata.options()
	if err != nil {
		return Result{}, invalidMetadataFailure(err)
	}
	result, failure := a.client.ForwardWithOptions(ctx, w, incoming, path, options)
	return a.finish(model, result, failure, metadata)
}

func (a *ClientAdapter) ForwardModels(ctx context.Context, w http.ResponseWriter, incoming *http.Request, allowedModels map[string]struct{}, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	if a == nil || a.client == nil {
		return Result{}, NormalizeFailure(&Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_unavailable", Message: "provider adapter is unavailable"})
	}
	if incoming == nil || !a.capabilities.SupportsRequest(http.MethodGet, "", "/v1/models") {
		return Result{}, unsupportedAdapterRoute()
	}
	options, err := metadata.options()
	if err != nil {
		return Result{}, invalidMetadataFailure(err)
	}
	result, failure := a.client.ForwardModelsWithOptions(ctx, w, incoming, allowedModels, options)
	return a.finish("", result, failure, metadata)
}

func (a *ClientAdapter) finish(model string, result Result, failure *Failure, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	if metadata.OnAttribution != nil {
		attribution := AttributionFromResult(a.capabilities.Provider, model, result)
		if attribution.UpstreamAccountID != "" || attribution.ConversationHash != "" || attribution.UpstreamRequestID != "" {
			metadata.OnAttribution(attribution)
		}
	}
	return result, NormalizeFailure(failure)
}

// RouterAdapter adapts an existing Router while preserving its exact model
// dispatch and independent upstream credentials.
type RouterAdapter struct {
	router       *Router
	capabilities ProviderCapabilities
}

func NewRouterAdapter(router *Router, capabilities ProviderCapabilities) *RouterAdapter {
	return &RouterAdapter{router: router, capabilities: cloneCapabilities(capabilities)}
}

func (a *RouterAdapter) Capabilities() ProviderCapabilities {
	if a == nil {
		return ProviderCapabilities{}
	}
	return cloneCapabilities(a.capabilities)
}

func (a *RouterAdapter) Supports(model, path string) bool {
	if a == nil {
		return false
	}
	return a.capabilities.SupportsRequest(http.MethodPost, model, path)
}

func (a *RouterAdapter) Forward(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, path string, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	if a == nil || a.router == nil {
		return Result{}, NormalizeFailure(&Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_unavailable", Message: "provider adapter is unavailable"})
	}
	if incoming == nil || !a.capabilities.SupportsRequest(incoming.Method, model, path) {
		return Result{}, unsupportedAdapterRoute()
	}
	options, err := metadata.options()
	if err != nil {
		return Result{}, invalidMetadataFailure(err)
	}
	result, failure := a.router.ForwardWithOptions(ctx, w, incoming, model, path, options)
	return a.finish(model, result, failure, metadata)
}

func (a *RouterAdapter) ForwardModels(ctx context.Context, w http.ResponseWriter, incoming *http.Request, allowedModels map[string]struct{}, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	if a == nil || a.router == nil {
		return Result{}, NormalizeFailure(&Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_unavailable", Message: "provider adapter is unavailable"})
	}
	if incoming == nil || !a.capabilities.SupportsRequest(http.MethodGet, "", "/v1/models") {
		return Result{}, unsupportedAdapterRoute()
	}
	options, err := metadata.options()
	if err != nil {
		return Result{}, invalidMetadataFailure(err)
	}
	result, failure := a.router.ForwardModelsWithOptions(ctx, w, incoming, allowedModels, options)
	return a.finish("", result, failure, metadata)
}

func (a *RouterAdapter) finish(model string, result Result, failure *Failure, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	if metadata.OnAttribution != nil {
		attribution := AttributionFromResult(a.capabilities.Provider, model, result)
		if attribution.UpstreamAccountID != "" || attribution.ConversationHash != "" || attribution.UpstreamRequestID != "" {
			metadata.OnAttribution(attribution)
		}
	}
	return result, NormalizeFailure(failure)
}

func cloneCapabilities(capabilities ProviderCapabilities) ProviderCapabilities {
	capabilities.Models = append([]string(nil), capabilities.Models...)
	capabilities.Paths = append([]string(nil), capabilities.Paths...)
	capabilities.Routes = append([]ProviderRoute(nil), capabilities.Routes...)
	return capabilities
}

func unsupportedAdapterRoute() *NormalizedFailure {
	return NormalizeFailure(&Failure{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "unsupported_endpoint", Message: "provider route is not supported"})
}

func invalidMetadataFailure(err error) *NormalizedFailure {
	return NormalizeFailure(&Failure{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "invalid_trusted_metadata", Message: "invalid trusted request metadata", Cause: fmt.Errorf("trusted metadata: %w", err)})
}
