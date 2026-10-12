package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// BillingFundingSource is an immutable admission-time position. RemainingUSD
// determines ordering only; settlement consumes the then-remaining funds from
// this exact period or cash-lot cutoff under the account lock.
type BillingFundingSource struct {
	SourceType     string `json:"source_type"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	PeriodID       string `json:"period_id,omitempty"`
	CashLotCutoff  int64  `json:"cash_lot_cutoff,omitempty"`
	Priority       int    `json:"priority"`
	PeriodDays     int    `json:"period_days,omitempty"`
	RemainingUSD   string `json:"remaining_usd"`
}

func sortBillingFundingSources(sources []BillingFundingSource) error {
	// Validate before sorting so a malformed amount cannot silently affect order.
	for _, source := range sources {
		if source.Priority < 1 || source.Priority > 100 {
			return fmt.Errorf("%w: invalid funding priority", ErrInvalid)
		}
		if _, err := billingRat(source.RemainingUSD); err != nil {
			return err
		}
	}
	sort.SliceStable(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.SourceType != b.SourceType {
			return a.SourceType == "subscription"
		}
		if a.PeriodDays != b.PeriodDays {
			return a.PeriodDays > b.PeriodDays
		}
		av, _ := billingRat(a.RemainingUSD)
		bv, _ := billingRat(b.RemainingUSD)
		if cmp := av.Cmp(bv); cmp != 0 {
			return cmp > 0
		}
		return a.SubscriptionID < b.SubscriptionID
	})
	return nil
}

func snapshotBillingFundingTx(ctx context.Context, tx *sql.Tx, userID, model string, at time.Time, balance string, cashPriority int, cashCutoff *int64) ([]BillingFundingSource, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.id,p.id,s.priority,s.period_days,p.remaining_usd::text
		FROM billing_subscriptions s JOIN billing_subscription_periods p ON p.id=s.current_period_id
		WHERE s.user_id=$1 AND s.enabled AND s.charge_enabled AND s.models_initialized
		AND s.model_ids ? $3 AND p.closed_at IS NULL AND p.starts_at<=$2 AND p.ends_at>$2
		AND p.remaining_usd>0 ORDER BY s.id FOR UPDATE OF s,p`, userID, at, model)
	if err != nil {
		return nil, mapDBError("read ordered billing sources", err)
	}
	sources := make([]BillingFundingSource, 0)
	for rows.Next() {
		source := BillingFundingSource{SourceType: "subscription"}
		if err := rows.Scan(&source.SubscriptionID, &source.PeriodID, &source.Priority, &source.PeriodDays, &source.RemainingUSD); err != nil {
			_ = rows.Close()
			return nil, mapDBError("scan ordered billing source", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cashCutoff != nil {
		sources = append(sources, BillingFundingSource{SourceType: "cash", CashLotCutoff: *cashCutoff, Priority: cashPriority, RemainingUSD: balance})
	}
	if err := sortBillingFundingSources(sources); err != nil {
		return nil, err
	}
	return sources, nil
}

func billingFundingRetryTx(ctx context.Context, tx *sql.Tx, userID, model string, at time.Time) (time.Duration, error) {
	var renewal sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT min(candidate) FROM (
		SELECT CASE WHEN e.starts_at>$2 THEN e.starts_at
			WHEN p.ends_at>$2 AND (e.ends_at IS NULL OR p.ends_at<e.ends_at) THEN p.ends_at END AS candidate
		FROM billing_subscriptions s
		JOIN billing_subscription_entitlements e ON e.subscription_id=s.id
		LEFT JOIN billing_subscription_periods p ON p.id=s.current_period_id
		WHERE s.user_id=$1 AND s.enabled AND s.charge_enabled AND s.models_initialized
		AND e.superseded_at IS NULL AND e.model_ids ? $3 AND e.allowance_usd>0
		AND (e.ends_at IS NULL OR e.ends_at>$2)
	) candidates`, userID, at, model).Scan(&renewal)
	if err != nil {
		return 0, mapDBError("read model-scoped billing retry time", err)
	}
	if renewal.Valid && renewal.Time.After(at) {
		return renewal.Time.Sub(at), nil
	}
	return 0, nil
}

func recordBillingFundingSourcesTx(ctx context.Context, tx *sql.Tx, reservation BillingReservation) error {
	for position, source := range reservation.OrderedFundingSources {
		var period, cutoff any
		if source.SourceType == "subscription" {
			period = source.PeriodID
		} else {
			cutoff = source.CashLotCutoff
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_reservation_sources
			(request_id,position,user_id,subscription_period_id,cash_lot_cutoff)
			VALUES ($1,$2,$3,$4,$5)`, reservation.RequestID, position+1, reservation.UserID, period, cutoff); err != nil {
			return mapDBError("record billing funding reference", err)
		}
	}
	return nil
}

func billingSettlementSources(reservation BillingReservation) ([]BillingFundingSource, error) {
	if reservation.FundingRuleVersion == 3 {
		seen := make(map[string]bool)
		for _, source := range reservation.OrderedFundingSources {
			key := source.PeriodID
			switch source.SourceType {
			case "subscription":
				if source.PeriodID == "" || source.SubscriptionID == "" || source.CashLotCutoff != 0 {
					return nil, fmt.Errorf("invalid subscription funding snapshot: %w", ErrConflict)
				}
			case "cash":
				key = "cash"
				if source.CashLotCutoff < 1 || source.PeriodID != "" || source.SubscriptionID != "" {
					return nil, fmt.Errorf("invalid cash funding snapshot: %w", ErrConflict)
				}
			default:
				return nil, fmt.Errorf("unknown funding source: %w", ErrConflict)
			}
			if seen[key] {
				return nil, fmt.Errorf("duplicate funding source: %w", ErrConflict)
			}
			seen[key] = true
		}
		return reservation.OrderedFundingSources, nil
	}
	if reservation.FundingRuleVersion != 1 && reservation.FundingRuleVersion != 2 {
		return nil, fmt.Errorf("unknown funding rule version: %w", ErrConflict)
	}
	sources := make([]BillingFundingSource, 0, 4)
	for _, period := range []struct {
		tier string
		id   *string
	}{{BillingTierDay, reservation.DayPeriodID}, {BillingTierWeek, reservation.WeekPeriodID}, {BillingTierMonth, reservation.MonthPeriodID}} {
		if period.id != nil {
			sources = append(sources, BillingFundingSource{SourceType: period.tier, PeriodID: *period.id})
		}
	}
	if reservation.CashLotCutoff != nil {
		sources = append(sources, BillingFundingSource{SourceType: "cash", CashLotCutoff: *reservation.CashLotCutoff})
	}
	return sources, nil
}

func decodeBillingFundingSources(body []byte) ([]BillingFundingSource, error) {
	var sources []BillingFundingSource
	if err := json.Unmarshal(body, &sources); err != nil {
		return nil, fmt.Errorf("decode billing funding snapshot: %w", err)
	}
	return sources, nil
}
