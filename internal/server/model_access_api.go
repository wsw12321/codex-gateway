package server

import (
	"context"
	"net/http"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

func requiresUserModelAccess(method, model string, pricing config.UsagePricing) bool {
	return method == http.MethodPost && pricing.IsManageableModel(model)
}

func (s *Server) allowedModelsForAPIKey(ctx context.Context, key store.APIKey) (map[string]struct{}, error) {
	repository := s.modelAccessStorage()
	if repository == nil {
		return nil, store.ErrModelAccessUnavailable
	}
	models, err := repository.ListModelAccessModels(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.configuredModelAccessModels(models); err != nil {
		return nil, err
	}
	enabled, err := repository.ListEnabledModelsForUser(ctx, key.UserID)
	if err != nil {
		return nil, err
	}
	return intersectAllowedModels(s.config.UsagePricing, enabled, key.ModelAllowlist), nil
}

func intersectAllowedModels(pricing config.UsagePricing, userEnabled, keyAllowlist []string) map[string]struct{} {
	candidates := make(map[string]struct{}, len(userEnabled)+1)
	for _, model := range userEnabled {
		if pricing.IsManageableModel(model) {
			candidates[model] = struct{}{}
		}
	}
	if _, exists := pricing.Models[config.InternalGovernanceModel]; exists {
		candidates[config.InternalGovernanceModel] = struct{}{}
	}
	if len(keyAllowlist) == 0 {
		return candidates
	}
	allowed := make(map[string]struct{}, len(keyAllowlist))
	for _, model := range keyAllowlist {
		if _, exists := candidates[model]; exists {
			allowed[model] = struct{}{}
		}
	}
	return allowed
}
