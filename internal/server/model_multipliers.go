package server

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/wsw/codex-gateway/internal/billing"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type modelMultiplierRepository interface {
	ListModelMultipliers(context.Context, []string) ([]store.ModelMultiplier, error)
	SetModelMultiplier(context.Context, store.SetModelMultiplierParams) (store.ModelMultiplier, error)
}

type modelMultiplierInput struct {
	billingOperationInput
	Multiplier string `json:"multiplier"`
}

type modelMultiplierResponse struct {
	store.ModelMultiplier
	Editable bool `json:"editable"`
}

func (s *Server) modelMultiplierStorage() modelMultiplierRepository {
	if s.modelMultiplierRepo != nil {
		return s.modelMultiplierRepo
	}
	return s.store
}

func (s *Server) billingModelMultipliers(w http.ResponseWriter, r *http.Request) {
	models := s.config.UsagePricing.ManageableModelNames()
	// The internal zero-price model is visible as a fixed rule, including
	// catalogs that do not accept governance requests.
	models = append(models, config.InternalGovernanceModel)
	sort.Strings(models)
	values, err := s.modelMultiplierStorage().ListModelMultipliers(r.Context(), models)
	if err != nil {
		s.billingStoreError(w, r, "list model multipliers", err)
		return
	}
	byModel := make(map[string]store.ModelMultiplier, len(values))
	for _, value := range values {
		byModel[value.Model] = value
	}
	items := make([]modelMultiplierResponse, 0, len(models))
	for _, model := range models {
		value, exists := byModel[model]
		if !exists {
			internalError(s, w, r, "list model multipliers", errors.New("model multiplier catalog is incomplete"))
			return
		}
		editable := model != config.InternalGovernanceModel
		if !editable {
			value.Multiplier, value.UpdatedAt = "1", nil
		}
		items = append(items, modelMultiplierResponse{ModelMultiplier: value, Editable: editable})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": items})
}

func (s *Server) updateModelMultiplier(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	// Match the configured pricing key exactly, as admission does. Gemini's
	// native customtools alias is already normalized before that boundary.
	if !s.config.UsagePricing.IsManageableModel(model) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "model_multiplier_not_editable", "模型未配置或为固定倍率，无法修改")
		return
	}
	var input modelMultiplierInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	multiplier, err := billing.ParseMultiplier(input.Multiplier)
	if err != nil {
		s.billingInputError(w, r)
		return
	}
	value, err := s.modelMultiplierStorage().SetModelMultiplier(r.Context(), store.SetModelMultiplierParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		Model:              model, Multiplier: multiplier,
	})
	if err != nil {
		s.billingStoreError(w, r, "set model multiplier", err)
		return
	}
	writeJSON(w, http.StatusOK, modelMultiplierResponse{ModelMultiplier: value, Editable: true})
}
