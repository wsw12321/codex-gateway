package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/wsw/codex-gateway/internal/store"
)

type modelAccessBatchRepository interface {
	ListModelAccessUsersBatch(context.Context, []string) ([]store.ModelAccessUser, error)
	SetModelAccessDefaultsBatch(context.Context, store.SetModelAccessDefaultsBatchParams) (store.ModelAccessBatchResult, error)
	SetUserModelAccessBatch(context.Context, store.SetUserModelAccessBatchParams) (store.ModelAccessBatchResult, error)
}

type updateModelAccessDefaultsBatchInput struct {
	Models []string `json:"models"`
	updateModelAccessDefaultInput
}

type updateUserModelAccessBatchInput struct {
	Models []string `json:"models"`
	updateUserModelAccessInput
}

func (s *Server) modelAccessBatchStorage(w http.ResponseWriter, r *http.Request) modelAccessBatchRepository {
	repository, ok := s.modelAccessStorage().(modelAccessBatchRepository)
	if !ok {
		internalError(s, w, r, "model access batch", errors.New("model access batch repository is unavailable"))
		return nil
	}
	return repository
}

func (s *Server) validateModelAccessBatch(w http.ResponseWriter, r *http.Request, models []string) bool {
	if len(models) == 0 || len(models) > 1000 {
		s.modelAccessInputError(w, r)
		return false
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == "" || model != strings.TrimSpace(model) {
			s.modelAccessInputError(w, r)
			return false
		}
		if _, duplicate := seen[model]; duplicate {
			s.modelAccessInputError(w, r)
			return false
		}
		seen[model] = struct{}{}
		if !s.config.UsagePricing.IsManageableModel(model) {
			s.modelAccessModelNotFound(w, r)
			return false
		}
	}
	return true
}

func (s *Server) modelAccessUsersBatch(w http.ResponseWriter, r *http.Request) {
	models := r.URL.Query()["models"]
	if !s.validateModelAccessBatch(w, r, models) {
		return
	}
	repository := s.modelAccessBatchStorage(w, r)
	if repository == nil {
		return
	}
	users, err := repository.ListModelAccessUsersBatch(r.Context(), models)
	if err != nil {
		s.modelAccessStoreError(w, r, "list model access batch users", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "users": users})
}

func (s *Server) updateModelAccessDefaultsBatch(w http.ResponseWriter, r *http.Request) {
	var input updateModelAccessDefaultsBatchInput
	if err := decodeJSON(w, r, &input, modelAccessUsersRequestBytes); err != nil {
		badJSON(w, r, err)
		return
	}
	if !s.validateModelAccessBatch(w, r, input.Models) {
		return
	}
	reason, ok := validModelAccessReason(input.Reason)
	if input.Enabled == nil || !ok {
		s.modelAccessInputError(w, r)
		return
	}
	repository := s.modelAccessBatchStorage(w, r)
	if repository == nil {
		return
	}
	result, err := repository.SetModelAccessDefaultsBatch(r.Context(), store.SetModelAccessDefaultsBatchParams{
		ModelAccessWriteParams: s.modelAccessWriteParams(r, reason), Models: input.Models, Enabled: *input.Enabled,
	})
	if err != nil {
		s.modelAccessStoreError(w, r, "update model access batch defaults", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) updateUserModelAccessBatch(w http.ResponseWriter, r *http.Request) {
	var input updateUserModelAccessBatchInput
	if err := decodeJSON(w, r, &input, modelAccessUsersRequestBytes); err != nil {
		badJSON(w, r, err)
		return
	}
	if !s.validateModelAccessBatch(w, r, input.Models) {
		return
	}
	reason, ok := validModelAccessReason(input.Reason)
	userIDs, usersOK := parseModelAccessUserIDs(input.Scope, input.UserIDs)
	if input.Enabled == nil || !ok || !usersOK {
		s.modelAccessInputError(w, r)
		return
	}
	repository := s.modelAccessBatchStorage(w, r)
	if repository == nil {
		return
	}
	result, err := repository.SetUserModelAccessBatch(r.Context(), store.SetUserModelAccessBatchParams{
		ModelAccessWriteParams: s.modelAccessWriteParams(r, reason), Models: input.Models,
		Enabled: *input.Enabled, Scope: input.Scope, UserIDs: userIDs,
	})
	if err != nil {
		s.modelAccessStoreError(w, r, "update model access batch users", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
