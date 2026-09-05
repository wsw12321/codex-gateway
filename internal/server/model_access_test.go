package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

type fakeModelAccessRepository struct {
	models         []store.ModelAccessModel
	users          []store.ModelAccessUser
	enabledModels  []string
	listModelsErr  error
	listUsersErr   error
	listEnabledErr error
	defaultErr     error
	usersErr       error
	defaultParams  store.SetModelAccessDefaultParams
	userParams     store.SetUserModelAccessParams
	defaultCalls   int
	userCalls      int
}

func (f *fakeModelAccessRepository) ListModelAccessModels(context.Context) ([]store.ModelAccessModel, error) {
	return f.models, f.listModelsErr
}

func (f *fakeModelAccessRepository) ListModelAccessUsers(context.Context, string) ([]store.ModelAccessUser, error) {
	return f.users, f.listUsersErr
}

func (f *fakeModelAccessRepository) ListEnabledModelsForUser(context.Context, string) ([]string, error) {
	return f.enabledModels, f.listEnabledErr
}

func (f *fakeModelAccessRepository) SetModelAccessDefault(_ context.Context, params store.SetModelAccessDefaultParams) (store.ModelAccessChangeResult, error) {
	f.defaultCalls++
	f.defaultParams = params
	return store.ModelAccessChangeResult{
		Model: params.Model, Enabled: params.Enabled, Scope: store.ModelAccessScopeDefault,
		TargetCount: 1, ChangedCount: 1,
	}, f.defaultErr
}

func (f *fakeModelAccessRepository) SetUserModelAccess(_ context.Context, params store.SetUserModelAccessParams) (store.ModelAccessChangeResult, error) {
	f.userCalls++
	f.userParams = params
	return store.ModelAccessChangeResult{
		Model: params.Model, Enabled: params.Enabled, Scope: params.Scope,
		TargetCount: int64(len(params.UserIDs)), ChangedCount: int64(len(params.UserIDs)),
	}, f.usersErr
}

