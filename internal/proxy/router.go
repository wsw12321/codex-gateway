package proxy

import (
	"context"
	"net/http"
)

// Router selects the CPA provider client by exact configured public model ID.
// Antigravity IDs are native CPA IDs; request bodies remain unchanged.
type Router struct {
	primary     *Client
	antigravity *Client
	routes      map[string]string
}

func NewRouter(primary, antigravity *Client, routes map[string]string) *Router {
	copyRoutes := make(map[string]string, len(routes))
	for public, native := range routes {
		copyRoutes[public] = native
	}
	return &Router{primary: primary, antigravity: antigravity, routes: copyRoutes}
}

func (r *Router) IsAntigravityModel(model string) bool {
	_, routed := r.routes[model]
	return routed
}

func (r *Router) ForwardWithOptions(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, path string, options ForwardOptions) (Result, *Failure) {
	if !r.IsAntigravityModel(model) {
		return r.primary.ForwardWithOptions(ctx, w, incoming, path, options)
	}
	if path == "/v1/responses/compact" {
		return Result{}, &Failure{Status: http.StatusNotImplemented, Type: "invalid_request_error", Code: "endpoint_not_supported", Message: "Antigravity 不支持 /v1/responses/compact"}
	}
	if r.antigravity == nil {
		return Result{}, &Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_unavailable", Message: "Antigravity 尚未就绪"}
	}
	return r.antigravity.ForwardWithOptions(ctx, w, incoming, path, options)
}

// ForwardModelsWithOptions serves the catalog of the one CPA instance, which
// lists every provider's models.
func (r *Router) ForwardModelsWithOptions(ctx context.Context, w http.ResponseWriter, incoming *http.Request, allowedModels map[string]struct{}, options ForwardOptions) (Result, *Failure) {
	if incoming.Method != http.MethodGet {
		return Result{}, &Failure{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "unsupported_endpoint", Message: "不支持的接口"}
	}
	return r.primary.ForwardModelsWithOptions(ctx, w, incoming, allowedModels, options)
}
