package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
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
	upstreamQuotaRequestBytes  = 256
	upstreamStatusRequestBytes = 256
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
	ID                  string     `json:"id"`
	EmailMasked         string     `json:"email_masked"`
	Plan                string     `json:"plan"`
	Status              string     `json:"status"`
	CliproxyStatus      string     `json:"cliproxy_status"`
	GatewayManualStatus string     `json:"gateway_manual_status"`
	GatewayQuotaStatus  string     `json:"gateway_quota_status"`
	CanManage           bool       `json:"can_manage"`
	LastSyncedAt        *time.Time `json:"last_synced_at"`
	RequestCount        int64      `json:"request_count"`
	ErrorCount          int64      `json:"error_count"`
	InputTokens         int64      `json:"input_tokens"`
	CachedInputTokens   int64      `json:"cached_input_tokens"`
	CacheWriteTokens    int64      `json:"cache_write_tokens"`
	OutputTokens        int64      `json:"output_tokens"`
	ReasoningTokens     int64      `json:"reasoning_tokens"`
	EquivalentCostUSD   string     `json:"equivalent_cost_usd"`
	AllocationWeight    int        `json:"allocation_weight"`
	ConcurrentLimit     int        `json:"concurrent_limit"`
	RollingCostUSD      string     `json:"rolling_cost_usd"`
	RollingCostShare    string     `json:"rolling_cost_share"`
	TargetShare         string     `json:"target_share"`
	AccessMode          string     `json:"access_mode"`
	AuthorizedUserIDs   []string   `json:"authorized_user_ids"`
}

