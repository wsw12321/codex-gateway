package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

type fakeModelAccessBatchRepository struct {
	fakeModelAccessRepository
	queried      []string
	defaultBatch store.SetModelAccessDefaultsBatchParams
	userBatch    store.SetUserModelAccessBatchParams
	batchCalls   int
}

func (f *fakeModelAccessBatchRepository) ListModelAccessUsersBatch(_ context.Context, models []string) ([]store.ModelAccessUser, error) {
	f.queried = models
	return f.users, f.listUsersErr
}

func (f *fakeModelAccessBatchRepository) SetModelAccessDefaultsBatch(_ context.Context, params store.SetModelAccessDefaultsBatchParams) (store.ModelAccessBatchResult, error) {
	f.batchCalls++
	f.defaultBatch = params
	return store.ModelAccessBatchResult{TargetCount: int64(len(params.Models)), ChangedCount: int64(len(params.Models))}, f.defaultErr
}

func (f *fakeModelAccessBatchRepository) SetUserModelAccessBatch(_ context.Context, params store.SetUserModelAccessBatchParams) (store.ModelAccessBatchResult, error) {
	f.batchCalls++
	f.userBatch = params
	return store.ModelAccessBatchResult{TargetCount: int64(len(params.Models) * len(params.UserIDs)), ChangedCount: int64(len(params.Models) * len(params.UserIDs))}, f.usersErr
}

func modelAccessBatchTestServer(repository *fakeModelAccessBatchRepository) *Server {
	s := newModelAccessHandlerServer(repository)
	s.config.UsagePricing.Models["gpt-6-sol"] = config.ModelPricing{}
	return s
}

func TestModelAccessBatchUsersPassesCartesianSelectionAndAudit(t *testing.T) {
	repository := &fakeModelAccessBatchRepository{}
	s := modelAccessBatchTestServer(repository)
	response := httptest.NewRecorder()
	s.updateUserModelAccessBatch(response, modelAccessRequest(http.MethodPut, "/admin/model-access/users", "",
		`{"models":["gpt-6-astra","gpt-6-sol"],"enabled":false,"scope":"selected","user_ids":["u1","u2"],"reason":"  batch incident  "}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	params := repository.userBatch
	if repository.batchCalls != 1 || !reflect.DeepEqual(params.Models, []string{"gpt-6-astra", "gpt-6-sol"}) ||
		!reflect.DeepEqual(params.UserIDs, []string{"u1", "u2"}) || params.Scope != "selected" || params.Enabled ||
		params.ActorUserID != "owner-1" || params.ActorSessionID != "session-1" || params.Reason != "batch incident" {
		t.Fatalf("unexpected params: %+v", params)
	}
	if !strings.Contains(response.Body.String(), `"target_count":4`) {
		t.Fatalf("response: %s", response.Body.String())
	}
}

func TestModelAccessBatchDefaultsAndAllUsers(t *testing.T) {
	repository := &fakeModelAccessBatchRepository{}
	s := modelAccessBatchTestServer(repository)
	response := httptest.NewRecorder()
	s.updateModelAccessDefaultsBatch(response, modelAccessRequest(http.MethodPut, "/admin/model-access/defaults", "",
		`{"models":["gpt-6-astra","gpt-6-sol"],"enabled":false,"reason":"new users"}`))
	if response.Code != http.StatusOK || len(repository.defaultBatch.Models) != 2 {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	s.updateUserModelAccessBatch(response, modelAccessRequest(http.MethodPut, "/admin/model-access/users", "",
		`{"models":["gpt-6-astra","gpt-6-sol"],"enabled":true,"scope":"all","reason":"existing users"}`))
	if response.Code != http.StatusOK || repository.userBatch.Scope != "all" || len(repository.userBatch.UserIDs) != 0 {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
}

func TestModelAccessBatchRejectsInvalidSelectionBeforeWrite(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"missing models", `{"enabled":true,"scope":"all","reason":"reason"}`, 400},
		{"empty models", `{"models":[],"enabled":true,"scope":"all","reason":"reason"}`, 400},
		{"duplicate models", `{"models":["gpt-6-astra","gpt-6-astra"],"enabled":true,"scope":"all","reason":"reason"}`, 400},
		{"unknown model", `{"models":["gpt-6-astra","unknown"],"enabled":true,"scope":"all","reason":"reason"}`, 404},
		{"internal model", `{"models":["gpt-6-astra","codex-auto-review"],"enabled":true,"scope":"all","reason":"reason"}`, 404},
		{"missing enabled", `{"models":["gpt-6-astra"],"scope":"all","reason":"reason"}`, 400},
		{"missing reason", `{"models":["gpt-6-astra"],"enabled":true,"scope":"all"}`, 400},
		{"all with ids", `{"models":["gpt-6-astra"],"enabled":true,"scope":"all","user_ids":[],"reason":"reason"}`, 400},
		{"selected no ids", `{"models":["gpt-6-astra"],"enabled":true,"scope":"selected","reason":"reason"}`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeModelAccessBatchRepository{}
			s := modelAccessBatchTestServer(repository)
			response := httptest.NewRecorder()
			s.updateUserModelAccessBatch(response, modelAccessRequest(http.MethodPut, "/admin/model-access/users", "", test.body))
			if response.Code != test.status || repository.batchCalls != 0 {
				t.Fatalf("status %d, calls %d: %s", response.Code, repository.batchCalls, response.Body.String())
			}
		})
	}
}

func TestModelAccessBatchQuery(t *testing.T) {
	repository := &fakeModelAccessBatchRepository{fakeModelAccessRepository: fakeModelAccessRepository{users: []store.ModelAccessUser{{Model: "gpt-6-astra", UserID: "u1", Enabled: true}, {Model: "gpt-6-sol", UserID: "u1", Enabled: false}}}}
	s := modelAccessBatchTestServer(repository)
	response := httptest.NewRecorder()
	s.modelAccessUsersBatch(response, httptest.NewRequest(http.MethodGet, "/admin/model-access/users?models=gpt-6-astra&models=gpt-6-sol", nil))
	if response.Code != http.StatusOK || len(repository.queried) != 2 || !strings.Contains(response.Body.String(), `"enabled":false`) {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
}
