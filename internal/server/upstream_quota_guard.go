package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	upstreamQuotaGuardCheckTimeout  = 30 * time.Second
	upstreamQuotaGuardRecordTimeout = 3 * time.Second
	upstreamQuotaGuardConcurrency   = 4
	upstreamQuotaGuardRequestBytes  = 256
	upstreamQuotaGuardMaxEntries    = 4096
)

// upstreamQuotaGuardRepository is the store surface used outside request
// handlers: automatic checks and readings from other quota queries.
type upstreamQuotaGuardRepository interface {
	ClaimUpstreamQuotaCheck(context.Context, string, time.Time) (store.UpstreamQuotaCheckClaim, error)
	RecordUpstreamQuotaObservation(context.Context, store.RecordUpstreamQuotaObservationParams) (store.UpstreamQuotaGuardState, error)
}

// upstreamQuotaGuardReader reads one account's windows for an automatic
// check. A non-empty code is a fixed result code, never upstream text.
type upstreamQuotaGuardReader func(ctx context.Context, provider, accountID string) (store.UpstreamQuotaObservation, string)

type upstreamQuotaGuardEntry struct {
	next    time.Time
	running bool
}

// upstreamQuotaGuardScheduler keeps requests from touching the database until
// an account's next automatic check is due. The database claim remains the
// authority for the minimum interval.
type upstreamQuotaGuardScheduler struct {
	mu      sync.Mutex
	entries map[string]upstreamQuotaGuardEntry
	slots   chan struct{}
}

func newUpstreamQuotaGuardScheduler() *upstreamQuotaGuardScheduler {
	return &upstreamQuotaGuardScheduler{
		entries: make(map[string]upstreamQuotaGuardEntry),
		slots:   make(chan struct{}, upstreamQuotaGuardConcurrency),
	}
}

func (s *Server) upstreamQuotaGuardScheduler() *upstreamQuotaGuardScheduler {
	s.quotaGuardOnce.Do(func() {
		if s.quotaGuards == nil {
			s.quotaGuards = newUpstreamQuotaGuardScheduler()
		}
	})
	return s.quotaGuards
}

// begin reserves a check slot when the account is due. A busy scheduler
// simply skips: the account's next request tries again.
func (g *upstreamQuotaGuardScheduler) begin(accountID string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entries[accountID]
	if entry.running || now.Before(entry.next) {
		return false
	}
	select {
	case g.slots <- struct{}{}:
	default:
		return false
	}
	entry.running = true
	g.entries[accountID] = entry
	if len(g.entries) > upstreamQuotaGuardMaxEntries {
		for id, item := range g.entries {
			if !item.running && !now.Before(item.next) {
				delete(g.entries, id)
			}
		}
	}
	return true
}

func (g *upstreamQuotaGuardScheduler) finish(accountID string, next time.Time) {
	g.mu.Lock()
	g.entries[accountID] = upstreamQuotaGuardEntry{next: next}
	g.mu.Unlock()
	<-g.slots
}

// reschedule adopts a schedule written by another path, such as a manual
// change or an Owner query, unless a check is running.
func (g *upstreamQuotaGuardScheduler) reschedule(accountID string, next time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if entry := g.entries[accountID]; !entry.running {
		g.entries[accountID] = upstreamQuotaGuardEntry{next: next}
	}
}

// forget drops cached schedules so every account consults the database on
// its next request, for example after the safety percentage changes.
func (g *upstreamQuotaGuardScheduler) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, entry := range g.entries {
		if !entry.running {
			delete(g.entries, id)
		}
	}
}

// noteUpstreamQuotaUse runs when an account starts serving a request. Only
// accounts in use are checked; an idle account is never queried.
func (s *Server) noteUpstreamQuotaUse(accountID string) {
	if s.quotaGuardRepo == nil || !validUpstreamAccountID(accountID) {
		return
	}
	scheduler := s.upstreamQuotaGuardScheduler()
	if !scheduler.begin(accountID, time.Now()) {
		return
	}
	go func() {
		scheduler.finish(accountID, s.runUpstreamQuotaCheck(accountID))
	}()
}

// runUpstreamQuotaCheck performs one automatic check and returns when the
// next one may start. Failures leave the state unchanged and wait for the
// minimum interval, which the claim has already recorded.
func (s *Server) runUpstreamQuotaCheck(accountID string) time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamQuotaGuardCheckTimeout)
	defer cancel()
	now := time.Now().UTC()
	retry := now.Add(store.UpstreamQuotaGuardMinInterval)
	claim, err := s.quotaGuardRepo.ClaimUpstreamQuotaCheck(ctx, accountID, now)
	if err != nil {
		s.logQuotaGuardFailure("", "upstream_quota_guard_claim_failed")
		return retry
	}
	if claim.Provider == "" {
		// Not a Codex or Claude account, or not synchronized yet.
		return retry
	}
	if !claim.Claimed {
		return claim.NextCheckAt
	}
	observation, code := s.readUpstreamQuotaGuard(ctx, claim.Provider, accountID)
	if code != "" {
		s.logQuotaGuardFailure(claim.Provider, code)
		return retry
	}
	state, err := s.quotaGuardRepo.RecordUpstreamQuotaObservation(ctx, store.RecordUpstreamQuotaObservationParams{
		Provider: claim.Provider, AccountID: accountID, Observation: observation, Automatic: true, At: now,
	})
	if err != nil {
		s.logQuotaGuardFailure(claim.Provider, "upstream_quota_guard_record_failed")
		return retry
	}
	return state.NextAutomaticCheck()
}

