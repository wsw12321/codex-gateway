package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProviderCapabilitiesMatchExactModelAndPath(t *testing.T) {
	capabilities := ProviderCapabilities{Provider: "codex", Routes: []ProviderRoute{
		{Provider: "codex", Model: "gpt-test", Method: http.MethodPost, Path: "/v1/responses"},
		{Provider: "codex", Model: "gpt-test", Method: http.MethodPost, Path: "/v1/responses/compact"},
		{Provider: "codex", Method: http.MethodGet, Path: "/v1/models"},
	}}
	for _, test := range []struct {
		name, method, model, path string
		want                      bool
	}{
		{name: "response", method: http.MethodPost, model: "gpt-test", path: "/v1/responses", want: true},
		{name: "compact", method: http.MethodPost, model: "gpt-test", path: "/v1/responses/compact", want: true},
		{name: "wrong model", method: http.MethodPost, model: "gpt-other", path: "/v1/responses"},
		{name: "wrong path", method: http.MethodPost, model: "gpt-test", path: "/v1/responses/compact/extra"},
		{name: "wrong method", method: http.MethodGet, model: "gpt-test", path: "/v1/responses"},
		{name: "catalog", method: http.MethodGet, path: "/v1/models", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := capabilities.SupportsRequest(test.method, test.model, test.path)
			if got != test.want {
				t.Fatalf("SupportsRequest(%q, %q, %q) = %v, want %v", test.method, test.model, test.path, got, test.want)
			}
		})
	}
}

func TestTrustedRequestMetadataStrictValidationAndCopy(t *testing.T) {
	const validUser = "aabbccdd-aabb-ccdd-aabb-ccddeeff0011"
	const validAffinity = "0123456789012345678901234567890123456789012"
	if _, err := NewTrustedRequestMetadata(validUser, validAffinity, nil); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, user, affinity string
	}{
		{name: "uppercase user", user: strings.ToUpper(validUser), affinity: validAffinity},
		{name: "short user", user: "00000000-0000-0000-0000-00000000000", affinity: validAffinity},
		{name: "affinity whitespace", user: validUser, affinity: " " + validAffinity},
		{name: "affinity too short", user: validUser, affinity: validAffinity[:42]},
		{name: "affinity punctuation", user: validUser, affinity: validAffinity[:42] + "/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewTrustedRequestMetadata(test.user, test.affinity, nil); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	groups := []AllocationGroup{{GroupID: "g", AccountIDs: []string{"0123456789abcdef"}}}
	metadata, err := NewTrustedRequestMetadata(validUser, validAffinity, groups)
	if err != nil {
		t.Fatal(err)
	}
	groups[0].AccountIDs[0] = "mutated"
	if metadata.Groups[0].AccountIDs[0] != "0123456789abcdef" {
		t.Fatal("metadata did not copy nested group values")
	}
}

func TestClientAdapterExactDispatchValidationAndSpoofStripping(t *testing.T) {
	const model = "gpt-test"
	const scope = "0123456789012345678901234567890123456789012"
	calls := 0
	base, _ := url.Parse("http://adapter.invalid")
	client := NewWithHTTPClient(base, "internal-secret", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") != "Bearer internal-secret" || request.Header.Get("Cookie") != "" || request.Header.Get(upstreamAccountHeader) != "" {
			return nil, errors.New("spoofed credential crossed provider boundary")
		}
		if request.Header.Get(affinityHeader) != scope {
			return nil, errors.New("trusted affinity missing")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, upstreamAccountHeader: {"0123456789abcdef"}}, Body: io.NopCloser(strings.NewReader(`{"model":"gpt-test"}`))}, nil
	})})
	adapter := NewClientAdapter(client, ProviderCapabilities{Provider: "codex", Routes: []ProviderRoute{{Provider: "codex", Model: model, Method: http.MethodPost, Path: "/v1/responses"}}})
	request := httptest.NewRequest(http.MethodPost, "https://gateway.invalid/v1/responses", strings.NewReader(`{"model":"gpt-test"}`))
	request.Header.Set("Authorization", "Bearer caller-secret")
	request.Header.Set("Cookie", "caller-cookie")
	request.Header.Set(upstreamAccountHeader, "fedcba9876543210")
	request.Header.Set(affinityHeader, "spoofed")
	recorder := httptest.NewRecorder()
	result, failure := adapter.Forward(context.Background(), recorder, request, model, "/v1/responses", TrustedRequestMetadata{AffinityScope: scope})
	if failure != nil || result.UpstreamAccountID != "0123456789abcdef" || calls != 1 {
		t.Fatalf("result=%+v failure=%v calls=%d", result, failure, calls)
	}
	_, failure = adapter.Forward(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)), "gpt-other", "/v1/responses", TrustedRequestMetadata{})
	if failure == nil || failure.Code != "unsupported_endpoint" || calls != 1 {
		t.Fatalf("unknown model failure=%v calls=%d", failure, calls)
	}
	_, failure = adapter.Forward(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)), model, "/v1/responses", TrustedRequestMetadata{AffinityScope: "invalid"})
	if failure == nil || failure.Code != "invalid_trusted_metadata" || failure.Status != http.StatusBadRequest || calls != 1 {
		t.Fatalf("invalid metadata failure=%v calls=%d", failure, calls)
	}
}

func TestNormalizeFailureDefensiveAndCompatibility(t *testing.T) {
	cause := errors.New("transport")
	failure := &Failure{Status: 700, Type: "bad type", Code: "bad code", Message: "cookie=secret", RetryAfter: -1, Cause: cause}
	normalized := NormalizeFailure(failure)
	if normalized == failure || normalized.Status != http.StatusBadGateway || normalized.Type != "upstream_error" || normalized.Code != "upstream_error" || normalized.Message != "upstream request failed" || normalized.RetryAfter != 0 || !errors.Is(normalized, cause) {
		t.Fatalf("normalized=%+v", normalized)
	}
	if failure.Status != 700 || failure.Type != "bad type" || failure.Code != "bad code" {
		t.Fatalf("input mutated: %+v", failure)
	}
	if NormalizeFailure(nil) != nil {
		t.Fatal("nil failure was not preserved")
	}
}
