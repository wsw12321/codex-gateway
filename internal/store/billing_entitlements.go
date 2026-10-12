package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// BillingSubscriptionEntitlement is a purchased immutable configuration. A
// reopen retires the configuration without deleting history or period references.
type BillingSubscriptionEntitlement struct {
	ID                string       `json:"id"`
	StartsAt          time.Time    `json:"starts_at"`
	EndsAt            *time.Time   `json:"ends_at"`
	Tier              string       `json:"tier"`
	PeriodDays        int          `json:"period_days"`
	AllowanceUSD      string       `json:"allowance_usd"`
	ModelIDs          []string     `json:"model_ids"`
	PeriodCount       int          `json:"period_count"`
	FirstPeriodNumber int          `json:"first_period_number"`
	Plan              *BillingPlan `json:"plan"`
}

func normalizeBillingPeriodDays(tier string, days int) (int, error) {
	expected := 0
	switch tier {
	case BillingTierDay:
		expected = 1
	case BillingTierWeek:
		expected = 7
	case BillingTierMonth:
		expected = 31
	case BillingTierCustom:
		if days < 1 || days > 3650 {
			return 0, fmt.Errorf("%w: custom period days must be between 1 and 3650", ErrInvalid)
		}
		return days, nil
	default:
		return 0, fmt.Errorf("%w: invalid subscription tier", ErrInvalid)
	}
	if days != 0 && days != expected {
		return 0, fmt.Errorf("%w: period days do not match tier", ErrInvalid)
	}
	return expected, nil
}
func normalizeBillingModels(models []string) ([]string, error) {
	values, err := normalizeModelAccessCatalog(models)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: at least one model must be selected", ErrInvalid)
	}
	return values, nil
}
func validateBillingModelsTx(ctx context.Context, tx *sql.Tx, models []string) error {
	encoded, err := json.Marshal(models)
	if err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM jsonb_array_elements_text($1::jsonb) m(model) WHERE NOT EXISTS(SELECT 1 FROM model_access_defaults d WHERE d.model=m.model AND d.catalog_active))`, encoded).Scan(&valid); err != nil {
		return mapDBError("validate subscription models", err)
	}
	if !valid {
		return fmt.Errorf("%w: model is not in the manageable catalog", ErrInvalid)
	}
	return nil
}
func billingExpiryDays(start time.Time, days, count int) (time.Time, error) {
	start = start.UTC()
	if days < 1 || days > 3650 || count < 1 || count > math.MaxInt32 || start.Year() < 1 || start.Year() > 9999 {
		return time.Time{}, fmt.Errorf("%w: subscription duration exceeds supported range", ErrInvalid)
	}
	seconds := int64(days) * int64(count) * 86400
	// Unix arithmetic avoids both Duration and machine-sized calendar overflow.
	end := time.Unix(start.Unix()+seconds, int64(start.Nanosecond())).UTC()
	if end.Year() > 9999 || !end.After(start) {
		return time.Time{}, fmt.Errorf("%w: subscription expiry exceeds supported range", ErrInvalid)
	}
	return end, nil
}

// InitializeBillingSubscriptionModels must run after the authoritative model
// catalog is synchronized and before admission. The marker is set atomically,
// so future catalog additions never expand a saved subscription scope.
func (s *Store) InitializeBillingSubscriptionModels(ctx context.Context) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE model_access_defaults IN SHARE MODE`); err != nil {
			return mapDBError("lock billing model initialization catalog", err)
		}
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM billing_subscriptions WHERE NOT models_initialized)`).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return nil
		}
		var models []byte
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(model ORDER BY model),'[]') FROM model_access_defaults WHERE catalog_active`).Scan(&models); err != nil {
			return err
		}
		var values []string
		if err := json.Unmarshal(models, &values); err != nil {
			return err
		}
		if len(values) == 0 {
			return fmt.Errorf("%w: cannot initialize subscriptions from an empty model catalog", ErrInvalid)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_entitlements e SET model_ids=$1::jsonb FROM billing_subscriptions s WHERE e.subscription_id=s.id AND NOT s.models_initialized`, models); err != nil {
			return mapDBError("initialize legacy entitlement models", err)
		}
		_, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET model_ids=$1::jsonb,models_initialized=true WHERE NOT models_initialized`, models)
		return mapDBError("initialize legacy subscription models", err)
	})
}