func (s *Server) logQuotaGuardFailure(provider, code string) {
	if s.logger != nil {
		s.logger.Warn("upstream quota guard check failed", "provider", provider, "code", code)
	}
}

func (s *Server) readUpstreamQuotaGuard(ctx context.Context, provider, accountID string) (store.UpstreamQuotaObservation, string) {
	if s.quotaGuardRead != nil {
		return s.quotaGuardRead(ctx, provider, accountID)
	}
	switch provider {
	case store.UpstreamProviderCodex:
		at := time.Now().UTC()
		quota, code := s.queryCodexQuota(ctx, accountID, "quota_guard")
		if code != "" {
			return store.UpstreamQuotaObservation{}, code
		}
		usage := codexAccountUsage(quota, at)
		// Group members may see this account's usage; reuse the fresh result.
		s.accountUsageCache().store(accountUsageKey(store.UpstreamProviderCodex, accountID), usage, accountUsageCodexTTL)
		return upstreamQuotaObservation(usage, at), ""
	case store.UpstreamProviderAnthropic:
		usage, code := s.readAnthropicAccountUsage(ctx, accountID)
		if code != "" {
			return store.UpstreamQuotaObservation{}, code
		}
		return upstreamQuotaObservation(usage, time.Now().UTC()), ""
	default:
		return store.UpstreamQuotaObservation{}, "usage_unavailable"
	}
}

// queryCodexQuota sends one live ChatGPT quota request through the
// per-account limiter shared with Owner and member queries, and audits it.
// The limiter's own refusal is returned as upstream_quota_query_rate_limited
// without an audit event, because no upstream request was made.
func (s *Server) queryCodexQuota(ctx context.Context, accountID, via string) (gatewayproxy.UpstreamQuota, string) {
	if s.upstream == nil {
		return gatewayproxy.UpstreamQuota{}, "sidecar_unavailable"
	}
	now := time.Now().UTC()
	release, _, accepted := s.upstreamQuotaLimiter().begin(accountID, now)
	if !accepted {
		return gatewayproxy.UpstreamQuota{}, "upstream_quota_query_rate_limited"
	}
	defer release()
	queryCtx, cancel := context.WithTimeout(ctx, upstreamQuotaTimeout)
	quota, err := s.upstream.QueryUpstreamAccountQuota(queryCtx, accountID)
	cancel()
	resultCode, resultStatus := "ok", http.StatusOK
	if err != nil {
		resultCode, resultStatus = upstreamManagementErrorDetails(err)
	}
	auditCtx, cancelAudit := context.WithTimeout(ctx, 3*time.Second)
	_, _ = s.store.AppendAuditEvent(auditCtx, store.AppendAuditEventParams{
		OccurredAt: time.Now().UTC(), ActorUserID: userFrom(ctx).ID,
		ActorSessionID: sessionFrom(ctx).ID, EventType: "upstream_account.quota_queried",
		Severity: "info", Success: err == nil, SourceIP: safeIP(ctx),
		SubjectType: "upstream_account", SubjectID: accountID,
		RequestID: httpx.RequestID(ctx), Metadata: map[string]any{
			"duration_ms": time.Since(now).Milliseconds(), "result_code": resultCode,
			"http_status": resultStatus, "via": via,
		},
	})
	cancelAudit()
	if err != nil {
		return gatewayproxy.UpstreamQuota{}, resultCode
	}
	return quota, ""
}

// upstreamQuotaObservation keeps only windows that report a used percentage.
func upstreamQuotaObservation(usage accountUsage, at time.Time) store.UpstreamQuotaObservation {
	observation := store.UpstreamQuotaObservation{ObservedAt: at}
	if usage.UpdatedAt != nil {
		observation.ObservedAt = *usage.UpdatedAt
	}
	window := func(value *accountUsageWindow) *store.UpstreamQuotaWindow {
		if value == nil || value.UsedPercent == nil {
			return nil
		}
		return &store.UpstreamQuotaWindow{UsedPercent: *value.UsedPercent, ResetAt: value.ResetAt}
	}
	observation.FiveHour, observation.SevenDay = window(usage.FiveHour), window(usage.SevenDay)
	return observation
}

