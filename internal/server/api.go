package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func (s *Server) proxyModels(w http.ResponseWriter, r *http.Request) {
	s.proxyCodex(w, r, "/v1/models", "models", "catalog")
}

func (s *Server) proxyResponses(w http.ResponseWriter, r *http.Request) {
	s.proxyCodex(w, r, "/v1/responses", "responses", "")
}

func (s *Server) proxyCompact(w http.ResponseWriter, r *http.Request) {
	s.proxyCodex(w, r, "/v1/responses/compact", "responses.compact", "")
}

func (s *Server) proxyCodex(w http.ResponseWriter, r *http.Request, upstreamPath, endpoint, fixedModel string) {
	key := apiKeyFrom(r.Context())
	requestedAt := time.Now().UTC()
	model := fixedModel
	var body *countingBody
	if r.Method == http.MethodPost {
		var err error
		model, body, err = prepareModelBody(w, r, s.config.BodyLimit)
		if err != nil {
			var maxBytes *http.MaxBytesError
			if errors.As(err, &maxBytes) {
				httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "请求体超过 64 MiB 限制")
			} else {
				httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "model_required", "请求必须在 JSON 顶层前部包含有效 model")
			}
			return
		}
		if !modelAllowed(model, key.ModelAllowlist) {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "model_not_allowed", "此 API Key 不允许使用该模型")
			return
		}
	}

	project, err := s.resolveAPIProject(r, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_project", "X-Codex-Project 不是当前账号的有效项目")
			return
		}
		internalError(s, w, r, "resolve API project", err)
		return
	}

	requestID := httpx.RequestID(r.Context())
	quota, err := s.store.ReserveQuota(r.Context(), store.ReserveQuotaParams{
		RequestID: requestID, UserID: key.UserID, APIKeyID: key.ID,
		Day: requestedAt, Now: requestedAt, ReservedTokens: 0, LeaseTTL: 5 * time.Minute,
		Limits: s.quotaLimits(key),
	})
	if err != nil {
		var exceeded *store.QuotaExceededError
		if errors.As(err, &exceeded) {
			retry := int(math.Ceil(exceeded.RetryAfter.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(max(retry, 1)))
			code := "quota_" + exceeded.Dimension + "_exceeded"
			s.createQuotaExceededAlert(r.Context(), key, requestID, requestedAt, exceeded)
			httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", code, "请求已达到网关限额")
			return
		}
		internalError(s, w, r, "reserve quota", err)
		return
	}
	quotaFinished := false
	defer func() {
		if !quotaFinished {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			_ = s.store.ReleaseQuota(ctx, quota.RequestID, time.Now().UTC())
		}
	}()

	projectID := ""
	if project != nil {
		projectID = project.ID
	}
	requestBytes := int64(0)
	if r.ContentLength > 0 {
		requestBytes = r.ContentLength
	}
	_, err = s.store.BeginUsageRequest(r.Context(), store.BeginUsageRequestParams{
		RequestID: requestID, UserID: key.UserID, DeviceID: key.DeviceID,
		APIKeyID: key.ID, ProjectID: projectID, Model: model, Endpoint: endpoint,
		RequestedAt: requestedAt, RequestBytes: requestBytes,
	})
	if err != nil {
		internalError(s, w, r, "begin usage request", err)
		return
	}

	stopRenewal := make(chan struct{})
	go s.renewQuotaLease(requestID, stopRenewal)
	result, failure := s.upstream.Forward(r.Context(), w, r, upstreamPath)
	close(stopRenewal)

	completedAt := time.Now().UTC()
	effectiveStatus := result.StatusCode
	state := "completed"
	errorCode := ""
	if failure != nil {
		state = "failed"
		errorCode = failure.Code
		if failure.Code == "client_disconnected" || errors.Is(r.Context().Err(), context.Canceled) {
			state = "cancelled"
			effectiveStatus = 499
		} else if failure.Status > 0 {
			effectiveStatus = failure.Status
		}
		if failure.Status > 0 && result.FirstByteAt.IsZero() {
			if failure.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(failure.RetryAfter))
			}
			httpx.WriteError(w, r, failure.Status, failure.Type, failure.Code, failure.Message)
		}
	}
	if effectiveStatus < 100 || effectiveStatus > 599 {
		effectiveStatus = http.StatusBadGateway
	}
	actualRequestBytes := requestBytes
	if body != nil {
		actualRequestBytes = body.bytes
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	_, completeErr := s.store.CompleteUsageRequest(writeCtx, store.CompleteUsageRequestParams{
		RequestID: requestID, State: state, HTTPStatus: effectiveStatus,
		ErrorCode: errorCode, FirstTokenAt: timeOrNilValue(result.FirstTokenAt), CompletedAt: completedAt,
		InputTokens: result.Usage.InputTokens, CachedInputTokens: result.Usage.CachedTokens,
		OutputTokens: result.Usage.OutputTokens, ReasoningTokens: result.Usage.ReasoningTokens,
		RequestBytes: actualRequestBytes, ResponseBytes: result.BytesOut, UpstreamRequestID: result.UpstreamRequestID,
		ActualModel: recordedUpstreamModel(result.Model),
	})
	if completeErr != nil {
		s.logger.Error("complete usage metadata", "request_id", requestID, "error_type", "database")
	}
	if err := s.store.SettleQuota(writeCtx, requestID, result.Usage.Total(), completedAt); err != nil {
		s.logger.Error("settle quota", "request_id", requestID, "error_type", "database")
	} else {
		quotaFinished = true
		s.createQuotaThresholdAlerts(writeCtx, key, completedAt)
	}
	_ = s.store.RecordAPIKeyUse(writeCtx, key.ID, key.DeviceID, completedAt)
	if failure != nil {
		s.createUpstreamAlert(writeCtx, key.UserID, requestID, failure)
	}
}