func newModelAccessHandlerServer(repository modelAccessRepository) *Server {
	return &Server{
		config: config.Config{UsagePricing: config.UsagePricing{Models: map[string]config.ModelPricing{
			"gpt-6-astra":                  {},
			config.InternalGovernanceModel: {},
		}}},
		modelAccessRepo: repository,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestModelAccessModelsFailsClosedForMissingOrDuplicateConfiguredRows(t *testing.T) {
	for _, test := range []struct {
		name   string
		models []store.ModelAccessModel
	}{
		{name: "missing", models: nil},
		{name: "duplicate", models: []store.ModelAccessModel{{Model: "gpt-6-astra"}, {Model: "gpt-6-astra"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newModelAccessHandlerServer(&fakeModelAccessRepository{models: test.models})
			response := httptest.NewRecorder()
			server.modelAccessModels(response, httptest.NewRequest(http.MethodGet, "/admin/model-access/models", nil))
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "internal_error") {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func modelAccessRequest(method, path, model, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.SetPathValue("model", model)
	ctx := context.WithValue(request.Context(), userContextKey, store.User{ID: "owner-1", Role: store.UserRoleOwner})
	ctx = context.WithValue(ctx, sessionContextKey, store.Session{ID: "session-1", UserID: "owner-1"})
	return request.WithContext(ctx)
}

func TestModelAccessModelsFiltersNonCatalogAndInternalModels(t *testing.T) {
	repository := &fakeModelAccessRepository{models: []store.ModelAccessModel{
		{Model: "gpt-6-astra", DefaultEnabled: true, EnabledUserCount: 2},
		{Model: config.InternalGovernanceModel, DefaultEnabled: true, EnabledUserCount: 2},
		{Model: "removed-model", DefaultEnabled: true, EnabledUserCount: 2},
	}}
	server := newModelAccessHandlerServer(repository)
	response := httptest.NewRecorder()
	server.modelAccessModels(response, httptest.NewRequest(http.MethodGet, "/admin/model-access/models", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var document struct {
		Models []store.ModelAccessModel `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Models) != 1 || document.Models[0].Model != "gpt-6-astra" {
		t.Fatalf("models = %#v", document.Models)
	}
}

func TestUpdateModelAccessDefaultPassesAuditAttribution(t *testing.T) {
	repository := &fakeModelAccessRepository{}
	server := newModelAccessHandlerServer(repository)
	response := httptest.NewRecorder()
	request := modelAccessRequest(
		http.MethodPut, "/admin/model-access/models/gpt-6-astra/default", "gpt-6-astra",
		`{"enabled":false,"reason":"  incident containment  "}`,
	)
	server.updateModelAccessDefault(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	params := repository.defaultParams
	if repository.defaultCalls != 1 || params.Model != "gpt-6-astra" || params.Enabled ||
		params.Reason != "incident containment" || params.ActorUserID != "owner-1" || params.ActorSessionID != "session-1" || params.At.IsZero() {
		t.Fatalf("params = %#v calls = %d", params, repository.defaultCalls)
	}
	if !strings.Contains(response.Body.String(), `"scope":"default"`) || !strings.Contains(response.Body.String(), `"changed_count":1`) {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestUpdateSelectedUserModelAccessPassesExactTargets(t *testing.T) {
	repository := &fakeModelAccessRepository{}
	server := newModelAccessHandlerServer(repository)
	response := httptest.NewRecorder()
	request := modelAccessRequest(
		http.MethodPut, "/admin/model-access/models/gpt-6-astra/users", "gpt-6-astra",
		`{"enabled":true,"scope":"selected","user_ids":["user-1","user-2"],"reason":"restore access"}`,
	)
	server.updateUserModelAccess(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	params := repository.userParams
	if repository.userCalls != 1 || params.Model != "gpt-6-astra" || !params.Enabled ||
		params.Scope != store.ModelAccessScopeSelected || params.Reason != "restore access" ||
		len(params.UserIDs) != 2 || params.UserIDs[0] != "user-1" || params.UserIDs[1] != "user-2" {
		t.Fatalf("params = %#v calls = %d", params, repository.userCalls)
	}
}

func TestUpdateUserModelAccessRejectsInvalidBatchInputs(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing enabled", body: `{"scope":"all","reason":"reason"}`},
		{name: "blank reason", body: `{"enabled":true,"scope":"all","reason":"   "}`},
		{name: "all carries ids", body: `{"enabled":true,"scope":"all","user_ids":[],"reason":"reason"}`},
		{name: "selected omits ids", body: `{"enabled":true,"scope":"selected","reason":"reason"}`},
		{name: "selected null ids", body: `{"enabled":true,"scope":"selected","user_ids":null,"reason":"reason"}`},
		{name: "selected duplicate ids", body: `{"enabled":true,"scope":"selected","user_ids":["user-1","user-1"],"reason":"reason"}`},
		{name: "unknown scope", body: `{"enabled":true,"scope":"current","reason":"reason"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeModelAccessRepository{}
			server := newModelAccessHandlerServer(repository)
			response := httptest.NewRecorder()
			request := modelAccessRequest(http.MethodPut, "/admin/model-access/models/gpt-6-astra/users", "gpt-6-astra", test.body)
			server.updateUserModelAccess(response, request)

			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_model_access_operation") {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if repository.userCalls != 0 {
				t.Fatalf("repository calls = %d", repository.userCalls)
			}
		})
	}
}

func TestModelAccessHandlersRejectUnknownCatalogModel(t *testing.T) {
	repository := &fakeModelAccessRepository{}
	server := newModelAccessHandlerServer(repository)
	response := httptest.NewRecorder()
	request := modelAccessRequest(
		http.MethodPut, "/admin/model-access/models/removed-model/default", "removed-model",
		`{"enabled":true,"reason":"reason"}`,
	)
	server.updateModelAccessDefault(response, request)

	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "model_access_model_not_found") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if repository.defaultCalls != 0 {
		t.Fatalf("repository calls = %d", repository.defaultCalls)
	}
}

func TestModelAccessStoreErrorsPreserveClientAndFailClosedSemantics(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid target", err: store.ErrInvalid, wantStatus: http.StatusBadRequest, wantCode: "invalid_model_access_operation"},
		{name: "removed model", err: store.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "model_access_model_not_found"},
		{name: "missing permission state", err: store.ErrModelAccessUnavailable, wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeModelAccessRepository{usersErr: test.err}
			server := newModelAccessHandlerServer(repository)
			response := httptest.NewRecorder()
			request := modelAccessRequest(
				http.MethodPut, "/admin/model-access/models/gpt-6-astra/users", "gpt-6-astra",
				`{"enabled":false,"scope":"all","reason":"reason"}`,
			)
			server.updateUserModelAccess(response, request)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantCode) {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestModelAccessWriteRoutesRequireSameOriginBrowserRequest(t *testing.T) {
	server := &Server{
		config: config.Config{RPOrigins: []string{"https://gateway.example"}},
		mux:    http.NewServeMux(),
	}
	server.routes()

	for _, path := range []string{
		"/admin/model-access/models/gpt-6-astra/default",
		"/admin/model-access/models/gpt-6-astra/users",
	} {
		request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"enabled":true,"reason":"reason"}`))
		response := httptest.NewRecorder()
		server.mux.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "invalid_origin") {
			t.Fatalf("path = %s, status = %d, body = %s", path, response.Code, response.Body.String())
		}

		request = httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"enabled":true,"reason":"reason"}`))
		request.Header.Set("Origin", "https://gateway.example")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		response = httptest.NewRecorder()
		server.mux.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "session_required") {
			t.Fatalf("same-origin path = %s, status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}

func TestModelAccessOwnerOnlyProtection(t *testing.T) {
	server := &Server{}
	called := false
	handler := server.ownerOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/admin/model-access/models", nil)
	request = request.WithContext(context.WithValue(
		request.Context(), userContextKey, store.User{ID: "member-1", Role: store.UserRoleMember},
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || called || !strings.Contains(response.Body.String(), "owner_required") {
		t.Fatalf("status = %d called = %t body = %s", response.Code, called, response.Body.String())
	}
}
