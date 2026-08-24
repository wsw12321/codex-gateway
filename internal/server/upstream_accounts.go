package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	upstreamAccountSyncTimeout = 5 * time.Second
	upstreamQuotaTimeout       = 15 * time.Second
	upstreamQuotaDebounce      = 5 * time.Second
)

var errUpstreamAccountRangeExpired = errors.New("upstream account range predates retained request detail")

type upstreamQuotaState struct {
	running      bool
	lastAccepted time.Time
}

type upstreamQuotaLimiter struct {
	mu      sync.Mutex
	entries map[string]upstreamQuotaState
}

func newUpstreamQuotaLimiter() *upstreamQuotaLimiter {
	return &upstreamQuotaLimiter{entries: make(map[string]upstreamQuotaState)}
}

func (s *Server) upstreamQuotaLimiter() *upstreamQuotaLimiter {
	s.quotaOnce.Do(func() {
		if s.quotas == nil {
			s.quotas = newUpstreamQuotaLimiter()
		}
	})
	return s.quotas
}

func (l *upstreamQuotaLimiter) begin(accountID string, now time.Time) (func(), int, bool) {
	if l == nil {
		return func() {}, 0, true
	}
	l.mu.Lock()
	state := l.entries[accountID]
	if state.running {
		l.mu.Unlock()
		return nil, 1, false
	}
	if wait := upstreamQuotaDebounce - now.Sub(state.lastAccepted); !state.lastAccepted.IsZero() && wait > 0 {
		l.mu.Unlock()
		return nil, max(1, int(math.Ceil(wait.Seconds()))), false
	}
	state.running = true
	state.lastAccepted = now
	l.entries[accountID] = state
	if len(l.entries) > 1024 {
		for id, entry := range l.entries {
			if !entry.running && now.Sub(entry.lastAccepted) > time.Hour {
				delete(l.entries, id)
			}
		}
	}
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		state := l.entries[accountID]
		state.running = false
		l.entries[accountID] = state
		l.mu.Unlock()
	}, 0, true
}