func (s *Server) createQuotaExceededAlert(ctx context.Context, key store.APIKey, requestID string, day time.Time, exceeded *store.QuotaExceededError) {
	scopeID := key.ID
	if exceeded.Scope == "user" {
		scopeID = key.UserID
	} else if exceeded.Scope == "global" {
		scopeID = "global"
	}
	_, _ = s.store.CreateAlert(ctx, store.CreateAlertParams{
		Type: "quota_exceeded", Severity: "warning", UserID: key.UserID, RequestID: requestID,
		DedupeKey: "quota:100:" + day.Format("2006-01-02") + ":" + exceeded.Scope + ":" + scopeID + ":" + exceeded.Dimension,
		Title:     "网关配额已达到 100%", Details: map[string]any{
			"scope": exceeded.Scope, "dimension": exceeded.Dimension,
			"current": exceeded.Current, "limit": exceeded.Limit,
		},
	})
}

func (s *Server) createQuotaThresholdAlerts(ctx context.Context, key store.APIKey, now time.Time) {
	counters, err := s.store.GetQuotaCounters(ctx, now, key.UserID, key.ID)
	if err != nil {
		return
	}
	limits := s.quotaLimits(key)
	for _, counter := range counters {
		requestLimit, tokenLimit := limits.KeyDailyRequests, limits.KeyDailyTokens
		if counter.ScopeType == "user" {
			requestLimit, tokenLimit = limits.UserDailyRequests, limits.UserDailyTokens
		}
		percent := int64(0)
		dimension := "daily_requests"
		current, limitValue := counter.RequestsReserved, requestLimit
		if requestLimit > 0 {
			percent = counter.RequestsReserved * 100 / requestLimit
		}
		if tokenLimit > 0 && counter.TokensUsed*100/tokenLimit > percent {
			percent = counter.TokensUsed * 100 / tokenLimit
			dimension, current, limitValue = "daily_tokens", counter.TokensUsed, tokenLimit
		}
		threshold := int64(0)
		if percent >= 100 {
			threshold = 100
		} else if percent >= 80 {
			threshold = 80
		}
		if threshold == 0 {
			continue
		}
		_, _ = s.store.CreateAlert(ctx, store.CreateAlertParams{
			Type: "quota_threshold", Severity: "warning", UserID: key.UserID,
			DedupeKey: "quota:" + strconv.FormatInt(threshold, 10) + ":" + now.Format("2006-01-02") + ":" + counter.ScopeType + ":" + counter.ScopeID,
			Title:     "网关配额已达到 " + strconv.FormatInt(threshold, 10) + "%",
			Details: map[string]any{
				"scope": counter.ScopeType, "dimension": dimension,
				"current": current, "limit": limitValue,
			},
		})
	}
}

