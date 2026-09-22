package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Router selects an upstream using exact configured public model IDs. The
// bridge owns the public-to-CLI translation; request bodies remain unchanged.
type Router struct {
	primary     *Client
	antigravity *Client
	routes      map[string]string
}

func NewRouter(primary, antigravity *Client, routes map[string]string) *Router {
	copyRoutes := make(map[string]string, len(routes))
	for public, cli := range routes {
		copyRoutes[public] = cli
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
	// Codex account affinity is not part of the bridge's protocol.
	return r.antigravity.ForwardWithOptions(ctx, w, incoming, path, ForwardOptions{
		OnUpstreamAccount: options.OnUpstreamAccount,
	})
}

type catalogFetch struct {
	body    []byte
	result  Result
	failure *Failure
}

func (r *Router) ForwardModelsWithOptions(ctx context.Context, w http.ResponseWriter, incoming *http.Request, allowedModels map[string]struct{}, options ForwardOptions) (Result, *Failure) {
	if incoming.Method != http.MethodGet {
		return Result{}, &Failure{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "unsupported_endpoint", Message: "不支持的接口"}
	}
	if len(r.routes) == 0 {
		return r.primary.ForwardModelsWithOptions(ctx, w, incoming, allowedModels, options)
	}
	// A bridge outage must not hold the existing catalog hostage. Catalog
	// requests get a short deadline independent from long generation calls.
	bridgeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	bridgeResult := make(chan catalogFetch, 1)
	if r.antigravity != nil {
		go func() {
			body, result, failure := r.antigravity.fetchModelCatalog(bridgeCtx, incoming, ForwardOptions{})
			bridgeResult <- catalogFetch{body: body, result: result, failure: failure}
		}()
	} else {
		bridgeResult <- catalogFetch{failure: &Failure{Code: "upstream_unavailable"}}
	}
	body, result, failure := r.primary.fetchModelCatalog(ctx, incoming, options)
	if failure != nil {
		return result, failure
	}
	bridge := <-bridgeResult
	if ctx.Err() != nil {
		return result, transportFailure(ctx, ctx.Err())
	}
	if bridge.failure != nil {
		if bridge.failure.Code == "upstream_invalid_model_catalog" {
			return result, bridge.failure
		}
		bridge.body = nil
	}
	merged, err := mergeModelCatalogs(body, bridge.body, r.routes, allowedModels)
	if err != nil {
		return result, invalidModelCatalogFailure(err)
	}
	return writeModelCatalog(w, merged, result)
}

func mergeModelCatalogs(primary, bridge []byte, routes map[string]string, allowedModels map[string]struct{}) ([]byte, error) {
	// Validate complete catalogs before applying visibility filters, so an
	// unauthorized duplicate model cannot silently change upstream ownership.
	if _, err := filterModelCatalog(primary, nil); err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(primary, &envelope); err != nil {
		return nil, err
	}
	var primaryEntries []json.RawMessage
	if err := json.Unmarshal(envelope["data"], &primaryEntries); err != nil {
		return nil, err
	}
	entries := make([]json.RawMessage, 0, len(primaryEntries))
	seen := make(map[string]struct{}, len(primaryEntries))
	for _, entry := range primaryEntries {
		var model struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(entry, &model)
		if _, duplicate := seen[model.ID]; duplicate {
			return nil, fmt.Errorf("primary catalog repeats model %q", model.ID)
		}
		seen[model.ID] = struct{}{}
		if _, routed := routes[model.ID]; !routed {
			entries = append(entries, entry)
		}
	}
	if len(bridge) > 0 {
		if _, err := filterModelCatalog(bridge, nil); err != nil {
			return nil, err
		}
		var catalog struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(bridge, &catalog); err != nil {
			return nil, err
		}
		for _, entry := range catalog.Data {
			var model struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(entry, &model)
			if _, duplicate := seen[model.ID]; duplicate {
				return nil, fmt.Errorf("upstream catalogs repeat model %q", model.ID)
			}
			seen[model.ID] = struct{}{}
			if _, routed := routes[model.ID]; routed {
				entries = append(entries, entry)
			}
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	envelope["data"] = encoded
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	if len(body) > 2*maxInternalResponseBodyBytes {
		return nil, errors.New("merged model catalog exceeds 2 MiB")
	}
	return filterModelCatalog(body, allowedModels)
}
