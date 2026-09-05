package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	modelAccessDefaultRequestBytes = 16 << 10
	modelAccessUsersRequestBytes   = 512 << 10
)

type modelAccessRepository interface {
	ListModelAccessModels(context.Context) ([]store.ModelAccessModel, error)
	ListModelAccessUsers(context.Context, string) ([]store.ModelAccessUser, error)
	ListEnabledModelsForUser(context.Context, string) ([]string, error)
	SetModelAccessDefault(context.Context, store.SetModelAccessDefaultParams) (store.ModelAccessChangeResult, error)
	SetUserModelAccess(context.Context, store.SetUserModelAccessParams) (store.ModelAccessChangeResult, error)
}

type updateModelAccessDefaultInput struct {
	Enabled *bool  `json:"enabled"`
	Reason  string `json:"reason"`
}

type updateUserModelAccessInput struct {
	Enabled *bool           `json:"enabled"`
	Scope   string          `json:"scope"`
	UserIDs json.RawMessage `json:"user_ids"`
	Reason  string          `json:"reason"`
}

func (s *Server) modelAccessStorage() modelAccessRepository {
	if s.modelAccessRepo != nil {
		return s.modelAccessRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

func (s *Server) configuredModelAccessModels(models []store.ModelAccessModel) ([]store.ModelAccessModel, error) {
	configured := s.config.UsagePricing.ManageableModelNames()
	byModel := make(map[string]store.ModelAccessModel, len(configured))
	for _, model := range models {
		if !s.config.UsagePricing.IsManageableModel(model.Model) {
			continue
		}
		if _, duplicate := byModel[model.Model]; duplicate {
			return nil, errors.New("model access catalog contains a duplicate configured model")
		}
		byModel[model.Model] = model
	}
	visible := make([]store.ModelAccessModel, 0, len(configured))
	for _, model := range configured {
		value, exists := byModel[model]
		if !exists {
			return nil, errors.New("model access catalog is missing a configured model")
		}
		visible = append(visible, value)
	}
	return visible, nil
}

func (s *Server) modelAccessModels(w http.ResponseWriter, r *http.Request) {
	repository := s.modelAccessStorage()
	if repository == nil {
		internalError(s, w, r, "list model access models", errors.New("model access repository is unavailable"))
		return
	}
	models, err := repository.ListModelAccessModels(r.Context())
	if err != nil {
		internalError(s, w, r, "list model access models", err)
		return
	}
	visible, err := s.configuredModelAccessModels(models)
	if err != nil {
		internalError(s, w, r, "list model access models", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": visible})
}

func (s *Server) modelAccessUsers(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if !s.config.UsagePricing.IsManageableModel(model) {
		s.modelAccessModelNotFound(w, r)
		return
	}
	repository := s.modelAccessStorage()
	if repository == nil {
		internalError(s, w, r, "list model access users", errors.New("model access repository is unavailable"))
		return
	}
	users, err := repository.ListModelAccessUsers(r.Context(), model)
	if err != nil {
		s.modelAccessStoreError(w, r, "list model access users", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "users": users})
}

func (s *Server) updateModelAccessDefault(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if !s.config.UsagePricing.IsManageableModel(model) {
		s.modelAccessModelNotFound(w, r)
		return
	}
	var input updateModelAccessDefaultInput
	if err := decodeJSON(w, r, &input, modelAccessDefaultRequestBytes); err != nil {
		badJSON(w, r, err)
		return
	}
	reason, ok := validModelAccessReason(input.Reason)
	if input.Enabled == nil || !ok {
		s.modelAccessInputError(w, r)
		return
	}
	repository := s.modelAccessStorage()
	if repository == nil {
		internalError(s, w, r, "update model access default", errors.New("model access repository is unavailable"))
		return
	}
	result, err := repository.SetModelAccessDefault(r.Context(), store.SetModelAccessDefaultParams{
		ModelAccessWriteParams: s.modelAccessWriteParams(r, reason),
		Model:                  model,
		Enabled:                *input.Enabled,
	})
	if err != nil {
		s.modelAccessStoreError(w, r, "update model access default", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) updateUserModelAccess(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if !s.config.UsagePricing.IsManageableModel(model) {
		s.modelAccessModelNotFound(w, r)
		return
	}
	var input updateUserModelAccessInput
	if err := decodeJSON(w, r, &input, modelAccessUsersRequestBytes); err != nil {
		badJSON(w, r, err)
		return
	}
	reason, ok := validModelAccessReason(input.Reason)
	if input.Enabled == nil || !ok {
		s.modelAccessInputError(w, r)
		return
	}
	userIDs, ok := parseModelAccessUserIDs(input.Scope, input.UserIDs)
	if !ok {
		s.modelAccessInputError(w, r)
		return
	}
	repository := s.modelAccessStorage()
	if repository == nil {
		internalError(s, w, r, "update user model access", errors.New("model access repository is unavailable"))
		return
	}
	result, err := repository.SetUserModelAccess(r.Context(), store.SetUserModelAccessParams{
		ModelAccessWriteParams: s.modelAccessWriteParams(r, reason),
		Model:                  model,
		Enabled:                *input.Enabled,
		Scope:                  input.Scope,
		UserIDs:                userIDs,
	})
	if err != nil {
		s.modelAccessStoreError(w, r, "update user model access", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func validModelAccessReason(value string) (string, bool) {
	value = strings.TrimSpace(value)
	return value, value != "" && utf8.RuneCountInString(value) <= 500
}

func parseModelAccessUserIDs(scope string, raw json.RawMessage) ([]string, bool) {
	switch scope {
	case store.ModelAccessScopeAll:
		return nil, len(raw) == 0
	case store.ModelAccessScopeSelected:
		if len(raw) == 0 {
			return nil, false
		}
		var userIDs []string
		if err := json.Unmarshal(raw, &userIDs); err != nil || len(userIDs) == 0 || len(userIDs) > 5000 {
			return nil, false
		}
		seen := make(map[string]struct{}, len(userIDs))
		for _, userID := range userIDs {
			if userID == "" || userID != strings.TrimSpace(userID) {
				return nil, false
			}
			if _, exists := seen[userID]; exists {
				return nil, false
			}
			seen[userID] = struct{}{}
		}
		return userIDs, true
	default:
		return nil, false
	}
}

func (s *Server) modelAccessWriteParams(r *http.Request, reason string) store.ModelAccessWriteParams {
	return store.ModelAccessWriteParams{
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()), Reason: reason,
		At: time.Now().UTC(),
	}
}

func (s *Server) modelAccessInputError(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_access_operation", "模型权限操作参数无效")
}

func (s *Server) modelAccessModelNotFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "model_access_model_not_found", "模型不在当前可管理目录中")
}

func (s *Server) modelAccessStoreError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, store.ErrInvalid):
		s.modelAccessInputError(w, r)
	case errors.Is(err, store.ErrNotFound):
		s.modelAccessModelNotFound(w, r)
	default:
		internalError(s, w, r, operation, err)
	}
}
