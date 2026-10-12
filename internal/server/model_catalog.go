package server

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type modelCatalogRepository interface {
	ListUsageModels(context.Context, string) ([]string, error)
}

type sessionModelOption struct {
	ID    string `json:"id"`
	Group string `json:"group"`
}

// Group names are emitted by the catalog boundary, so every selector uses the
// same provider families, including retained historical and account models.
func modelCatalogOptions(models []string) []sessionModelOption {
	seen := make(map[string]bool, len(models))
	options := make([]sessionModelOption, 0, len(models))
	for _, model := range models {
		if seen[model] || model == "" || model == config.InternalGovernanceModel {
			continue
		}
		seen[model] = true
		group := "GPT"
		switch {
		case config.IsAntigravityModel(model), config.IsLegacyAntigravityModel(model), strings.HasPrefix(model, "gemini-"):
			group = "Gemini"
		case strings.HasPrefix(model, "claude-"):
			group = "Claude"
		}
		options = append(options, sessionModelOption{ID: model, Group: group})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	return options
}

func (s *Server) sessionModelCatalog(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	scope := "api_key"
	if values, exists := query["scope"]; exists {
		if len(values) != 1 {
			s.modelCatalogInputError(w, r)
			return
		}
		scope = values[0]
	}
	if err != nil || len(query) > 1 || (len(query) == 1 && query["scope"] == nil) {
		s.modelCatalogInputError(w, r)
		return
	}
	owner := userFrom(r.Context()).Role == store.UserRoleOwner
	switch scope {
	case "manageable", "global_usage":
		if !owner {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "owner_required", "仅 Owner 可查询此模型目录")
			return
		}
	case "api_key", "usage":
	default:
		s.modelCatalogInputError(w, r)
		return
	}
	models := s.config.UsagePricing.ManageableModelNames()
	if scope == "api_key" {
		allowed, err := s.allowedModelsForAPIKey(r.Context(), store.APIKey{UserID: userFrom(r.Context()).ID})
		if err != nil {
			internalError(s, w, r, "list session model catalog", err)
			return
		}
		models = make([]string, 0, len(allowed))
		for model := range allowed {
			models = append(models, model)
		}
	}
	if scope == "usage" || scope == "global_usage" {
		repository := s.modelCatalogRepo
		if repository == nil {
			repository = s.store
		}
		userID := userFrom(r.Context()).ID
		if scope == "global_usage" {
			userID = ""
		}
		history, err := repository.ListUsageModels(r.Context(), userID)
		if err != nil {
			internalError(s, w, r, "list historical model catalog", err)
			return
		}
		models = append(models, history...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": modelCatalogOptions(models)})
}

func (s *Server) modelCatalogInputError(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_catalog_scope", "模型目录范围无效")
}