// recordUpstreamQuotaReading applies a quota reading obtained by an Owner or
// member query. It never fails the caller's request.
func (s *Server) recordUpstreamQuotaReading(ctx context.Context, provider, accountID string, usage accountUsage) {
	if s.quotaGuardRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamQuotaGuardRecordTimeout)
	defer cancel()
	now := time.Now().UTC()
	state, err := s.quotaGuardRepo.RecordUpstreamQuotaObservation(ctx, store.RecordUpstreamQuotaObservationParams{
		Provider: provider, AccountID: accountID, Observation: upstreamQuotaObservation(usage, now), At: now,
	})
	if err != nil {
		s.logQuotaGuardFailure(provider, "upstream_quota_guard_record_failed")
		return
	}
	s.upstreamQuotaGuardScheduler().reschedule(accountID, state.NextAutomaticCheck())
}

type upstreamQuotaWindowDTO struct {
	UsedPercent int        `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at"`
}

type upstreamQuotaGuardDTO struct {
	Status        string                  `json:"status"`
	Source        string                  `json:"source"`
	LimitedWindow string                  `json:"limited_window,omitempty"`
	ResetAt       *time.Time              `json:"reset_at"`
	RestoreAt     *time.Time              `json:"restore_at"`
	ChangedAt     *time.Time              `json:"changed_at"`
	ObservedAt    *time.Time              `json:"observed_at"`
	FiveHour      *upstreamQuotaWindowDTO `json:"five_hour"`
	SevenDay      *upstreamQuotaWindowDTO `json:"seven_day"`
	NextCheckAt   *time.Time              `json:"next_check_at"`
}

// upstreamQuotaGuardView describes an account; one without a stored state
// has never been checked and is available.
func upstreamQuotaGuardView(state store.UpstreamQuotaGuardState, present bool) *upstreamQuotaGuardDTO {
	if !present {
		return &upstreamQuotaGuardDTO{Status: store.UpstreamQuotaGuardAvailable, Source: store.UpstreamQuotaGuardSourceAuto}
	}
	window := func(value *store.UpstreamQuotaWindow) *upstreamQuotaWindowDTO {
		if value == nil {
			return nil
		}
		return &upstreamQuotaWindowDTO{UsedPercent: value.UsedPercent, ResetAt: value.ResetAt}
	}
	view := &upstreamQuotaGuardDTO{
		Status: state.Status, Source: state.Source, LimitedWindow: state.LimitedWindow,
		ResetAt: state.ResetAt, RestoreAt: state.RestoreAt, ChangedAt: state.ChangedAt, ObservedAt: state.ObservedAt,
		FiveHour: window(state.FiveHour), SevenDay: window(state.SevenDay),
	}
	if next := state.NextAutomaticCheck(); !next.IsZero() {
		next = next.UTC()
		view.NextCheckAt = &next
	}
	return view
}

func (s *Server) setUpstreamAccountQuotaGuard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_quota_guard_request", "额度状态请求必须为 JSON，且不能包含查询参数")
		return
	}
	var limited *bool
	if err := decodeSingleJSONField(r, upstreamQuotaGuardRequestBytes, "limited", &limited); err != nil {
		badJSON(w, r, err)
		return
	}
	if limited == nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_quota_guard_status", "额度状态 limited 必须为布尔值")
		return
	}
	state, err := s.accountStore(r).SetUpstreamQuotaGuardStatus(r.Context(), store.SetUpstreamQuotaGuardStatusParams{
		AccountID: id, Limited: *limited, At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set upstream quota guard", err)
		return
	}
	s.upstreamQuotaGuardScheduler().reschedule(id, state.NextAutomaticCheck())
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "quota_guard": upstreamQuotaGuardView(state, true)})
}

func (s *Server) setUpstreamQuotaGuardSettings(w http.ResponseWriter, r *http.Request) {
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_quota_guard_request", "剩余用量保险额度请求必须为 JSON，且不能包含查询参数")
		return
	}
	var percent *int64
	if err := decodeSingleJSONField(r, upstreamQuotaGuardRequestBytes, "safety_percent", &percent); err != nil {
		badJSON(w, r, err)
		return
	}
	if percent == nil || *percent < 0 || *percent > store.MaxUpstreamQuotaGuardPercent {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_quota_guard_percent", "剩余用量保险额度必须是 0 至 50 的整数百分比")
		return
	}
	settings, err := s.store.SetUpstreamQuotaGuardSettings(r.Context(), store.SetUpstreamQuotaGuardSettingsParams{
		SafetyPercent: int(*percent), At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set upstream quota guard settings", err)
		return
	}
	s.upstreamQuotaGuardScheduler().forget()
	writeJSON(w, http.StatusOK, map[string]any{
		"safety_percent": settings.SafetyPercent, "revision": settings.Revision, "updated_at": settings.UpdatedAt,
	})
}