func insertBillingEntitlementTx(ctx context.Context, tx *sql.Tx, userID, subscriptionID string, start time.Time, tier string, days, count, first int, allowance string, models []string, plan *BillingPlan, at time.Time) (string, *time.Time, error) {
	var end *time.Time
	if count > 0 {
		value, err := billingExpiryDays(start, days, count)
		if err != nil {
			return "", nil, err
		}
		end = &value
	}
	modelJSON, err := json.Marshal(models)
	if err != nil {
		return "", nil, err
	}
	var planJSON any
	if plan != nil {
		value, err := json.Marshal(plan)
		if err != nil {
			return "", nil, err
		}
		planJSON = value
	}
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO billing_subscription_entitlements(subscription_id,user_id,starts_at,ends_at,tier,period_days,allowance_usd,model_ids,period_count,first_period_number,plan_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::numeric,$8::jsonb,$9,$10,$11::jsonb,$12) RETURNING id`, subscriptionID, userID, start, timeOrNil(end), tier, days, allowance, modelJSON, count, first, planJSON, at).Scan(&id)
	return id, end, mapDBError("create subscription entitlement", err)
}

// rollBillingEntitlementsTx locates the active segment and computes its current
// period directly; dormant subscriptions cost one lookup rather than one loop
// per missed period. Caller holds the account lock.
func rollBillingEntitlementsTx(ctx context.Context, tx *sql.Tx, userID string, at time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.id,p.id,p.ends_at,s.expires_at FROM billing_subscriptions s JOIN billing_subscription_periods p ON p.id=s.current_period_id WHERE s.user_id=$1 AND s.enabled AND p.ends_at<=$2 ORDER BY s.id FOR UPDATE OF s,p`, userID, at)
	if err != nil {
		return 0, mapDBError("lock subscription periods", err)
	}
	type stale struct {
		id, periodID string
		end          time.Time
		expiry       sql.NullTime
	}
	var values []stale
	for rows.Next() {
		var value stale
		if err := rows.Scan(&value.id, &value.periodID, &value.end, &value.expiry); err != nil {
			_ = rows.Close()
			return 0, err
		}
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	disabled := 0
	for _, value := range values {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_periods SET closed_at=ends_at,close_reason='expired' WHERE id=$1 AND closed_at IS NULL`, value.periodID); err != nil {
			return disabled, mapDBError("close elapsed billing period", err)
		}
		if value.expiry.Valid && !value.expiry.Time.After(at) {
			if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET enabled=false,disabled_at=expires_at,updated_at=$2 WHERE id=$1`, value.id, at); err != nil {
				return disabled, err
			}
			disabled++
			continue
		}
		var entitlementID, tier, allowance string
		var start time.Time
		var end sql.NullTime
		var days, count, first, totalCount int
		var models []byte
		if err := tx.QueryRowContext(ctx, `SELECT e.id,e.tier,e.allowance_usd::text,e.starts_at,e.ends_at,e.period_days,e.period_count,e.first_period_number,e.model_ids,s.period_count FROM billing_subscription_entitlements e JOIN billing_subscriptions s ON s.id=e.subscription_id WHERE e.subscription_id=$1 AND e.superseded_at IS NULL AND e.starts_at<=$2 AND (e.ends_at IS NULL OR e.ends_at>$2) ORDER BY e.starts_at DESC LIMIT 1`, value.id, at).Scan(&entitlementID, &tier, &allowance, &start, &end, &days, &count, &first, &models, &totalCount); err != nil {
			return disabled, mapDBError("locate current subscription entitlement", err)
		}
		seconds := at.Unix() - start.Unix()
		if at.Nanosecond() < start.Nanosecond() {
			seconds--
		}
		elapsed := seconds / (int64(days) * 86400)
		if elapsed > math.MaxInt32-int64(first) {
			return disabled, fmt.Errorf("%w: subscription period number overflow", ErrInvalid)
		}
		periodNumber := first + int(elapsed)
		if elapsed > 0 {
			start, err = billingExpiryDays(start, days, int(elapsed))
			if err != nil {
				return disabled, err
			}
		}
		periodEnd, err := billingExpiryDays(start, days, 1)
		if err != nil {
			return disabled, err
		}
		id, err := newUUID()
		if err != nil {
			return disabled, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_subscription_periods(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,period_number,period_count,created_at,entitlement_id,period_days) VALUES($1,$2,$3,$4,$5,$6,$7::numeric,$7::numeric,$8,$9,$10,$11,$12)`, id, value.id, userID, tier, start, periodEnd, allowance, periodNumber, totalCount, at, entitlementID, days); err != nil {
			return disabled, mapDBError("create current entitlement period", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET current_period_id=$2,current_period_number=$3,tier=$4,period_days=$5,allowance_usd=$6::numeric,model_ids=$7::jsonb,updated_at=$8 WHERE id=$1`, value.id, id, periodNumber, tier, days, allowance, models, at); err != nil {
			return disabled, mapDBError("activate entitlement period", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_ledger_entries(user_id,entry_type,amount_usd,subscription_tier,subscription_period_id,reason,created_at) VALUES($1,'subscription_renewal',$2::numeric,$3,$4,'automatic renewal',$5)`, userID, allowance, tier, id, at); err != nil {
			return disabled, mapDBError("record current entitlement period", err)
		}
	}
	return disabled, nil
}