type upstreamAccountUsageDTO struct {
	RequestCount      int64  `json:"request_count"`
	ErrorCount        int64  `json:"error_count"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	EquivalentCostUSD string `json:"equivalent_cost_usd"`
}

type upstreamAccountDTO struct {
	ID                string     `json:"id"`
	EmailMasked       string     `json:"email_masked"`
	Plan              string     `json:"plan"`
	Status            string     `json:"status"`
	LastSyncedAt      *time.Time `json:"last_synced_at"`
	RequestCount      int64      `json:"request_count"`
	ErrorCount        int64      `json:"error_count"`
	InputTokens       int64      `json:"input_tokens"`
	CachedInputTokens int64      `json:"cached_input_tokens"`
	CacheWriteTokens  int64      `json:"cache_write_tokens"`
	OutputTokens      int64      `json:"output_tokens"`
	ReasoningTokens   int64      `json:"reasoning_tokens"`
	EquivalentCostUSD string     `json:"equivalent_cost_usd"`
}

type upstreamAccountsResponse struct {
	From         *time.Time               `json:"from"`
	Until        time.Time                `json:"until"`
	All          bool                     `json:"all"`
	SyncWarning  string                   `json:"sync_warning,omitempty"`
	Accounts     []upstreamAccountDTO     `json:"accounts"`
	Unattributed *upstreamAccountUsageDTO `json:"unattributed,omitempty"`
}

func (s *Server) upstreamAccountsJSON(w http.ResponseWriter, r *http.Request) {
	query, err := parseUpstreamAccountQuery(time.Now(), r.URL.Query())
	if err != nil {
		if errors.Is(err, errUpstreamAccountRangeExpired) {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "upstream_account_detail_expired", "所选区间已超出明细保留期，请改用全部历史")
			return
		}
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_account_filter", "上游账号统计区间无效")
		return
	}

	// Keep list-and-apply snapshots ordered within this gateway instance. Without
	// serialization, a slower, older sidecar response could finish after a newer
	// response and overwrite fresher availability metadata.
	s.upstreamAccountSyncMu.Lock()
	syncCtx, cancelSync := context.WithTimeout(r.Context(), upstreamAccountSyncTimeout)
	remoteAccounts, err := s.upstream.ListUpstreamAccounts(syncCtx)
	cancelSync()
	syncWarning := ""
	if err != nil {
		s.upstreamAccountSyncMu.Unlock()
		syncWarning = "upstream_account_sync_unavailable"
		if s.logger != nil {
			code, _ := upstreamManagementErrorDetails(err)
			s.logger.Warn("upstream account metadata sync failed", "code", code)
		}
	} else {
		snapshots := make([]store.UpstreamAccountSnapshot, 0, len(remoteAccounts))
		for _, account := range remoteAccounts {
			status := store.UpstreamAccountStatusUnavailable
			if account.Status == "active" {
				status = store.UpstreamAccountStatusAvailable
			}
			snapshots = append(snapshots, store.UpstreamAccountSnapshot{
				ID: account.ID, MaskedEmail: account.MaskedEmail, Plan: account.Plan, Status: status,
				LastSyncedAt: account.LastSyncedAt,
			})
		}
		if err := s.store.SyncUpstreamAccounts(r.Context(), snapshots, time.Now().UTC()); err != nil {
			s.upstreamAccountSyncMu.Unlock()
			internalError(s, w, r, "sync upstream accounts", err)
			return
		}
		s.upstreamAccountSyncMu.Unlock()
	}

	// Recheck immediately before the detail query: waiting for the serialized
	// sidecar snapshot may have moved a boundary request beyond retention.
	if !query.All && query.From.Before(time.Now().UTC().Add(-globalUsageMaximumRange)) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "upstream_account_detail_expired", "所选区间已超出明细保留期，请改用全部历史")
		return
	}

	rows, err := s.store.SummarizeUpstreamAccounts(r.Context(), store.UpstreamAccountSummaryFilter{
		From: query.From, Until: query.Until, All: query.All, LiveFrom: query.LiveFrom,
	})
	if err != nil {
		internalError(s, w, r, "summarize upstream accounts", err)
		return
	}
	response := upstreamAccountsResponse{
		Until: query.Until, All: query.All, SyncWarning: syncWarning,
		Accounts: make([]upstreamAccountDTO, 0, len(rows)),
	}
	if !query.All {
		from := query.From
		response.From = &from
	}
	for _, row := range rows {
		usage := upstreamUsageDTO(row)
		if row.AccountID == nil {
			response.Unattributed = &usage
			continue
		}
		response.Accounts = append(response.Accounts, upstreamAccountDTO{
			ID: *row.AccountID, EmailMasked: row.MaskedEmail, Plan: row.Plan,
			Status: row.Status, LastSyncedAt: row.LastSyncedAt,
			RequestCount: usage.RequestCount, ErrorCount: usage.ErrorCount,
			InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
			CacheWriteTokens: usage.CacheWriteTokens, OutputTokens: usage.OutputTokens,
			ReasoningTokens: usage.ReasoningTokens, EquivalentCostUSD: usage.EquivalentCostUSD,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func upstreamUsageDTO(row store.UpstreamAccountSummary) upstreamAccountUsageDTO {
	return upstreamAccountUsageDTO{
		RequestCount: row.RequestCount, ErrorCount: row.ErrorCount,
		InputTokens: row.InputTokens, CachedInputTokens: row.CachedInputTokens,
		CacheWriteTokens: row.CacheWriteTokens, OutputTokens: row.OutputTokens,
		ReasoningTokens: row.ReasoningTokens, EquivalentCostUSD: row.EquivalentCostUSD,
	}
}

func parseUpstreamAccountQuery(now time.Time, values url.Values) (globalUsageQuery, error) {
	for key := range values {
		switch key {
		case "from", "until", "all":
		default:
			return globalUsageQuery{}, errors.New("unsupported upstream account filter")
		}
	}
	query, err := parseGlobalUsageQuery(now, values)
	if err != nil {
		return globalUsageQuery{}, err
	}
	// Bounded summaries are exact request-detail queries. Once the 90-day
	// retention job may have removed part of the interval, returning zero token
	// counts beside a nonzero immutable-ledger cost would be misleading. The
	// all-history path is durable through monthly aggregates and remains valid.
	if !query.All && query.From.Before(now.UTC().Add(-globalUsageMaximumRange)) {
		return globalUsageQuery{}, errUpstreamAccountRangeExpired
	}
	return query, nil
}

type quotaWindowDTO struct {
	UsedRatio      float64    `json:"used_ratio"`
	RemainingRatio float64    `json:"remaining_ratio"`
	ResetsAt       *time.Time `json:"resets_at"`
}

type additionalQuotaWindowDTO struct {
	Name           string     `json:"name"`
	UsedRatio      float64    `json:"used_ratio"`
	RemainingRatio float64    `json:"remaining_ratio"`
	ResetsAt       *time.Time `json:"resets_at"`
	WindowSeconds  *int64     `json:"window_seconds,omitempty"`
}

type upstreamQuotaResponse struct {
	QueriedAt         time.Time                  `json:"queried_at"`
	Plan              string                     `json:"plan"`
	FiveHour          quotaWindowDTO             `json:"five_hour"`
	SevenDay          quotaWindowDTO             `json:"seven_day"`
	AdditionalWindows []additionalQuotaWindowDTO `json:"additional_windows"`
}

func (s *Server) upstreamAccountQuota(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if !validUpstreamAccountID(accountID) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	now := time.Now().UTC()
	started := time.Now()
	resultCode := "sidecar_request_failed"
	resultStatus := http.StatusBadGateway
	success := false
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer cancel()
		_, _ = s.store.AppendAuditEvent(ctx, store.AppendAuditEventParams{
			OccurredAt: time.Now().UTC(), ActorUserID: userFrom(r.Context()).ID,
			ActorSessionID: sessionFrom(r.Context()).ID, EventType: "upstream_account.quota_queried",
			Severity: "info", Success: success, SourceIP: safeIP(r.Context()),
			SubjectType: "upstream_account", SubjectID: accountID,
			RequestID: httpx.RequestID(r.Context()), Metadata: map[string]any{
				"duration_ms": time.Since(started).Milliseconds(), "result_code": resultCode,
				"http_status": resultStatus,
			},
		})
	}()

	release, retryAfter, accepted := s.upstreamQuotaLimiter().begin(accountID, now)
	if !accepted {
		resultCode = "upstream_quota_query_rate_limited"
		resultStatus = http.StatusTooManyRequests
		w.Header().Set("Retry-After", integerString(retryAfter))
		httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", resultCode, "额度查询过于频繁，请稍后重试")
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), upstreamQuotaTimeout)
	quota, err := s.upstream.QueryUpstreamAccountQuota(ctx, accountID)
	cancel()
	if err != nil {
		resultCode, resultStatus = upstreamManagementErrorDetails(err)
		writeUpstreamManagementError(w, r, err, "无法查询官方额度")
		return
	}
	additional := make([]additionalQuotaWindowDTO, 0, len(quota.AdditionalWindows))
	for _, window := range quota.AdditionalWindows {
		additional = append(additional, additionalQuotaWindowDTO{
			Name: window.Name, UsedRatio: window.UsedRatio, RemainingRatio: window.RemainingRatio,
			ResetsAt: window.ResetAt, WindowSeconds: window.WindowSeconds,
		})
	}
	resultCode = "ok"
	resultStatus = http.StatusOK
	success = true
	writeJSON(w, http.StatusOK, upstreamQuotaResponse{
		QueriedAt: quota.QueriedAt, Plan: quota.Plan,
		FiveHour:          quotaWindowDTO{UsedRatio: quota.FiveHour.UsedRatio, RemainingRatio: quota.FiveHour.RemainingRatio, ResetsAt: quota.FiveHour.ResetAt},
		SevenDay:          quotaWindowDTO{UsedRatio: quota.SevenDay.UsedRatio, RemainingRatio: quota.SevenDay.RemainingRatio, ResetsAt: quota.SevenDay.ResetAt},
		AdditionalWindows: additional,
	})
}

func validUpstreamAccountID(value string) bool {
	if len(value) != 16 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func upstreamManagementErrorDetails(err error) (string, int) {
	var internalErr *gatewayproxy.InternalAPIError
	if !errors.As(err, &internalErr) {
		return "sidecar_unavailable", http.StatusBadGateway
	}
	code := internalErr.SafeCode()
	status := http.StatusBadGateway
	switch code {
	case "invalid_upstream_account":
		status = http.StatusNotFound
	case "upstream_quota_rate_limited":
		status = http.StatusTooManyRequests
	case "sidecar_timeout":
		status = http.StatusGatewayTimeout
	case "upstream_reauthentication_required", "sidecar_unavailable":
		status = http.StatusServiceUnavailable
	case "sidecar_invalid_response", "sidecar_request_failed":
		// These are fixed, client-safe proxy error codes.
	default:
		// Do not let a newly added or accidentally data-derived internal code
		// become part of the browser response or the durable audit metadata.
		code = "sidecar_request_failed"
	}
	return code, status
}

func writeUpstreamManagementError(w http.ResponseWriter, r *http.Request, err error, message string) {
	code, status := upstreamManagementErrorDetails(err)
	var internalErr *gatewayproxy.InternalAPIError
	if errors.As(err, &internalErr) && internalErr.RetryAfter > 0 && internalErr.RetryAfter <= 3600 {
		w.Header().Set("Retry-After", integerString(internalErr.RetryAfter))
	}
	httpx.WriteError(w, r, status, "upstream_error", code, message)
}
