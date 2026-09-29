package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	decimal "github.com/wsw/codex-gateway/internal/billing"
)

// ErrBillingPlanChanged requires a fresh quote and explicit confirmation.
var ErrBillingPlanChanged = fmt.Errorf("billing plan or subscription configuration changed: %w", ErrConflict)

type BillingPlan struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	PriceUSD       string    `json:"price_usd"`
	Tier           string    `json:"tier"`
	AllowanceUSD   string    `json:"allowance_usd"`
	MinPeriodCount int       `json:"min_period_count"`
	Active         bool      `json:"active"`
	Version        int64     `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type CreateBillingPlanParams struct {
	BillingWriteParams
	Name, PriceUSD, Tier, AllowanceUSD string
	MinPeriodCount                     int
	Active                             bool
}

type UpdateBillingPlanParams struct {
	BillingWriteParams
	PlanID                             string
	Version                            int64
	Name, PriceUSD, Tier, AllowanceUSD string
	MinPeriodCount                     int
	Active                             bool
}

type PurchaseBillingPlanParams struct {
	BillingWriteParams
	UserID, PlanID                         string
	PlanVersion, SubscriptionConfigVersion int64
	PeriodCount                            int
}

type RenewBillingSubscriptionParams struct {
	BillingWriteParams
	UserID, Tier                           string
	PlanVersion, SubscriptionConfigVersion int64
	PeriodCount                            int
}

type BillingPlanTransaction struct {
	Plan         BillingPlan              `json:"plan"`
	PeriodCount  int                      `json:"period_count"`
	TotalUSD     string                   `json:"total_usd"`
	BalanceUSD   string                   `json:"balance_usd"`
	Subscription BillingSubscriptionState `json:"subscription"`
	Ledger       BillingLedgerEntry       `json:"ledger"`
}

// This is stored alongside the ledger so replay never consults mutable plans,
// balances, subscriptions, or spent period snapshots.
type billingTransactionSnapshot struct {
	Plan         *BillingPlan              `json:"plan,omitempty"`
	PeriodCount  int                       `json:"period_count,omitempty"`
	TotalUSD     string                    `json:"total_usd,omitempty"`
	BalanceUSD   string                    `json:"balance_usd,omitempty"`
	Subscription *BillingSubscriptionState `json:"subscription,omitempty"`
}

func billingPlanAuditMetadata(plan BillingPlan) map[string]any {
	return map[string]any{"id": plan.ID, "name": plan.Name, "price_usd": plan.PriceUSD,
		"tier": plan.Tier, "allowance_usd": plan.AllowanceUSD, "min_period_count": plan.MinPeriodCount,
		"active": plan.Active, "version": plan.Version}
}

const billingPlanColumns = `id,name,price_usd::text,tier,allowance_usd::text,min_period_count,active,version,created_at,updated_at`

func scanBillingPlan(row rowScanner) (BillingPlan, error) {
	var p BillingPlan
	err := row.Scan(&p.ID, &p.Name, &p.PriceUSD, &p.Tier, &p.AllowanceUSD, &p.MinPeriodCount, &p.Active, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (s *Store) ListBillingPlans(ctx context.Context, includeInactive bool) ([]BillingPlan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+billingPlanColumns+` FROM billing_plans WHERE active OR $1 ORDER BY created_at,id`, includeInactive)
	if err != nil {
		return nil, mapDBError("list billing plans", err)
	}
	defer rows.Close()
	plans := make([]BillingPlan, 0)
	for rows.Next() {
		p, err := scanBillingPlan(rows)
		if err != nil {
			return nil, mapDBError("scan billing plan", err)
		}
		plans = append(plans, p)
	}
	return plans, mapDBError("iterate billing plans", rows.Err())
}

func normalizeBillingPlan(p *BillingPlan) error {
	p.Name = strings.TrimSpace(p.Name)
	if len([]rune(p.Name)) < 1 || len([]rune(p.Name)) > 100 {
		return fmt.Errorf("%w: plan name must contain 1 to 100 characters", ErrInvalid)
	}
	if _, err := billingPeriodDuration(p.Tier); err != nil {
		return err
	}
	if p.MinPeriodCount < 1 || p.MinPeriodCount > 99 {
		return fmt.Errorf("%w: minimum purchase count must be between 1 and 99", ErrInvalid)
	}
	var err error
	p.PriceUSD, err = decimal.ParseRate(p.PriceUSD)
	if err != nil {
		return fmt.Errorf("%w: invalid plan price", ErrInvalid)
	}
	p.AllowanceUSD, err = decimal.ParseRate(p.AllowanceUSD)
	if err != nil {
		return fmt.Errorf("%w: invalid plan allowance", ErrInvalid)
	}
	return nil
}

func (s *Store) CreateBillingPlan(ctx context.Context, params CreateBillingPlanParams) (BillingPlan, error) {
	if params.MinPeriodCount == 0 {
		params.MinPeriodCount = 1
	}
	return s.writeBillingPlan(ctx, params.BillingWriteParams, BillingPlan{Name: params.Name, PriceUSD: params.PriceUSD, Tier: params.Tier, AllowanceUSD: params.AllowanceUSD, MinPeriodCount: params.MinPeriodCount, Active: params.Active}, true)
}

func (s *Store) UpdateBillingPlan(ctx context.Context, params UpdateBillingPlanParams) (BillingPlan, error) {
	if params.PlanID == "" || params.Version < 1 {
		return BillingPlan{}, fmt.Errorf("%w: plan and version are required", ErrInvalid)
	}
	return s.writeBillingPlan(ctx, params.BillingWriteParams, BillingPlan{ID: params.PlanID, Version: params.Version, Name: params.Name, PriceUSD: params.PriceUSD, Tier: params.Tier, AllowanceUSD: params.AllowanceUSD, MinPeriodCount: params.MinPeriodCount, Active: params.Active}, false)
}

func (s *Store) writeBillingPlan(ctx context.Context, params BillingWriteParams, plan BillingPlan, create bool) (BillingPlan, error) {
	var result BillingPlan
	if err := validateBillingWrite(params); err != nil {
		return result, err
	}
	if err := normalizeBillingPlan(&plan); err != nil {
		return result, err
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	opType := "plan_update"
	if create {
		opType = "plan_create"
	}
	fingerprint := billingOperationFingerprint(opType, params.ActorUserID, params.Reason, plan.ID, strconv.FormatInt(plan.Version, 10), plan.Name, plan.PriceUSD, plan.Tier, plan.AllowanceUSD, strconv.Itoa(plan.MinPeriodCount), strconv.FormatBool(plan.Active))
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		created, replayID, err := claimBillingOperationTx(ctx, tx, params, opType, "", fingerprint)
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
				return fmt.Errorf("read plan operation snapshot: %w", err)
			}
			if snap.Plan == nil {
				return fmt.Errorf("plan operation snapshot missing: %w", ErrConflict)
			}
			result = *snap.Plan
			return nil
		}
		if create {
			result, err = scanBillingPlan(tx.QueryRowContext(ctx, `INSERT INTO billing_plans (name,price_usd,tier,allowance_usd,min_period_count,active,created_at,updated_at,updated_by_user_id) VALUES ($1,$2::numeric,$3,$4::numeric,$5,$6,$7,$7,$8) RETURNING `+billingPlanColumns, plan.Name, plan.PriceUSD, plan.Tier, plan.AllowanceUSD, plan.MinPeriodCount, plan.Active, params.At, params.ActorUserID))
		} else {
			current, readErr := scanBillingPlan(tx.QueryRowContext(ctx, `SELECT `+billingPlanColumns+` FROM billing_plans WHERE id=$1 FOR UPDATE`, plan.ID))
			if readErr != nil {
				return mapDBError("lock billing plan", readErr)
			}
			if current.Version != plan.Version {
				return ErrBillingPlanChanged
			}
			result, err = scanBillingPlan(tx.QueryRowContext(ctx, `UPDATE billing_plans SET
				version=version+CASE WHEN (name,price_usd,tier,allowance_usd,min_period_count,active) IS DISTINCT FROM ($2::text,$3::numeric,$4::text,$5::numeric,$6::integer,$7::boolean) THEN 1 ELSE 0 END,
				name=$2,price_usd=$3::numeric,tier=$4,allowance_usd=$5::numeric,min_period_count=$6,active=$7,updated_at=$8,updated_by_user_id=$9 WHERE id=$1 RETURNING `+billingPlanColumns, plan.ID, plan.Name, plan.PriceUSD, plan.Tier, plan.AllowanceUSD, plan.MinPeriodCount, plan.Active, params.At, params.ActorUserID))
		}
		if err != nil {
			return mapDBError("save billing plan", err)
		}
		snapshot, err := json.Marshal(billingTransactionSnapshot{Plan: &result})
		if err != nil {
			return err
		}
		var ledgerID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries (operation_id,entry_type,reason,actor_user_id,created_at,transaction_snapshot) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, params.OperationID, opType, params.Reason, params.ActorUserID, params.At, snapshot).Scan(&ledgerID); err != nil {
			return mapDBError("record plan ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, ledgerID); err != nil {
			return err
		}
		return appendBillingAuditTx(ctx, tx, params, "billing."+opType, "billing_plan", result.ID, map[string]any{"operation_id": params.OperationID, "plan": billingPlanAuditMetadata(result)})
	})
	return result, err
}

// Fixed 24-hour days are added as seconds, avoiding time.Duration's 292-year
// limit. The public JSON time representation bounds supported dates to 9999.
func billingPlanExpiry(start time.Time, tier string, count int) (time.Time, error) {
	start = start.UTC()
	duration, err := billingPeriodDuration(tier)
	if err != nil {
		return time.Time{}, err
	}
	if count < 1 || count > math.MaxInt32 || start.Year() < 1 || start.Year() > 9999 {
		return time.Time{}, fmt.Errorf("%w: subscription duration exceeds supported range", ErrInvalid)
	}
	seconds := int64(count) * int64(duration/time.Second)
	value := start.AddDate(0, 0, int(seconds/86400))
	if value.Year() > 9999 || !value.After(start) {
		return time.Time{}, fmt.Errorf("%w: subscription expiry exceeds supported range", ErrInvalid)
	}
	return value, nil
}

func advanceBillingPeriod(previousEnd, at time.Time, tier string, current int) (time.Time, time.Time, int, error) {
	duration, err := billingPeriodDuration(tier)
	if err != nil {
		return time.Time{}, time.Time{}, 0, err
	}
	at = at.UTC()
	start := previousEnd.UTC()
	advance := int64(1)
	if previousEnd.IsZero() {
		start = at
	} else {
		seconds := at.Unix() - start.Unix()
		if at.Nanosecond() < start.Nanosecond() {
			seconds--
		}
		if seconds < 0 || at.Year() < 1 || at.Year() > 9999 || start.Year() < 1 || start.Year() > 9999 {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("%w: invalid subscription period time", ErrInvalid)
		}
		elapsed := seconds / int64(duration/time.Second)
		advance += elapsed
		if elapsed > 0 {
			start, err = billingPlanExpiry(start, tier, int(elapsed))
			if err != nil {
				return time.Time{}, time.Time{}, 0, err
			}
		}
	}
	if current < 1 || advance > math.MaxInt32-int64(current) {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("%w: subscription period number exceeds supported range", ErrInvalid)
	}
	end, err := billingPlanExpiry(start, tier, 1)
	return start, end, current + int(advance), err
}

const billingSubscriptionStateColumns = `s.id,s.tier,s.enabled,s.allowance_usd::text,s.period_count,s.current_period_number,s.expires_at,p.id,p.starts_at,p.ends_at,p.remaining_usd::text,s.updated_at,s.config_version,
	CASE WHEN bp.active AND bp.version=s.plan_version THEN jsonb_build_object('id',bp.id,'name',bp.name,'price_usd',bp.price_usd::text,'tier',bp.tier,'allowance_usd',bp.allowance_usd::text,'min_period_count',bp.min_period_count,'active',bp.active,'version',bp.version,'created_at',bp.created_at,'updated_at',bp.updated_at) END`
const billingSubscriptionStateJoins = ` FROM billing_subscriptions s LEFT JOIN billing_subscription_periods p ON p.id=s.current_period_id LEFT JOIN billing_plans bp ON bp.id=s.plan_id `

func readBillingSubscriptionStateTx(ctx context.Context, tx *sql.Tx, userID, tier string, at time.Time) (BillingSubscriptionState, error) {
	value, err := subscriptionStateFromPeriodRow(tx.QueryRowContext(ctx, `SELECT `+billingSubscriptionStateColumns+billingSubscriptionStateJoins+` WHERE s.user_id=$1 AND s.tier=$2`, userID, tier))
	value.CanRenew = value.Plan != nil && value.Enabled && value.ExpiresAt != nil && value.ExpiresAt.After(at)
	return value, mapDBError("read subscription state", err)
}

// Caller holds the account and, for purchases, the catalog shared lock.
func reopenBillingSubscriptionTx(ctx context.Context, tx *sql.Tx, params PutSubscriptionParams, allowance string, plan *BillingPlan) (BillingSubscriptionState, error) {
	var empty BillingSubscriptionState
	duration, err := billingPeriodDuration(params.Tier)
	if err != nil {
		return empty, err
	}
	var expiresAt *time.Time
	if params.PeriodCount > 0 {
		value, err := billingPlanExpiry(params.At, params.Tier, params.PeriodCount)
		if err != nil {
			return empty, err
		}
		expiresAt = &value
	}
	var subscriptionID string
	var oldPeriodID sql.NullString
	var wasEnabled bool
	err = tx.QueryRowContext(ctx, `SELECT id,current_period_id,enabled FROM billing_subscriptions WHERE user_id=$1 AND tier=$2 FOR UPDATE`, params.UserID, params.Tier).Scan(&subscriptionID, &oldPeriodID, &wasEnabled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		subscriptionID, err = newUUID()
		if err != nil {
			return empty, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_subscriptions (id,user_id,tier,enabled,allowance_usd,period_count,current_period_number,expires_at,created_at,updated_at) VALUES ($1,$2,$3,true,$4::numeric,$5,1,$6,$7,$7)`, subscriptionID, params.UserID, params.Tier, allowance, params.PeriodCount, timeOrNil(expiresAt), params.At); err != nil {
			return empty, mapDBError("create billing subscription", err)
		}
	case err != nil:
		return empty, mapDBError("lock billing subscription", err)
	case oldPeriodID.Valid && wasEnabled:
		result, err := tx.ExecContext(ctx, `UPDATE billing_subscription_periods SET closed_at=$2,close_reason='modified' WHERE id=$1 AND closed_at IS NULL AND $2>=starts_at`, oldPeriodID.String, params.At)
		if err != nil {
			return empty, mapDBError("close modified subscription period", err)
		}
		if err := requireAffected("close modified subscription period", result); err != nil {
			return empty, err
		}
	}
	periodID, err := newUUID()
	if err != nil {
		return empty, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_subscription_periods (id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,period_number,period_count,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$7::numeric,1,$8,$5)`, periodID, subscriptionID, params.UserID, params.Tier, params.At, params.At.Add(duration), allowance, params.PeriodCount); err != nil {
		return empty, mapDBError("create subscription period", err)
	}
	var planID, planVersion any
	if plan != nil {
		planID, planVersion = plan.ID, plan.Version
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET enabled=true,allowance_usd=$2::numeric,period_count=$3,current_period_number=1,expires_at=$4,current_period_id=$5,disabled_at=NULL,updated_at=$6,plan_id=$7,plan_version=$8,config_version=nextval('billing_subscription_config_versions') WHERE id=$1`, subscriptionID, allowance, params.PeriodCount, timeOrNil(expiresAt), periodID, params.At, planID, planVersion); err != nil {
		return empty, mapDBError("activate billing subscription", err)
	}
	return readBillingSubscriptionStateTx(ctx, tx, params.UserID, params.Tier, params.At)
}

