package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type modelPriceRepository interface {
	ListModelPrices(context.Context, config.UsagePricing) ([]store.ModelPrice, error)
	SetModelPrice(context.Context, store.SetModelPriceParams) (store.ModelPrice, error)
}

type modelPriceInput struct {
	billingOperationInput
	Action      string               `json:"action"`
	Version     *int64               `json:"version"`
	StructureID string               `json:"structure_id"`
	Price       *config.ModelPricing `json:"price,omitempty"`
}

func (s *Server) modelPriceStorage() modelPriceRepository {
	if s.modelPriceRepo != nil {
		return s.modelPriceRepo
	}
	return s.store
}

func (s *Server) billingModelPrices(w http.ResponseWriter, r *http.Request) {
	values, err := s.modelPriceStorage().ListModelPrices(r.Context(), s.config.UsagePricing)
	if err != nil {
		s.billingStoreError(w, r, "list model prices", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": values})
}

func (s *Server) updateModelPrice(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if model == config.InternalGovernanceModel {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "model_price_not_editable", "模型未配置或为固定零价，无法修改")
		return
	}
	var input modelPriceInput
	if !s.decodeBillingWrite(w, r, &input) {
		return
	}
	if input.Version == nil || *input.Version < 0 || input.StructureID == "" ||
		(input.Action != "save" && input.Action != "restore") ||
		(input.Action == "save" && input.Price == nil) ||
		(input.Action == "restore" && input.Price != nil) {
		s.billingInputError(w, r)
		return
	}
	// A completed operation must still replay its first result after deployment
	// changes or removes the model. Storage checks idempotency before rejecting
	// new writes to an absent model or a stale structure.
	if _, configured := s.config.UsagePricing.Models[model]; configured {
		structureID, err := s.config.UsagePricing.ModelPriceStructureID(model)
		if err != nil {
			internalError(s, w, r, "read configured model price structure", err)
			return
		}
		if input.Action == "save" && input.StructureID == structureID {
			price, err := s.config.UsagePricing.ValidateModelPriceOverride(model, *input.Price)
			if err != nil {
				s.billingInputError(w, r)
				return
			}
			input.Price = &price
		}
	}
	value, err := s.modelPriceStorage().SetModelPrice(r.Context(), store.SetModelPriceParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, input.Reason),
		Pricing:            s.config.UsagePricing, Model: model, Action: input.Action,
		Version: *input.Version, StructureID: input.StructureID, Price: input.Price,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "model_price_not_editable", "模型未配置或为固定零价，无法修改")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "model_price_conflict", "模型价格、配置结构或操作 ID 已变更，请重新加载后确认")
			return
		}
		s.billingStoreError(w, r, "set model price", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
