package server

import (
	"encoding/csv"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type usageSummary struct {
	Requests          int64   `json:"requests"`
	Tokens            int64   `json:"tokens"`
	InputTokens       int64   `json:"input_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	CacheRate         float64 `json:"cache_rate"`
	ErrorRate         float64 `json:"error_rate"`
	P95TTFTMillis     int64   `json:"p95_ttft_ms"`
	P95DurationMillis int64   `json:"p95_duration_ms"`
}

func (s *Server) usageJSON(w http.ResponseWriter, r *http.Request) {
	filter, err := s.usageFilter(r)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_usage_filter", "统计筛选条件无效")
		return
	}
	requests, err := s.store.ListUsageRequests(r.Context(), filter)
	if err != nil {
		internalError(s, w, r, "list usage", err)
		return
	}
	exact, err := s.store.SummarizeUsageRequests(r.Context(), filter)
	if err != nil {
		internalError(s, w, r, "summarize usage", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": summaryFromStore(exact), "requests": requests})
}

func (s *Server) usageCSV(w http.ResponseWriter, r *http.Request) {
	filter, err := s.usageFilter(r)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_usage_filter", "统计筛选条件无效")
		return
	}
	filter.Limit = 500
	requests, err := s.store.ListUsageRequests(r.Context(), filter)
	if err != nil {
		internalError(s, w, r, "export usage", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-gateway-usage.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{
		"request_id", "user_id", "device_id", "key_prefix", "project_id", "model", "endpoint",
		"state", "http_status", "error_code", "requested_at", "ttft_ms", "duration_ms",
		"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens",
		"request_bytes", "response_bytes", "upstream_request_id",
	})
	for _, request := range requests {
		_ = writer.Write([]string{
			request.RequestID, request.UserID, request.DeviceID, request.KeyPrefix, stringPointer(request.ProjectID),
			request.Model, request.Endpoint, request.State, intPointer(request.HTTPStatus), stringPointer(request.ErrorCode),
			request.RequestedAt.Format(time.RFC3339Nano), int64Pointer(request.TTFTMillis), int64Pointer(request.DurationMillis),
			strconv.FormatInt(request.InputTokens, 10), strconv.FormatInt(request.CachedInputTokens, 10),
			strconv.FormatInt(request.OutputTokens, 10), strconv.FormatInt(request.ReasoningTokens, 10),
			strconv.FormatInt(request.RequestBytes, 10), strconv.FormatInt(request.ResponseBytes, 10),
			stringPointer(request.UpstreamRequestID),
		})
	}
	writer.Flush()
}

func (s *Server) usageFilter(r *http.Request) (store.UsageFilter, error) {
	query := r.URL.Query()
	until := time.Now().UTC().Add(time.Second)
	from := until.Add(-7 * 24 * time.Hour)
	var err error
	if value := query.Get("from"); value != "" {
		from, err = parseDashboardTime(value, false)
		if err != nil {
			return store.UsageFilter{}, err
		}
	}
	if value := query.Get("until"); value != "" {
		until, err = parseDashboardTime(value, true)
		if err != nil {
			return store.UsageFilter{}, err
		}
	}
	if !from.Before(until) || until.Sub(from) > 366*24*time.Hour {
		return store.UsageFilter{}, errors.New("usage interval is invalid")
	}
	user := userFrom(r.Context())
	userID := user.ID
	if user.Role == store.UserRoleOwner && query.Get("user_id") != "" {
		userID = query.Get("user_id")
	}
	filter := store.UsageFilter{
		From: &from, Until: &until, UserID: userID,
		DeviceID: query.Get("device_id"), APIKeyID: query.Get("api_key_id"),
		ProjectID: query.Get("project_id"), Model: query.Get("model"), State: query.Get("state"),
		Limit: 500,
	}
	if status := query.Get("status"); status != "" {
		if len(status) == 3 && status[1:] == "xx" && status[0] >= '1' && status[0] <= '5' {
			filter.StatusClass = int(status[0] - '0')
		} else {
			value, err := strconv.Atoi(status)
			if err != nil || value < 100 || value > 599 {
				return store.UsageFilter{}, errors.New("invalid HTTP status filter")
			}
			filter.HTTPStatus = value
		}
	}
	return filter, nil
}

func parseDashboardTime(value string, endOfDay bool) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		parsed = parsed.Add(24 * time.Hour)
	}
	return parsed.UTC(), nil
}

func summarizeUsage(requests []store.UsageRequest) usageSummary {
	var summary usageSummary
	var errorsCount int64
	var ttft, duration []int64
	for _, request := range requests {
		summary.Requests++
		summary.InputTokens += request.InputTokens
		summary.CachedInputTokens += request.CachedInputTokens
		summary.OutputTokens += request.OutputTokens
		summary.ReasoningTokens += request.ReasoningTokens
		if request.State != "completed" || (request.HTTPStatus != nil && *request.HTTPStatus >= 400) {
			errorsCount++
		}
		if request.TTFTMillis != nil {
			ttft = append(ttft, *request.TTFTMillis)
		}
		if request.DurationMillis != nil {
			duration = append(duration, *request.DurationMillis)
		}
	}
	summary.Tokens = summary.InputTokens + summary.OutputTokens
	if summary.InputTokens > 0 {
		summary.CacheRate = float64(summary.CachedInputTokens) / float64(summary.InputTokens)
	}
	if summary.Requests > 0 {
		summary.ErrorRate = float64(errorsCount) / float64(summary.Requests)
	}
	summary.P95TTFTMillis = percentile95(ttft)
	summary.P95DurationMillis = percentile95(duration)
	return summary
}

func summaryFromStore(value store.UsageSummary) usageSummary {
	summary := usageSummary{
		Requests: value.RequestCount, InputTokens: value.InputTokens,
		CachedInputTokens: value.CachedInputTokens, OutputTokens: value.OutputTokens,
		ReasoningTokens: value.ReasoningTokens,
		P95TTFTMillis:   value.P95TTFTMillis, P95DurationMillis: value.P95DurationMillis,
	}
	summary.Tokens = summary.InputTokens + summary.OutputTokens
	if summary.InputTokens > 0 {
		summary.CacheRate = float64(summary.CachedInputTokens) / float64(summary.InputTokens)
	}
	if summary.Requests > 0 {
		summary.ErrorRate = float64(value.ErrorCount) / float64(summary.Requests)
	}
	return summary
}

func percentile95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	index := (95*len(values) + 99) / 100
	if index < 1 {
		index = 1
	}
	return values[index-1]
}

func stringPointer(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func intPointer(value *int) string {
	if value == nil {
		return ""
	}
	return strconv.Itoa(*value)
}

func int64Pointer(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}