func (s *Server) resolveAPIProject(r *http.Request, key store.APIKey) (*store.Project, error) {
	values := r.Header.Values("X-Codex-Project")
	if len(values) > 1 {
		return nil, store.ErrInvalid
	}
	if len(values) == 1 && strings.TrimSpace(values[0]) != "" {
		project, err := s.store.ResolveProject(r.Context(), key.UserID, values[0])
		if err != nil {
			return nil, err
		}
		return &project, nil
	}
	if key.DefaultProjectID == nil {
		return nil, nil
	}
	project, err := s.store.GetProject(r.Context(), key.UserID, *key.DefaultProjectID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && project.Status != store.StatusActive) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &project, nil
}

func (s *Server) quotaLimits(key store.APIKey) store.QuotaLimits {
	limits := store.QuotaLimits{
		KeyRequestsPerMinute: int64(s.config.Limits.KeyRPM), UserRequestsPerMinute: int64(s.config.Limits.UserRPM),
		KeyConcurrent: int64(s.config.Limits.KeyConcurrent), UserConcurrent: int64(s.config.Limits.UserConcurrent),
		GlobalConcurrent: int64(s.config.Limits.GlobalConcurrent),
		KeyDailyRequests: s.config.Limits.KeyRequestsPerDay, UserDailyRequests: s.config.Limits.UserRequestsPerDay,
		KeyDailyTokens: s.config.Limits.KeyTokensPerDay, UserDailyTokens: s.config.Limits.UserTokensPerDay,
	}
	if key.RPMLimit != nil {
		limits.KeyRequestsPerMinute = int64(*key.RPMLimit)
	}
	if key.ConcurrentLimit != nil {
		limits.KeyConcurrent = int64(*key.ConcurrentLimit)
	}
	if key.DailyRequestLimit != nil {
		limits.KeyDailyRequests = int64(*key.DailyRequestLimit)
	}
	if key.DailyTokenLimit != nil {
		limits.KeyDailyTokens = *key.DailyTokenLimit
	}
	return limits
}

func (s *Server) renewQuotaLease(requestID string, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.store.RenewQuotaLease(ctx, requestID, now.UTC(), 5*time.Minute)
			cancel()
			if err != nil {
				s.logger.Warn("renew quota lease failed", "request_id", requestID)
			}
		}
	}
}

func (s *Server) createUpstreamAlert(ctx context.Context, userID, requestID string, failure *gatewayproxy.Failure) {
	alertType := ""
	title := ""
	severity := "warning"
	switch failure.Code {
	case "upstream_reauthentication_required":
		alertType, title, severity = "oauth_invalid", "上游 Codex 需要重新登录", "critical"
	case "upstream_rate_limited":
		alertType, title = "upstream_rate_limit", "上游已达到使用限制"
	case "upstream_unavailable", "upstream_timeout", "upstream_stream_error":
		alertType, title = "sidecar_unhealthy", "Codex 兼容层或上游不可用"
	default:
		return
	}
	_, _ = s.store.CreateAlert(ctx, store.CreateAlertParams{
		Type: alertType, Severity: severity, UserID: userID, RequestID: requestID,
		DedupeKey: alertType, Title: title, Details: map[string]any{"error_code": failure.Code},
	})
}

func timeOrNilValue(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