type upstreamAccountsResponse struct {
	AllocationFrom  time.Time                `json:"allocation_from"`
	AllocationUntil time.Time                `json:"allocation_until"`
	From            *time.Time               `json:"from"`
	Until           time.Time                `json:"until"`
	All             bool                     `json:"all"`
	SyncWarning     string                   `json:"sync_warning,omitempty"`
	Accounts        []upstreamAccountDTO     `json:"accounts"`
	Unattributed    *upstreamAccountUsageDTO `json:"unattributed,omitempty"`
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
	defer s.upstreamAccountSyncMu.Unlock()
	syncCtx, cancelSync := context.WithTimeout(r.Context(), upstreamAccountSyncTimeout)
	remoteAccounts, err := s.upstream.ListUpstreamAccounts(syncCtx)
	cancelSync()
	syncWarning := ""
	manageable := make(map[string]bool, len(remoteAccounts))
	remoteStatus := make(map[string]gatewayproxy.UpstreamAccount, len(remoteAccounts))
	if err != nil {
		syncWarning = "upstream_account_sync_unavailable"
		if s.logger != nil {
			code, _ := upstreamManagementErrorDetails(err)
			s.logger.Warn("upstream account metadata sync failed", "code", code)
		}
	} else {
		snapshots := make([]store.UpstreamAccountSnapshot, 0, len(remoteAccounts))
		for _, account := range remoteAccounts {
			known := upstreamAccountSourceStatusKnown(account)
			manageable[account.ID] = known
			remoteStatus[account.ID] = account
			status := store.UpstreamAccountStatusUnavailable
			if known && account.Status == "available" {
				status = store.UpstreamAccountStatusAvailable
			}
			snapshots = append(snapshots, store.UpstreamAccountSnapshot{
				ID: account.ID, MaskedEmail: account.MaskedEmail, Plan: account.Plan, Status: status,
				LastSyncedAt: account.LastSyncedAt,
			})
		}
		if err := s.store.SyncUpstreamAccounts(r.Context(), snapshots, time.Now().UTC()); err != nil {
			internalError(s, w, r, "sync upstream accounts", err)
			return
		}
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
	// Allocation always uses the current rolling window, independently of the
	// historical statistics filter. Weights and costs share one DB snapshot.
	allocationUntil := time.Now().UTC()
	allocations, err := s.store.ListUpstreamAccountAllocations(r.Context(), allocationUntil)
	if err != nil {
		internalError(s, w, r, "list upstream allocations", err)
		return
	}
	allocationByID := make(map[string]store.UpstreamAccountAllocation, len(allocations))
	for _, allocation := range allocations {
		allocationByID[allocation.AccountID] = allocation
	}
	access, err := s.store.ListUpstreamAccountAccess(r.Context())
	if err != nil {
		internalError(s, w, r, "list upstream account access", err)
		return
	}
	accessByID := make(map[string]store.UpstreamAccountAccess, len(access))
	for _, item := range access {
		accessByID[item.AccountID] = item
	}
	response := upstreamAccountsResponse{
		AllocationFrom: allocationUntil.Add(-24 * time.Hour), AllocationUntil: allocationUntil,
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
		allocation := allocationByID[*row.AccountID]
		accountAccess := accessByID[*row.AccountID]
		remote, present := remoteStatus[*row.AccountID]
		sourceKnown := present && upstreamAccountSourceStatusKnown(remote)
		cliproxyStatus, gatewayManualStatus, gatewayQuotaStatus, finalStatus := "unknown", "unknown", "unknown", "unknown"
		if sourceKnown {
			cliproxyStatus, gatewayManualStatus, gatewayQuotaStatus = remote.CliproxyStatus, remote.GatewayManualStatus, remote.GatewayQuotaStatus
			finalStatus = row.Status
			if finalStatus != store.UpstreamAccountStatusAvailable && finalStatus != store.UpstreamAccountStatusUnavailable {
				finalStatus = "unknown"
			}
		}
		response.Accounts = append(response.Accounts, upstreamAccountDTO{
			AccessMode: accountAccess.Mode, AuthorizedUserIDs: accountAccess.UserIDs,
			AllocationWeight: allocation.AllocationWeight, ConcurrentLimit: row.ConcurrentLimit, RollingCostUSD: allocation.CostUSD,
			RollingCostShare: allocation.CostShare, TargetShare: allocation.TargetShare,
			ID: *row.AccountID, EmailMasked: row.MaskedEmail, Plan: row.Plan,
			Status: finalStatus, CliproxyStatus: cliproxyStatus, GatewayManualStatus: gatewayManualStatus,
			GatewayQuotaStatus: gatewayQuotaStatus, CanManage: manageable[*row.AccountID], LastSyncedAt: row.LastSyncedAt,
			RequestCount: usage.RequestCount, ErrorCount: usage.ErrorCount,
			InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
			CacheWriteTokens: usage.CacheWriteTokens, OutputTokens: usage.OutputTokens,
			ReasoningTokens: usage.ReasoningTokens, EquivalentCostUSD: usage.EquivalentCostUSD,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func upstreamAccountSourceStatusKnown(account gatewayproxy.UpstreamAccount) bool {
	return account.CliproxyStatus != "" && account.GatewayManualStatus != "" && account.GatewayQuotaStatus != ""
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

func (s *Server) setUpstreamAccountStatus(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if !validUpstreamAccountID(accountID) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.URL.RawQuery != "" {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_account_status_request", "账号状态请求必须为 JSON，且不能包含查询参数")
		return
	}
	enabled, err := decodeUpstreamAccountStatusRequest(r)
	if err != nil {
		badJSON(w, r, err)
		return
	}

	started := time.Now()
	resultCode, resultStatus, success := "sidecar_request_failed", http.StatusBadGateway, false
	eventType := "upstream_account.disabled"
	if enabled {
		eventType = "upstream_account.enabled"
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer cancel()
		_, err := s.store.AppendAuditEvent(ctx, store.AppendAuditEventParams{
			OccurredAt: time.Now().UTC(), ActorUserID: userFrom(r.Context()).ID,
			ActorSessionID: sessionFrom(r.Context()).ID, EventType: eventType,
			Severity: "info", Success: success, SourceIP: safeIP(r.Context()),
			SubjectType: "upstream_account", SubjectID: accountID,
			RequestID: httpx.RequestID(r.Context()), Metadata: map[string]any{
				"duration_ms": time.Since(started).Milliseconds(), "result_code": resultCode,
				"http_status": resultStatus,
			},
		})
		if err != nil && s.logger != nil {
			s.logger.Error("upstream account status audit failed", "actor_user_id", userFrom(r.Context()).ID,
				"account_id", accountID, "event_type", eventType, "success", success,
				"result_code", resultCode, "code", "upstream_account_status_audit_failed")
		}
	}()

	// The same lock covers the entire list snapshot and summary response. An
	// older list must finish before an administrator's confirmed state change.
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), upstreamAccountSyncTimeout)
	result, err := s.upstream.SetUpstreamAccountStatus(ctx, accountID, enabled)
	cancel()
	if err != nil {
		resultCode, resultStatus = upstreamManagementErrorDetails(err)
		writeUpstreamManagementError(w, r, err, "无法修改上游账号状态")
		return
	}
	if result.CliproxyStatus == "" || result.GatewayManualStatus == "" || result.GatewayQuotaStatus == "" {
		err := &gatewayproxy.InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_account_status_protocol_error"}
		resultCode, resultStatus = upstreamManagementErrorDetails(err)
		writeUpstreamManagementError(w, r, err, "无法修改上游账号状态")
		return
	}
	resultCode, resultStatus, success = "ok", http.StatusOK, true
	// The sidecar owns the control state. Return its confirmation independently
	// of the dashboard's next list/statistics refresh or audit database failures.
	writeJSON(w, http.StatusOK, result)
}

func decodeUpstreamAccountStatusRequest(r *http.Request) (bool, error) {
	if r.ContentLength > upstreamStatusRequestBytes {
		return false, &http.MaxBytesError{Limit: upstreamStatusRequestBytes}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, upstreamStatusRequestBytes+1))
	if err != nil {
		return false, err
	}
	if len(body) > upstreamStatusRequestBytes {
		return false, &http.MaxBytesError{Limit: upstreamStatusRequestBytes}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false, errors.New("account status request must be an object")
	}
	key, err := decoder.Token()
	if err != nil || key != "enabled" {
		return false, errors.New("account status request requires enabled")
	}
	var enabled *bool
	if err := decoder.Decode(&enabled); err != nil || enabled == nil {
		return false, errors.New("account status enabled must be a boolean")
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return false, errors.New("account status request must contain only enabled")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false, errors.New("request must contain exactly one JSON value")
	}
	return *enabled, nil
}

type upstreamQuotaRPCRequest struct {
	Method *string `json:"method"`
	ID     *int64  `json:"id"`
}

func (s *Server) upstreamAccountQuota(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if !validUpstreamAccountID(accountID) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	var input upstreamQuotaRPCRequest
	if err := decodeUpstreamQuotaRPCRequest(r, &input); err != nil {
		badJSON(w, r, err)
		return
	}
	if input.Method == nil || *input.Method != gatewayproxy.AccountRateLimitsMethod ||
		input.ID == nil || *input.ID != gatewayproxy.AccountRateLimitsRequestID {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_quota_request", "额度查询协议无效")
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
	resultCode = "ok"
	resultStatus = http.StatusOK
	success = true
	writeJSON(w, http.StatusOK, quota)
}

func decodeUpstreamQuotaRPCRequest(r *http.Request, destination *upstreamQuotaRPCRequest) error {
	if r.ContentLength > upstreamQuotaRequestBytes {
		return &http.MaxBytesError{Limit: upstreamQuotaRequestBytes}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, upstreamQuotaRequestBytes+1))
	if err != nil {
		return err
	}
	if len(body) > upstreamQuotaRequestBytes {
		return &http.MaxBytesError{Limit: upstreamQuotaRequestBytes}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("quota RPC request must be an object")
	}
	var seenMethod, seenID bool
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("quota RPC request key must be a string")
		}
		switch key {
		case "method":
			if seenMethod {
				return errors.New("duplicate quota RPC method")
			}
			seenMethod = true
			if err := decoder.Decode(&destination.Method); err != nil {
				return err
			}
		case "id":
			if seenID {
				return errors.New("duplicate quota RPC id")
			}
			seenID = true
			if err := decoder.Decode(&destination.ID); err != nil {
				return err
			}
		default:
			return errors.New("unknown quota RPC field")
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return errors.New("quota RPC request object was not closed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
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

var upstreamManagementErrors = map[string]struct {
	status  int
	message string
}{
	"sidecar_account_status_protocol_error":       {http.StatusBadGateway, "账号管理服务拒绝了状态修改协议，请管理员检查 Gateway 与 sidecar 版本"},
	"upstream_account_identity_invalid":           {http.StatusConflict, "上游账号身份校验失败，请管理员检查 OAuth 登录状态"},
	"upstream_account_status_persistence_failed":  {http.StatusServiceUnavailable, "上游账号状态保存失败，账号继续保持不可用，请检查 sidecar 持久卷后重试"},
	"invalid_upstream_account":                    {http.StatusNotFound, "上游账号不存在，请刷新账号列表"},
	"upstream_account_disabled":                   {http.StatusConflict, "该上游账号已停用，无法查询额度"},
	"upstream_quota_rate_limited":                 {http.StatusTooManyRequests, "ChatGPT 额度查询被限流，请稍后重试"},
	"upstream_reauthentication_required":          {http.StatusServiceUnavailable, "ChatGPT 额度接口拒绝了账号认证，请管理员检查该账号的登录状态"},
	"sidecar_unavailable":                         {http.StatusServiceUnavailable, "无法连接上游账号管理服务，请管理员检查 sidecar 运行状态和内部网络"},
	"sidecar_timeout":                             {http.StatusGatewayTimeout, "等待上游账号管理服务响应超时，请稍后重试"},
	"sidecar_invalid_response":                    {http.StatusBadGateway, "上游账号管理服务返回的数据不符合协议，请管理员检查 Gateway 与 sidecar 版本"},
	"sidecar_request_failed":                      {http.StatusBadGateway, "上游账号管理服务返回未识别的错误，请管理员检查 sidecar 状态和版本"},
	"sidecar_auth_failed":                         {http.StatusServiceUnavailable, "上游账号管理服务的内部认证失败，请管理员检查 Gateway 与 sidecar 的内部密钥配置"},
	"sidecar_auth_unavailable":                    {http.StatusServiceUnavailable, "上游账号管理服务的内部认证未就绪，请管理员检查 sidecar 配置"},
	"sidecar_account_registry_unavailable":        {http.StatusServiceUnavailable, "上游账号管理服务的账号管理未就绪，请管理员检查 sidecar 状态"},
	"sidecar_quota_protocol_error":                {http.StatusBadGateway, "额度查询服务拒绝了查询协议，请管理员检查 Gateway 与 sidecar 版本"},
	"upstream_quota_account_identity_unavailable": {http.StatusBadGateway, "上游账号缺少有效账号标识，请管理员检查 OAuth 登录状态"},
	"upstream_quota_credential_unavailable":       {http.StatusBadGateway, "上游账号缺少有效登录凭据，请管理员重新登录该账号"},
	"upstream_quota_request_failed":               {http.StatusBadGateway, "额度查询服务无法构造 ChatGPT 请求，请管理员检查 sidecar 配置"},
	"upstream_quota_unavailable":                  {http.StatusBadGateway, "额度查询服务无法连接 ChatGPT，请管理员检查 sidecar 出站网络和代理"},
	"upstream_quota_upstream_error":               {http.StatusBadGateway, "ChatGPT 额度接口返回错误，请稍后重试"},
	"upstream_quota_timeout":                      {http.StatusGatewayTimeout, "额度查询服务请求 ChatGPT 超时，请检查出站网络或稍后重试"},
	"upstream_quota_invalid_response":             {http.StatusBadGateway, "ChatGPT 额度响应读取失败或过大，请稍后重试"},
	"upstream_quota_schema_changed":               {http.StatusBadGateway, "ChatGPT 额度数据无法解析，可能需要更新额度适配器"},
	"upstream_quota_json_invalid":                 {http.StatusBadGateway, "ChatGPT 额度响应不是完整的单一 JSON，可能返回了非 JSON 页面"},
	"upstream_quota_field_type_invalid":           {http.StatusBadGateway, "ChatGPT 额度字段类型不符合预期，需检查额度适配器兼容性"},
	"upstream_quota_rate_limit_missing":           {http.StatusBadGateway, "ChatGPT 响应缺少额度结构，需检查额度适配器兼容性"},
	"upstream_quota_plan_unsupported":             {http.StatusBadGateway, "旧版额度适配器拒绝了套餐标识，请管理员更新 codex-compat"},
	"upstream_quota_percent_missing":              {http.StatusBadGateway, "ChatGPT 额度窗口缺少已用百分比 used_percent"},
	"upstream_quota_percent_out_of_range":         {http.StatusBadGateway, "ChatGPT 额度已用百分比超出 0–100 的有效范围"},
	"upstream_quota_percent_fractional":           {http.StatusBadGateway, "ChatGPT 额度返回了小数百分比，当前适配器只接受整数"},
	"upstream_quota_window_invalid":               {http.StatusBadGateway, "ChatGPT 额度窗口时长不在当前适配器接受的范围内"},
	"upstream_quota_reset_invalid":                {http.StatusBadGateway, "ChatGPT 额度重置时间不在当前适配器接受的范围内"},
	"upstream_quota_limit_id_invalid":             {http.StatusBadGateway, "ChatGPT 额外额度标识不符合当前适配器格式要求"},
	"upstream_quota_limit_id_duplicate":           {http.StatusBadGateway, "ChatGPT 额度标识重复或与默认 codex 额度冲突"},
	"upstream_quota_limits_excessive":             {http.StatusBadGateway, "ChatGPT 返回的额外额度数量超过当前适配器上限"},
}

func upstreamManagementErrorDetails(err error) (string, int) {
	var internalErr *gatewayproxy.InternalAPIError
	if !errors.As(err, &internalErr) {
		return "sidecar_unavailable", http.StatusBadGateway
	}
	code := internalErr.SafeCode()
	if details, ok := upstreamManagementErrors[code]; ok {
		return code, details.status
	}
	// Only fixed codes may reach the browser or durable audit metadata.
	return "sidecar_request_failed", http.StatusBadGateway
}

func writeUpstreamManagementError(w http.ResponseWriter, r *http.Request, err error, message string) {
	code, status := upstreamManagementErrorDetails(err)
	if details, ok := upstreamManagementErrors[code]; ok {
		message = details.message
	}
	var internalErr *gatewayproxy.InternalAPIError
	if errors.As(err, &internalErr) && internalErr.RetryAfter > 0 && internalErr.RetryAfter <= 3600 {
		w.Header().Set("Retry-After", integerString(internalErr.RetryAfter))
	}
	httpx.WriteError(w, r, status, "upstream_error", code, message)
}
