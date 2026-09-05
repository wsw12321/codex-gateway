package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestResponsesRequireUserModelAccessExceptInternalGovernance(t *testing.T) {
	pricing := config.UsagePricing{Models: map[string]config.ModelPricing{
		"gpt-6-astra":                  {},
		config.InternalGovernanceModel: {},
	}}
	if !requiresUserModelAccess(http.MethodPost, "gpt-6-astra", pricing) {
		t.Fatal("manageable POST model does not require user access")
	}
	if requiresUserModelAccess(http.MethodPost, config.InternalGovernanceModel, pricing) {
		t.Fatal("internal governance model unexpectedly requires managed access")
	}
	if requiresUserModelAccess(http.MethodGet, "gpt-6-astra", pricing) {
		t.Fatal("GET model catalog should resolve access separately")
	}
	if requiresUserModelAccess(http.MethodPost, "gpt-unpriced", pricing) {
		t.Fatal("unpriced model should be rejected before model-access admission")
	}
}

func TestIntersectAllowedModels(t *testing.T) {
	pricing := config.UsagePricing{Models: map[string]config.ModelPricing{
		"gpt-enabled":                  {},
		"gpt-user-disabled":            {},
		config.InternalGovernanceModel: {},
	}}
	tests := []struct {
		name        string
		userEnabled []string
		keyAllow    []string
		want        []string
	}{
		{
			name:        "pricing user and unrestricted key intersection",
			userEnabled: []string{"gpt-enabled", "gpt-unpriced"},
			want:        []string{config.InternalGovernanceModel, "gpt-enabled"},
		},
		{
			name:        "key allowlist narrows enabled models",
			userEnabled: []string{"gpt-enabled"},
			keyAllow:    []string{"gpt-enabled", "gpt-user-disabled", "gpt-unpriced"},
			want:        []string{"gpt-enabled"},
		},
		{
			name:        "key can allow internal governance model",
			userEnabled: nil,
			keyAllow:    []string{config.InternalGovernanceModel},
			want:        []string{config.InternalGovernanceModel},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := intersectAllowedModels(pricing, test.userEnabled, test.keyAllow)
			got := make([]string, 0, len(set))
			for model := range set {
				got = append(got, model)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("intersection = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestIntersectAllowedModelsCanBeEmpty(t *testing.T) {
	pricing := config.UsagePricing{Models: map[string]config.ModelPricing{"gpt-disabled": {}}}
	if got := intersectAllowedModels(pricing, nil, nil); len(got) != 0 {
		t.Fatalf("intersection = %#v, want empty", got)
	}
}

func TestAllowedModelsForAPIKeyFailsClosedOnMissingCatalogState(t *testing.T) {
	repository := &fakeModelAccessRepository{enabledModels: []string{"gpt-6-astra"}}
	server := newModelAccessHandlerServer(repository)
	if _, err := server.allowedModelsForAPIKey(context.Background(), store.APIKey{UserID: "user-1"}); err == nil {
		t.Fatal("missing configured model default did not fail closed")
	}
}

func TestAllowedModelsForAPIKeyUsesUserAndKeyIntersection(t *testing.T) {
	repository := &fakeModelAccessRepository{
		models:        []store.ModelAccessModel{{Model: "gpt-6-astra"}},
		enabledModels: []string{"gpt-6-astra", "gpt-unpriced"},
	}
	server := newModelAccessHandlerServer(repository)
	allowed, err := server.allowedModelsForAPIKey(context.Background(), store.APIKey{
		UserID: "user-1", ModelAllowlist: []string{"gpt-6-astra", "gpt-user-disabled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := allowed["gpt-6-astra"]; !ok || len(allowed) != 1 {
		t.Fatalf("allowed models = %#v", allowed)
	}

	repository.listEnabledErr = store.ErrModelAccessUnavailable
	if _, err := server.allowedModelsForAPIKey(context.Background(), store.APIKey{UserID: "user-1"}); !errors.Is(err, store.ErrModelAccessUnavailable) {
		t.Fatalf("missing user access error = %v", err)
	}
}

func TestWriteModelNotAllowedUsesStablePermissionError(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	writeModelNotAllowed(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "permission_error" || body.Error.Code != "model_not_allowed" {
		t.Fatalf("error = %#v", body.Error)
	}
}
