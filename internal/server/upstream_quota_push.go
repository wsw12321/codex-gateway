package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	upstreamQuotaPushRequestBytes = 16 << 10
	// upstreamQuotaPushSkew bounds how far in the future a reported
	// observation may lie; CPA and the Gateway share a host clock.
	upstreamQuotaPushSkew = 5 * time.Minute
)

// recordObservedUpstreamQuota receives one account's quota from the CPA
// plugin, which reads it from the rate-limit headers of a response CPA
// received and reports it only when it changed. The reading goes through the
// quota guard like an Owner query: the rule applies at once to an automatic
// state, a manual state keeps until the next automatic check, and a reading
// that is not newer than the stored one is ignored. Each report is metadata
// only; the guard audits the state changes it causes.
func (s *Server) recordObservedUpstreamQuota(w http.ResponseWriter, r *http.Request) {
	if !s.egressInternalAuth(w, r) {
		return
	}
	invalid := func() {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_quota_report", "额度上报无效")
	}
	if !strictJSONRequest(r) || r.ContentLength > upstreamQuotaPushRequestBytes {
		invalid()
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, upstreamQuotaPushRequestBytes+1))
	if err != nil || len(body) > upstreamQuotaPushRequestBytes || rejectDuplicateJSON(body) != nil {
		invalid()
		return
	}
	now := time.Now().UTC()
	provider := attemptProvider(r)
	var accountID string
	var usage accountUsage
	switch provider {
	case store.UpstreamProviderCodex:
		var wire struct {
			AccountID string          `json:"account_id"`
			Quota     json.RawMessage `json:"quota"`
		}
		if decodeStrictBody(body, &wire) != nil || !validUpstreamAccountID(wire.AccountID) {
			invalid()
			return
		}
		quota, err := gatewayproxy.DecodeObservedUpstreamQuota(wire.Quota, now)
		if err != nil {
			invalid()
			return
		}
		accountID, usage = wire.AccountID, codexAccountUsage(quota, now)
	case store.UpstreamProviderAnthropic:
		var wire struct {
			AccountID  string               `json:"account_id"`
			ObservedAt *time.Time           `json:"observed_at"`
			Windows    *[]cpaObservedWindow `json:"windows"`
		}
		if decodeStrictBody(body, &wire) != nil || !validUpstreamAccountID(wire.AccountID) || wire.ObservedAt == nil ||
			wire.Windows == nil || len(*wire.Windows) > 2 || !validObservedQuotaTime(*wire.ObservedAt, now) {
			invalid()
			return
		}
		seen := map[string]bool{}
		for _, window := range *wire.Windows {
			if seen[window.Window] {
				invalid()
				return
			}
			seen[window.Window] = true
		}
		observedAt := wire.ObservedAt.UTC()
		var ok bool
		if usage, ok = anthropicAccountUsage(&observedAt, nil, *wire.Windows, now); !ok {
			invalid()
			return
		}
		accountID = wire.AccountID
	default:
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "not_found", "接口不存在")
		return
	}
	observation := upstreamQuotaObservation(usage, now)
	if observation.FiveHour == nil && observation.SevenDay == nil {
		// Nothing the guard reads, such as a Codex limit without its windows.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.quotaGuardRepo == nil {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_quota_guard_unavailable", "额度守卫暂不可用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), upstreamQuotaGuardRecordTimeout)
	defer cancel()
	state, err := s.quotaGuardRepo.RecordUpstreamQuotaObservation(ctx, store.RecordUpstreamQuotaObservationParams{
		Provider: provider, AccountID: accountID, Observation: observation, RequireNewer: true, At: now,
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	case errors.Is(err, store.ErrInvalid):
		invalid()
		return
	case err != nil:
		internalError(s, w, r, "record observed upstream quota", err)
		return
	}
	s.upstreamQuotaGuardScheduler().reschedule(accountID, state.NextAutomaticCheck())
	w.WriteHeader(http.StatusNoContent)
}

func validObservedQuotaTime(observed, now time.Time) bool {
	return observed.Unix() >= minObservedQuotaUnix && !observed.After(now.Add(upstreamQuotaPushSkew))
}

// minObservedQuotaUnix is 2000-01-01T00:00:00Z, the same floor the quota
// envelope's observation time is held to.
const minObservedQuotaUnix = 946684800
