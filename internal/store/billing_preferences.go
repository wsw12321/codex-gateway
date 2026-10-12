package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type BillingCashPreferences struct {
	Priority      int  `json:"priority"`
	ChargeEnabled bool `json:"charge_enabled"`
}
type SetBillingSubscriptionPreferencesParams struct {
	BillingWriteParams
	UserID, SubscriptionID string
	Priority               int
	ChargeEnabled          bool
}
type SetBillingCashPreferencesParams struct {
	BillingWriteParams
	UserID        string
	Priority      int
	ChargeEnabled bool
}
type DeleteBillingPlanParams struct {
	BillingWriteParams
	PlanID  string
	Version int64
}

type billingPreferencesSnapshot struct {
	Subscription *BillingSubscriptionState `json:"subscription,omitempty"`
	Cash         *BillingCashPreferences   `json:"cash_preferences,omitempty"`
}

func (s *Store) SetBillingSubscriptionPreferences(ctx context.Context, p SetBillingSubscriptionPreferencesParams) (BillingSubscriptionState, error) {
	if p.SubscriptionID == "" {
		return BillingSubscriptionState{}, fmt.Errorf("%w: subscription ID required", ErrInvalid)
	}
	snap, err := s.setBillingPreferences(ctx, p.BillingWriteParams, p.UserID, p.SubscriptionID, p.Priority, p.ChargeEnabled)
	if err != nil {
		return BillingSubscriptionState{}, err
	}
	return *snap.Subscription, nil
}
func (s *Store) SetBillingCashPreferences(ctx context.Context, p SetBillingCashPreferencesParams) (BillingCashPreferences, error) {
	snap, err := s.setBillingPreferences(ctx, p.BillingWriteParams, p.UserID, "", p.Priority, p.ChargeEnabled)
	if err != nil {
		return BillingCashPreferences{}, err
	}
	return *snap.Cash, nil
}
func (s *Store) setBillingPreferences(ctx context.Context, p BillingWriteParams, userID, subscriptionID string, priority int, enabled bool) (billingPreferencesSnapshot, error) {
	var result billingPreferencesSnapshot
	if err := validateBillingWrite(p); err != nil {
		return result, err
	}
	if userID == "" || p.ActorUserID != userID || priority < 1 || priority > 100 {
		return result, fmt.Errorf("%w: preferences require their owner and priority between 1 and 100", ErrInvalid)
	}
	p.At = normalizedBillingTime(p.At, s.now)
	p.Reason = strings.TrimSpace(p.Reason)
	op := "cash_preferences"
	if subscriptionID != "" {
		op = "subscription_preferences"
	}
	fingerprint := billingOperationFingerprint(op, userID, subscriptionID, strconv.Itoa(priority), strconv.FormatBool(enabled), p.Reason)
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, userID); err != nil {
			return err
		}
		created, replayID, err := claimBillingOperationTx(ctx, tx, p, op, userID, fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err := replayBillingLedgerTx(ctx, tx, *replayID)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(entry.TransactionSnapshot, &result); err != nil {
				return err
			}
			if (subscriptionID == "" && result.Cash == nil) || (subscriptionID != "" && result.Subscription == nil) {
				return fmt.Errorf("%w: missing preferences snapshot", ErrConflict)
			}
			return nil
		}
		if subscriptionID == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET cash_priority=$2,cash_source_disabled=$3,updated_at=$4 WHERE user_id=$1`, userID, priority, !enabled, p.At); err != nil {
				return mapDBError("save cash preferences", err)
			}
			result.Cash = &BillingCashPreferences{Priority: priority, ChargeEnabled: enabled}
		} else {
			if _, err := rollBillingSubscriptionsTx(ctx, tx, userID, p.At); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET priority=$3,charge_enabled=$4,updated_at=$5 WHERE user_id=$1 AND id=$2 AND enabled`, userID, subscriptionID, priority, enabled, p.At)
			if err != nil {
				return mapDBError("save subscription preferences", err)
			}
			if err := requireAffected("save subscription preferences", res); err != nil {
				return err
			}
			value, err := readBillingSubscriptionStateTx(ctx, tx, userID, subscriptionID, p.At)
			if err != nil {
				return err
			}
			result.Subscription = &value
		}
		snapshot, err := json.Marshal(result)
		if err != nil {
			return err
		}
		var ledgerID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries(user_id,operation_id,entry_type,reason,actor_user_id,created_at,transaction_snapshot) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb) RETURNING id`, userID, p.OperationID, op, p.Reason, p.ActorUserID, p.At, snapshot).Scan(&ledgerID); err != nil {
			return mapDBError("record preferences ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, p.OperationID, ledgerID); err != nil {
			return err
		}
		target := userID
		if subscriptionID != "" {
			target = subscriptionID
		}
		return appendBillingAuditTx(ctx, tx, p, "billing."+op, "billing_preferences", target, map[string]any{"priority": priority, "charge_enabled": enabled, "operation_id": p.OperationID})
	})
	return result, err
}

func (s *Store) DeleteBillingPlan(ctx context.Context, p DeleteBillingPlanParams) (BillingPlan, error) {
	var result BillingPlan
	if err := validateBillingWrite(p.BillingWriteParams); err != nil {
		return result, err
	}
	if p.PlanID == "" || p.Version < 1 {
		return result, fmt.Errorf("%w: plan and version required", ErrInvalid)
	}
	p.At = normalizedBillingTime(p.At, s.now)
	p.Reason = strings.TrimSpace(p.Reason)
	fingerprint := billingOperationFingerprint("plan_delete", p.ActorUserID, p.PlanID, strconv.FormatInt(p.Version, 10), p.Reason)
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		created, replayID, err := claimBillingOperationTx(ctx, tx, p.BillingWriteParams, "plan_delete", "", fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err := replayBillingLedgerTx(ctx, tx, *replayID)
			if err != nil {
				return err
			}
			var snap billingTransactionSnapshot
			if err := json.Unmarshal(entry.TransactionSnapshot, &snap); err != nil {
				return err
			}
			if snap.Plan == nil {
				return fmt.Errorf("%w: missing deleted plan snapshot", ErrConflict)
			}
			result = *snap.Plan
			return nil
		}
		current, err := scanBillingPlan(tx.QueryRowContext(ctx, `SELECT `+billingPlanColumns+` FROM billing_plans WHERE id=$1 FOR UPDATE`, p.PlanID))
		if err != nil {
			return mapDBError("lock plan for deletion", err)
		}
		if current.DeletedAt != nil || current.Version != p.Version {
			return ErrBillingPlanChanged
		}
		result, err = scanBillingPlan(tx.QueryRowContext(ctx, `UPDATE billing_plans SET active=false,deleted_at=$2,updated_at=$2,version=version+1,updated_by_user_id=$3 WHERE id=$1 RETURNING `+billingPlanColumns, p.PlanID, p.At, p.ActorUserID))
		if err != nil {
			return mapDBError("delete billing plan", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET plan_id=NULL,plan_version=NULL,config_version=nextval('billing_subscription_config_versions') WHERE plan_id=$1`, p.PlanID); err != nil {
			return mapDBError("detach deleted plan subscriptions", err)
		}
		snapshot, err := json.Marshal(billingTransactionSnapshot{Plan: &result})
		if err != nil {
			return err
		}
		var ledgerID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries(operation_id,entry_type,reason,actor_user_id,created_at,transaction_snapshot) VALUES($1,'plan_delete',$2,$3,$4,$5::jsonb) RETURNING id`, p.OperationID, p.Reason, p.ActorUserID, p.At, snapshot).Scan(&ledgerID); err != nil {
			return mapDBError("record deleted plan ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, p.OperationID, ledgerID); err != nil {
			return err
		}
		return appendBillingAuditTx(ctx, tx, p.BillingWriteParams, "billing.plan_delete", "billing_plan", p.PlanID, map[string]any{"operation_id": p.OperationID, "plan": billingPlanAuditMetadata(result)})
	})
	return result, err
}
