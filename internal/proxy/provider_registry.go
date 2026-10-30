package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// AdapterRegistry selects a provider adapter from gateway-owned route
// metadata.  The registry deliberately has no request-header based routing:
// callers must supply the authenticated model and the canonical upstream
// path, while provider attribution comes from the registered capability.
//
// Registration order is retained as the final tie-breaker.  More specific
// capabilities (an exact model route, or an exact model in Models) always win
// over a broad fallback adapter.  This lets a gateway register an explicit
// provider route and retain the existing Codex adapter as the legacy fallback.
type AdapterRegistry struct {
	entries []adapterRegistryEntry
}

type adapterRegistryEntry struct {
	name    string
	adapter ProviderAdapter
}

// ProviderAdapterRegistry is the descriptive name used at integration
// boundaries.  Keep AdapterRegistry as the short name for package callers.
type ProviderAdapterRegistry = AdapterRegistry

// NewAdapterRegistry creates a registry and registers the supplied adapters in
// order.  Nil adapters are ignored so optional providers can be wired without
// a separate branch at every call site.
func NewAdapterRegistry(adapters ...ProviderAdapter) *AdapterRegistry {
	registry := &AdapterRegistry{}
	for _, adapter := range adapters {
		_ = registry.Register(adapter)
	}
	return registry
}

// NewProviderAdapterRegistry is an alias with an explicit provider-oriented
// name for callers that prefer it.
func NewProviderAdapterRegistry(adapters ...ProviderAdapter) *AdapterRegistry {
	return NewAdapterRegistry(adapters...)
}

// Register appends an adapter while preserving registration order.  A nil
// adapter is rejected instead of creating an entry that could shadow a valid
// fallback route.
func (r *AdapterRegistry) Register(adapter ProviderAdapter) error {
	return r.RegisterNamed("", adapter)
}

// RegisterNamed appends an adapter with an optional local name.  Names are
// diagnostics only and are not read from an inbound request.
func (r *AdapterRegistry) RegisterNamed(name string, adapter ProviderAdapter) error {
	if r == nil {
		return errors.New("provider adapter registry is nil")
	}
	if adapter == nil {
		return errors.New("provider adapter is nil")
	}
	r.entries = append(r.entries, adapterRegistryEntry{name: name, adapter: adapter})
	return nil
}

// Len reports the number of registered adapters.  It is intentionally small
// and read-only so tests and diagnostics can verify optional provider wiring.
func (r *AdapterRegistry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.entries)
}

// Resolve returns the highest-specificity adapter for an exact method/model/
// path tuple.  No path normalization, prefix matching, or inbound provider
// header lookup is performed.
func (r *AdapterRegistry) Resolve(method, model, path string) ProviderAdapter {
	if r == nil {
		return nil
	}
	method = normalizeMethod(method)
	if method == "" || path == "" {
		return nil
	}
	var selected ProviderAdapter
	bestScore := -1
	for _, entry := range r.entries {
		capabilities := entry.adapter.Capabilities()
		if !capabilities.SupportsRequest(method, model, path) {
			continue
		}
		score := capabilitySpecificity(capabilities, method, model, path)
		if selected == nil || score > bestScore {
			selected, bestScore = entry.adapter, score
		}
	}
	return selected
}

// ResolveNamed is useful for diagnostics and tests.  The returned name is
// registry-local metadata and cannot influence dispatch.
func (r *AdapterRegistry) ResolveNamed(method, model, path string) (string, ProviderAdapter) {
	if r == nil {
		return "", nil
	}
	method = normalizeMethod(method)
	if method == "" || path == "" {
		return "", nil
	}
	var selected adapterRegistryEntry
	bestScore := -1
	for _, entry := range r.entries {
		capabilities := entry.adapter.Capabilities()
		if !capabilities.SupportsRequest(method, model, path) {
			continue
		}
		score := capabilitySpecificity(capabilities, method, model, path)
		if selected.adapter == nil || score > bestScore {
			selected, bestScore = entry, score
		}
	}
	if selected.adapter == nil {
		return "", nil
	}
	return selected.name, selected.adapter
}

// Forward dispatches a generation request through the selected adapter.  A
// missing route is a compact, deterministic 404 rather than a fallback based
// on untrusted request metadata.
func (r *AdapterRegistry) Forward(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, path string, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	method := ""
	if incoming != nil {
		method = incoming.Method
	}
	adapter := r.Resolve(method, model, path)
	if adapter == nil {
		return Result{}, unsupportedAdapterRoute()
	}
	result, failure := adapter.Forward(ctx, w, incoming, model, path, metadata)
	return result, NormalizeFailure(failure)
}

// ForwardModels dispatches GET /v1/models through the catalog-capable adapter.
// Catalog merging and bridge outage semantics remain the responsibility of
// the adapter (for example RouterAdapter wrapping the existing Router).
func (r *AdapterRegistry) ForwardModels(ctx context.Context, w http.ResponseWriter, incoming *http.Request, allowedModels map[string]struct{}, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	adapter := r.Resolve(http.MethodGet, "", "/v1/models")
	if adapter == nil {
		return Result{}, unsupportedAdapterRoute()
	}
	result, failure := adapter.ForwardModels(ctx, w, incoming, allowedModels, metadata)
	return result, NormalizeFailure(failure)
}

func normalizeMethod(method string) string {
	return strings.ToUpper(strings.TrimSpace(method))
}

func capabilitySpecificity(capabilities ProviderCapabilities, method, model, path string) int {
	best := 0
	for _, route := range capabilities.Routes {
		if route.Path != path || (route.Method != "" && normalizeMethod(route.Method) != method) || (route.Model != "" && route.Model != model) {
			continue
		}
		score := 30 // exact path
		if route.Method != "" {
			score += 20
		}
		if route.Model != "" {
			score += 100
		}
		if score > best {
			best = score
		}
	}
	if len(capabilities.Routes) > 0 {
		return best
	}
	if len(capabilities.Models) > 0 && model != "" && containsExact(capabilities.Models, model) {
		best += 100
	}
	if len(capabilities.Paths) > 0 && (containsExact(capabilities.Paths, path) || containsExact(capabilities.Paths, method+" "+path)) {
		best += 30
	}
	if capabilities.Provider != "" {
		best++
	}
	return best
}
