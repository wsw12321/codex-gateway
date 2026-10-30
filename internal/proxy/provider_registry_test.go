package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type registryTestAdapter struct {
	caps       ProviderCapabilities
	forwarded  int
	modelCalls int
}

func (a *registryTestAdapter) Capabilities() ProviderCapabilities { return a.caps }

func (a *registryTestAdapter) Supports(model, path string) bool {
	return a.caps.Supports(model, path)
}

func (a *registryTestAdapter) Forward(_ context.Context, _ http.ResponseWriter, _ *http.Request, model, _ string, metadata TrustedRequestMetadata) (Result, *NormalizedFailure) {
	a.forwarded++
	return Result{Model: model, StatusCode: http.StatusOK, UpstreamRequestID: metadata.UserID}, nil
}

func (a *registryTestAdapter) ForwardModels(_ context.Context, _ http.ResponseWriter, _ *http.Request, _ map[string]struct{}, _ TrustedRequestMetadata) (Result, *NormalizedFailure) {
	a.modelCalls++
	return Result{StatusCode: http.StatusOK}, nil
}

func TestAdapterRegistryPrefersExactModelAndKeepsFallback(t *testing.T) {
	const exactModel = "gemini-exact"
	exact := &registryTestAdapter{caps: ProviderCapabilities{Provider: "antigravity", Routes: []ProviderRoute{{Provider: "antigravity", Model: exactModel, Method: http.MethodPost, Path: "/v1/responses"}}}}
	fallback := &registryTestAdapter{caps: ProviderCapabilities{Provider: "codex", Routes: []ProviderRoute{{Provider: "codex", Method: http.MethodPost, Path: "/v1/responses"}}}}
	registry := NewAdapterRegistry(exact, fallback)

	if got := registry.Resolve(http.MethodPost, exactModel, "/v1/responses"); got != exact {
		t.Fatalf("exact model resolved to %T, want exact adapter", got)
	}
	if got := registry.Resolve(http.MethodPost, "gpt-fallback", "/v1/responses"); got != fallback {
		t.Fatalf("fallback model resolved to %T, want fallback adapter", got)
	}
	if got := registry.Resolve(http.MethodPost, exactModel, "/v1/responses/"); got != nil {
		t.Fatalf("compact path suffix unexpectedly resolved to %T", got)
	}
}

func TestAdapterRegistryDoesNotTrustInboundProviderHeader(t *testing.T) {
	const model = "gpt-exact"
	adapter := &registryTestAdapter{caps: ProviderCapabilities{Provider: "codex", Routes: []ProviderRoute{{Provider: "codex", Model: model, Method: http.MethodPost, Path: "/v1/responses"}}}}
	registry := NewAdapterRegistry(adapter)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("X-Provider", "antigravity")
	_, failure := registry.Forward(context.Background(), httptest.NewRecorder(), request, model, "/v1/responses", TrustedRequestMetadata{UserID: "00000000-0000-0000-0000-000000000001"})
	if failure != nil || adapter.forwarded != 1 {
		t.Fatalf("registry trusted inbound provider header: failure=%v forwarded=%d", failure, adapter.forwarded)
	}
}

func TestAdapterRegistryCatalogRouteUsesRegisteredAdapter(t *testing.T) {
	adapter := &registryTestAdapter{caps: ProviderCapabilities{Provider: "gateway", Routes: []ProviderRoute{{Provider: "gateway", Method: http.MethodGet, Path: "/v1/models"}}}}
	registry := NewAdapterRegistry(adapter)
	_, failure := registry.ForwardModels(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil), nil, TrustedRequestMetadata{})
	if failure != nil || adapter.modelCalls != 1 {
		t.Fatalf("catalog dispatch failure=%v calls=%d", failure, adapter.modelCalls)
	}
}

func TestAdapterRegistryLeavesLegacyRouterPathValidationIntact(t *testing.T) {
	base, _ := url.Parse("http://registry.invalid")
	client := NewWithHTTPClient(base, "internal-secret", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported path reached upstream transport")
		return nil, nil
	})})
	registry := NewAdapterRegistry(NewRouterAdapter(NewRouter(client, nil, nil), ProviderCapabilities{Provider: "gateway"}))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, failure := registry.Forward(context.Background(), httptest.NewRecorder(), request, "gpt-test", "/v1/chat/completions", TrustedRequestMetadata{})
	if failure == nil || failure.Code != "unsupported_endpoint" || failure.Status != http.StatusNotFound {
		t.Fatalf("legacy path validation changed: failure=%v", failure)
	}
}