func billingPlanTotal(price string, count int) (string, error) {
	value, err := billingRat(price)
	if err != nil {
		return "", err
	}
	value.Mul(value, big.NewRat(int64(count), 1))
	total := value.FloatString(decimal.AmountScale)
	if _, err := decimal.ParsePrice(total); err != nil {
		return "", fmt.Errorf("%w: purchase total exceeds supported range", ErrInvalid)
	}
	return total, nil
}

func debitBillingPlanCashTx(ctx context.Context, tx *sql.Tx, userID, total string, at time.Time) (string, error) {
	var balance string
	if err := tx.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, userID).Scan(&balance); err != nil {
		return "", mapDBError("read purchase balance", err)
	}
	updated, err := billingSubtract(balance, total)
	if err != nil {
		return "", err
	}
	amount, err := billingRat(updated)
	if err != nil {
		return "", err
	}
	if amount.Sign() < 0 {
		return "", &InsufficientFundsError{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,remaining_usd::text FROM billing_cash_credit_lots WHERE user_id=$1 AND remaining_usd>0 ORDER BY lot_sequence FOR UPDATE`, userID)
	if err != nil {
		return "", mapDBError("lock purchase cash lots", err)
	}
	type cashLot struct {
		id        int64
		remaining string
	}
	var lots []cashLot
	for rows.Next() {
		var lot cashLot
		if err := rows.Scan(&lot.id, &lot.remaining); err != nil {
			_ = rows.Close()
			return "", err
		}
		lots = append(lots, lot)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	needed := total
	for _, lot := range lots {
		if !billingPositive(needed) {
			break
		}
		deduction, err := billingMin(lot.remaining, needed)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_cash_credit_lots SET remaining_usd=remaining_usd-$2::numeric WHERE id=$1`, lot.id, deduction); err != nil {
			return "", mapDBError("debit purchase cash lot", err)
		}
		needed, err = billingSubtract(needed, deduction)
		if err != nil {
			return "", err
		}
	}
	if billingPositive(needed) {
		return "", fmt.Errorf("cash lots do not reconcile with account: %w", ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET balance_usd=$2::numeric,updated_at=$3 WHERE user_id=$1`, userID, updated, at); err != nil {
		return "", mapDBError("update purchase balance", err)
	}
	return updated, nil
}

func (s *Store) PurchaseBillingPlan(ctx context.Context, params PurchaseBillingPlanParams) (BillingPlanTransaction, error) {
	if params.PlanID == "" {
		return BillingPlanTransaction{}, fmt.Errorf("%w: plan is required", ErrInvalid)
	}
	return s.transactBillingPlan(ctx, params.BillingWriteParams, params.UserID, params.PlanID, "", params.PlanVersion, params.SubscriptionConfigVersion, params.PeriodCount, false)
}

func (s *Store) RenewBillingSubscription(ctx context.Context, params RenewBillingSubscriptionParams) (BillingPlanTransaction, error) {
	if _, err := billingPeriodDuration(params.Tier); err != nil {
		return BillingPlanTransaction{}, err
	}
	return s.transactBillingPlan(ctx, params.BillingWriteParams, params.UserID, "", params.Tier, params.PlanVersion, params.SubscriptionConfigVersion, params.PeriodCount, true)
}

func (s *Store) transactBillingPlan(ctx context.Context, params BillingWriteParams, userID, planID, tier string, planVersion, configVersion int64, count int, renew bool) (BillingPlanTransaction, error) {
	var result BillingPlanTransaction
	if err := validateBillingWrite(params); err != nil {
		return result, err
	}
	if userID == "" || userID != params.ActorUserID || planVersion < 1 || configVersion < 0 || count < 1 || count > 99 {
		return result, fmt.Errorf("%w: invalid plan transaction", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	opType := "plan_purchase"
	if renew {
		opType = "plan_renewal"
	}
	fingerprint := billingOperationFingerprint(opType, params.ActorUserID, userID, params.Reason, planID, tier, strconv.FormatInt(planVersion, 10), strconv.FormatInt(configVersion, 10), strconv.Itoa(count))
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, userID); err != nil {
			return err
		}
		created, replayID, err := claimBillingOperationTx(ctx, tx, params, opType, userID, fingerprint)
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
				return fmt.Errorf("read plan transaction snapshot: %w", err)
			}
			if snap.Plan == nil || snap.Subscription == nil {
				return fmt.Errorf("plan transaction snapshot missing: %w", ErrConflict)
			}
			result = BillingPlanTransaction{Plan: *snap.Plan, PeriodCount: snap.PeriodCount, TotalUSD: snap.TotalUSD, BalanceUSD: snap.BalanceUSD, Subscription: *snap.Subscription, Ledger: entry}
			return nil
		}
		if renew {
			// The account lock stabilizes this lookup; acquire the plan lock before
			// any subscription or lot lock to match purchases and catalog edits.
			var boundID sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT plan_id FROM billing_subscriptions WHERE user_id=$1 AND tier=$2`, userID, tier).Scan(&boundID)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !boundID.Valid) {
				return ErrBillingPlanChanged
			}
			if err != nil {
				return mapDBError("read renewal binding", err)
			}
			planID = boundID.String
		}
		plan, err := scanBillingPlan(tx.QueryRowContext(ctx, `SELECT `+billingPlanColumns+` FROM billing_plans WHERE id=$1 FOR SHARE`, planID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBillingPlanChanged
		}
		if err != nil {
			return mapDBError("lock purchase plan", err)
		}
		if !plan.Active || plan.Version != planVersion {
			return ErrBillingPlanChanged
		}
		if count < plan.MinPeriodCount {
			return fmt.Errorf("%w: quantity is below the plan minimum", ErrInvalid)
		}
		if !renew {
			tier = plan.Tier
		}
		var currentConfig int64
		err = tx.QueryRowContext(ctx, `SELECT config_version FROM billing_subscriptions WHERE user_id=$1 AND tier=$2 FOR UPDATE`, userID, tier).Scan(&currentConfig)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return mapDBError("lock purchase subscription", err)
		}
		if currentConfig != configVersion {
			return ErrBillingPlanChanged
		}
		var subscription BillingSubscriptionState
		if renew {
			// Advance natural period boundaries before checking renewal. A renewal
			// does not refill the current period or change its immutable snapshot.
			if _, err := rollBillingSubscriptionsTx(ctx, tx, userID, params.At); err != nil {
				return err
			}
			subscription, err = readBillingSubscriptionStateTx(ctx, tx, userID, tier, params.At)
			if err != nil {
				return err
			}
			if !subscription.CanRenew || subscription.Plan.ID != plan.ID {
				return ErrBillingPlanChanged
			}
			if subscription.PeriodCount > math.MaxInt32-count {
				return fmt.Errorf("%w: subscription period count exceeds supported range", ErrInvalid)
			}
			expiry, err := billingPlanExpiry(*subscription.ExpiresAt, tier, count)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET period_count=period_count+$2,expires_at=$3,updated_at=$4,config_version=nextval('billing_subscription_config_versions') WHERE id=$1`, subscription.ID, count, expiry, params.At); err != nil {
				return mapDBError("extend plan subscription", err)
			}
			subscription, err = readBillingSubscriptionStateTx(ctx, tx, userID, tier, params.At)
			if err != nil {
				return err
			}
		} else {
			subscription, err = reopenBillingSubscriptionTx(ctx, tx, PutSubscriptionParams{BillingWriteParams: params, UserID: userID, Tier: tier, AllowanceUSD: plan.AllowanceUSD, PeriodCount: count}, plan.AllowanceUSD, &plan)
			if err != nil {
				return err
			}
		}
		total, err := billingPlanTotal(plan.PriceUSD, count)
		if err != nil {
			return err
		}
		balance, err := debitBillingPlanCashTx(ctx, tx, userID, total, params.At)
		if err != nil {
			return err
		}
		snapshot, err := json.Marshal(billingTransactionSnapshot{Plan: &plan, PeriodCount: count, TotalUSD: total, BalanceUSD: balance, Subscription: &subscription})
		if err != nil {
			return err
		}
		entry, err := scanBillingLedgerEntry(tx.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries (user_id,operation_id,entry_type,amount_usd,cash_delta_usd,balance_after_usd,subscription_tier,subscription_period_id,reason,actor_user_id,created_at,transaction_snapshot) VALUES ($1,$2,$3,$4::numeric,-$4::numeric,$5::numeric,$6,$7,$8,$9,$10,$11) RETURNING `+billingLedgerColumns, userID, params.OperationID, opType, total, balance, tier, subscription.PeriodID, params.Reason, params.ActorUserID, params.At, snapshot))
		if err != nil {
			return mapDBError("record plan transaction ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, entry.ID); err != nil {
			return err
		}
		if err := appendBillingAuditTx(ctx, tx, params, "billing."+opType, "subscription", userID+":"+tier, map[string]any{"operation_id": params.OperationID, "plan": billingPlanAuditMetadata(plan), "period_count": count, "total_usd": total}); err != nil {
			return err
		}
		result = BillingPlanTransaction{Plan: plan, PeriodCount: count, TotalUSD: total, BalanceUSD: balance, Subscription: subscription, Ledger: entry}
		return nil
	})
	return result, err
}
