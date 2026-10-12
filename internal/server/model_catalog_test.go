package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

type catalogTestRepository struct {
	users  []string
	models []string
	err    error
}

func (f *catalogTestRepository) ListUsageModels(_ context.Context, userID string) ([]string, error) {
	f.users = append(f.users, userID)
	return f.models, f.err
}

func TestModelCatalogScopesAndHistoricalCandidates(t *testing.T) {
	for _, tc := range []struct {
		scope, role, user string
		status            int
		want              []sessionModelOption
	}{
		{"manageable", store.UserRoleMember, "", 403, nil},
		{"global_usage", store.UserRoleMember, "", 403, nil},
		{"manageable", store.UserRoleOwner, "", 200, []sessionModelOption{{"claude-test", "Claude"}, {"gemini-pro-agent", "Gemini"}, {"gpt-test", "GPT"}}},
		{"api_key", store.UserRoleMember, "", 200, []sessionModelOption{{"gpt-test", "GPT"}}},
		{"usage", store.UserRoleMember, "self-1", 200, []sessionModelOption{{"claude-retired", "Claude"}, {"claude-test", "Claude"}, {"gemini-pro-agent", "Gemini"}, {"gpt-test", "GPT"}}},
		{"global_usage", store.UserRoleOwner, "", 200, []sessionModelOption{{"claude-retired", "Claude"}, {"claude-test", "Claude"}, {"gemini-pro-agent", "Gemini"}, {"gpt-test", "GPT"}}},
		{"unknown", store.UserRoleOwner, "", 400, nil},
		{"usage&user_id=other", store.UserRoleOwner, "", 400, nil},
		{"usage&scope=global_usage", store.UserRoleOwner, "", 400, nil},
	} {
		t.Run(tc.scope+tc.role, func(t *testing.T) {
			s, _ := newBillingSourceTestServer(t, tc.role, nil)
			s.config.UsagePricing = config.UsagePricing{Models: map[string]config.ModelPricing{"gpt-test": {}, "claude-test": {}, "gemini-pro-agent": {}}}
			s.modelAccessRepo = &fakeModelAccessRepository{models: []store.ModelAccessModel{{Model: "gpt-test"}, {Model: "claude-test"}, {Model: "gemini-pro-agent"}}, enabledModels: []string{"gpt-test", "gemini-pro-agent"}}
			repo := &catalogTestRepository{models: []string{"claude-retired", "gpt-test"}}
			s.modelCatalogRepo = repo
			r := billingPlanTestRequest(http.MethodGet, "/admin/model-catalog?scope="+tc.scope, "")
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("scope: %d %s", w.Code, w.Body)
			}
			if tc.status == 200 {
				var result struct {
					Models []sessionModelOption `json:"models"`
				}
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || !reflect.DeepEqual(result.Models, tc.want) {
					t.Fatalf("catalog: %s", w.Body)
				}
				if tc.scope == "usage" || tc.scope == "global_usage" {
					if !reflect.DeepEqual(repo.users, []string{tc.user}) {
						t.Fatalf("history scope: %v", repo.users)
					}
				}
			}
		})
	}
}

func TestModelCatalogRequiresSessionAndFailsClosed(t *testing.T) {
	s, _ := newBillingSourceTestServer(t, store.UserRoleMember, nil)
	r := billingPlanTestRequest(http.MethodGet, "/admin/model-catalog", "")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("anonymous catalog: %d", w.Code)
	}
	s.modelAccessRepo = &fakeModelAccessRepository{listModelsErr: errors.New("catalog unavailable")}
	addBillingSourceTestSession(t, r)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != 500 {
		t.Fatalf("unavailable catalog: %d %s", w.Code, w.Body)
	}
}

func TestAPIKeyModelScopeRejectsEmptyRestrictedSelection(t *testing.T) {
	for _, tc := range []struct {
		scope  string
		models []string
		ok     bool
	}{
		{"unrestricted", nil, true}, {"unrestricted", []string{}, true}, {"unrestricted", []string{"gpt-test"}, false},
		{"restricted", nil, false}, {"restricted", []string{}, false}, {"restricted", []string{"gpt-test"}, true},
		{"restricted", []string{"gpt-test", "gpt-test"}, false}, {"restricted", []string{"bad model"}, false}, {"all", nil, false},
	} {
		if got := validAPIKeyModelScope(tc.scope, tc.models); got != tc.ok {
			t.Fatalf("scope=%s models=%v accepted=%v", tc.scope, tc.models, got)
		}
	}
}

func TestBillingPeriodAndModelValidation(t *testing.T) {
	for _, tc := range []struct {
		tier string
		days int
		ok   bool
	}{{"day", 1, true}, {"week", 7, true}, {"month", 31, true}, {"month", 30, false}, {"custom", 1, true}, {"custom", 3650, true}, {"custom", 0, false}, {"custom", 3651, false}, {"custom", -1, false}} {
		if validBillingPeriod(tc.tier, tc.days) != tc.ok {
			t.Fatalf("period %s/%d", tc.tier, tc.days)
		}
	}
	s := &Server{config: config.Config{UsagePricing: config.UsagePricing{Models: map[string]config.ModelPricing{"gpt-test": {}, config.InternalGovernanceModel: {}}}}}
	for _, models := range [][]string{nil, {}, {"invalid model"}, {"gpt-test", "gpt-test"}, {config.InternalGovernanceModel}} {
		if s.validBillingModels(models) {
			t.Fatalf("accepted models %v", models)
		}
	}
	if !s.validBillingModels([]string{"gpt-test"}) {
		t.Fatal("rejected configured model")
	}
}
