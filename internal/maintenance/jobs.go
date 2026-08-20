// Package maintenance performs idempotent metadata aggregation and retention.
// It never reads request or response content because those values never enter
// the database schema.
package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

type Runner struct {
	Store    *store.Store
	Logger   *slog.Logger
	Timezone string
	Now      func() time.Time
}

func (r Runner) Run(ctx context.Context) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	r.runOnce(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r Runner) RunOnce(ctx context.Context) { r.runOnce(ctx) }

func (r Runner) runOnce(ctx context.Context) {
	now := r.Now().UTC()
	location, err := time.LoadLocation(r.Timezone)
	if err != nil {
		r.Logger.Error("invalid aggregation timezone")
		return
	}
	// Terminal usage is the durable source of truth for both quota and billing.
	// Repair it before any aggregation, stale-release or retention work.
	if _, err := r.Store.RetryUnsettledRequests(ctx, 1000); err != nil {
		r.Logger.Warn("request billing settlement recovery failed")
	}
	if _, err := r.Store.ExpireBillingSubscriptions(ctx, now, 1000); err != nil {
		r.Logger.Warn("finite subscription expiry convergence failed")
	}
	local := now.In(location)
	yesterday := time.Date(local.Year(), local.Month(), local.Day()-1, 0, 0, 0, 0, location)
	if err := r.Store.AggregateUsageDay(ctx, yesterday, r.Timezone); err != nil {
		r.Logger.Warn("daily usage aggregation failed", "day", yesterday.Format("2006-01-02"))
	}
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
	for _, value := range []time.Time{month, month.AddDate(0, -1, 0)} {
		if err := r.Store.AggregateUsageMonth(ctx, value, r.Timezone); err != nil {
			r.Logger.Warn("monthly usage aggregation failed", "month", value.Format("2006-01"))
		}
	}
	if _, err := r.Store.ReleaseStaleQuotaReservations(ctx, now.Add(-35*time.Minute), 1000); err != nil {
		r.Logger.Warn("stale quota cleanup failed")
	}
	if _, err := r.Store.DeleteUsageRequestsBefore(ctx, now.Add(-90*24*time.Hour), 10_000); err != nil {
		r.Logger.Warn("usage retention cleanup failed")
	}
	if _, err := r.Store.DeleteAuditEventsBefore(ctx, now.Add(-365*24*time.Hour), 10_000); err != nil {
		r.Logger.Warn("audit retention cleanup failed")
	}
	if err := r.Store.DeleteQuotaStateBefore(ctx, now.Add(-92*24*time.Hour)); err != nil {
		r.Logger.Warn("quota retention cleanup failed")
	}
	// Expired browser identity artifacts contain hashes and metadata only. Keep
	// a short grace period for incident review, then remove them in bounded SQL.
	_, _ = r.Store.DB().ExecContext(ctx, `
		DELETE FROM sessions WHERE
			(COALESCE(revoked_at, absolute_expires_at) < $1)
		`, now.Add(-7*24*time.Hour))
	_, _ = r.Store.DB().ExecContext(ctx, `
		DELETE FROM invitations WHERE expires_at < $1
		`, now.Add(-30*24*time.Hour))
}
