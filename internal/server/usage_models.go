package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

// usageModelsDefaultWindow is the period the usage page opens with.
const usageModelsDefaultWindow = 24 * time.Hour

type usageModelRepository interface {
	SummarizeUsageByModel(context.Context, store.UsageFilter) (store.UsageModelBreakdown, error)
}

type usageModelRow struct {
	Model             string `json:"model"`
	Requests          int64  `json:"requests"`
	Errors            int64  `json:"errors"`
	InProgress        int64  `json:"in_progress"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	Tokens            int64  `json:"tokens"`
	ChargedUSD        string `json:"charged_usd"`
}

type usageModelTotal struct {
	Requests   int64  `json:"requests"`
	Tokens     int64  `json:"tokens"`
	ChargedUSD string `json:"charged_usd"`
}

type usageModelsResponse struct {
	From        time.Time       `json:"from"`
	Until       time.Time       `json:"until"`
	GeneratedAt time.Time       `json:"generated_at"`
	Models      []usageModelRow `json:"models"`
	Total       usageModelTotal `json:"total"`
	Truncated   bool            `json:"truncated"`
}

func (s *Server) usageModelStorage() usageModelRepository {
	if s.usageModelRepo != nil {
		return s.usageModelRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

// usageModelsJSON returns per-model request counts, tokens and settled
// charges for the signed-in user (an Owner may pass user_id). It accepts the
// same filters as /admin/usage but defaults to the last 24 hours, so the
// usage page can open on the quota each model consumed without listing every
// request.
func (s *Server) usageModelsJSON(w http.ResponseWriter, r *http.Request) {
	filter, err := s.usageFilter(r)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_usage_filter", "统计筛选条件无效")
		return
	}
	if r.URL.Query().Get("from") == "" {
		from := filter.Until.Add(-usageModelsDefaultWindow)
		filter.From = &from
	}
	repository := s.usageModelStorage()
	if repository == nil {
		internalError(s, w, r, "summarize usage by model", errors.New("usage repository unavailable"))
		return
	}
	breakdown, err := repository.SummarizeUsageByModel(r.Context(), filter)
	if err != nil {
		internalError(s, w, r, "summarize usage by model", err)
		return
	}
	response := usageModelsResponse{
		From: filter.From.UTC(), Until: filter.Until.UTC(), GeneratedAt: time.Now().UTC(),
		Models:    make([]usageModelRow, 0, len(breakdown.Models)),
		Total:     usageModelTotal{Requests: breakdown.TotalRequests, Tokens: breakdown.TotalTokens, ChargedUSD: breakdown.TotalChargedUSD},
		Truncated: breakdown.Truncated,
	}
	for _, model := range breakdown.Models {
		response.Models = append(response.Models, usageModelRow{
			Model: model.Model, Requests: model.RequestCount, Errors: model.ErrorCount, InProgress: model.InProgressCount,
			InputTokens: model.InputTokens, CachedInputTokens: model.CachedInputTokens, OutputTokens: model.OutputTokens,
			Tokens: model.InputTokens + model.OutputTokens, ChargedUSD: model.ChargedUSD,
		})
	}
	writeJSON(w, http.StatusOK, response)
}
