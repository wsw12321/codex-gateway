//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
)

func TestBillingPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 16, MaxIdleConns: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	// HTTP handlers enforce the Owner role. Store tests only need a valid audit
	// actor, so use a member here and leave the database's single-owner slot to
	// the pre-existing integration suite when both tests share one disposable DB.
	owner := globalUsageIntegrationUser(t, ctx, repository, "billing-actor-"+suffix, UserRoleMember)
	now := time.Now().UTC().Truncate(time.Microsecond)

	t.Run("account migration backfill and new user trigger", func(t *testing.T) {
		billingIntegrationAccountMigration(t, ctx, repository, suffix)

		user := globalUsageIntegrationUser(t, ctx, repository, "bill-zero-"+suffix, UserRoleMember)
		state, err := repository.GetBillingState(ctx, user.ID, 10, 0)
		if err != nil {
			t.Fatalf("GetBillingState(new user): %v", err)
		}
		if state.BalanceUSD != "0.000000000000" {
			t.Fatalf("new user balance = %s, want zero", state.BalanceUSD)
		}
	})

	t.Run("missing subscription target is not found", func(t *testing.T) {
		missingUserID, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		write := billingIntegrationWrite(t, owner.ID, "missing target", now)
		_, err = repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: write, UserID: missingUserID,
			Tier: BillingTierDay, AllowanceUSD: "1",
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("PutSubscription missing target error = %v, want ErrNotFound", err)
		}
		var operations int
		if err := repository.db.QueryRowContext(ctx, `SELECT count(*)
			FROM billing_operations WHERE operation_id = $1`, write.OperationID).Scan(&operations); err != nil {
			t.Fatalf("count missing-target operations: %v", err)
		}
		if operations != 0 {
			t.Fatalf("missing-target write left %d idempotency rows", operations)
		}
	})

	t.Run("recharge snapshot idempotency and negative adjustment", func(t *testing.T) {
		user, _, _ := billingIntegrationPrincipal(t, ctx, repository, "cash-"+suffix)

		rateWrite := billingIntegrationWrite(t, owner.ID, "set integration rate", now)
		settings, err := repository.SetRechargeRate(ctx, SetRechargeRateParams{
			BillingWriteParams: rateWrite, USDPerCNY: "2",
		})
		if err != nil {
			t.Fatalf("SetRechargeRate: %v", err)
		}
		if settings.USDPerCNY != "2.000000000000" {
			t.Fatalf("rate = %s, want 2", settings.USDPerCNY)
		}

		write := billingIntegrationWrite(t, owner.ID, "invoice billing integration", now.Add(time.Second))
		params := RechargeUserParams{
			BillingWriteParams: write, UserID: user.ID, CNYAmount: "10",
		}
		first, err := repository.RechargeUser(ctx, params)
		if err != nil {
			t.Fatalf("RechargeUser: %v", err)
		}
		replay, err := repository.RechargeUser(ctx, params)
		if err != nil {
			t.Fatalf("RechargeUser replay: %v", err)
		}
		if replay.ID != first.ID || first.AmountUSD != "20.000000000000" ||
			first.CNYAmount == nil || *first.CNYAmount != "10.000000000000" ||
			first.USDPerCNYSnapshot == nil || *first.USDPerCNYSnapshot != "2.000000000000" {
			t.Fatalf("unexpected recharge/replay: first=%+v replay=%+v", first, replay)
		}
		changed, err := repository.SetRechargeRate(ctx, SetRechargeRateParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "change future recharge rate", now.Add(1500*time.Millisecond)),
			USDPerCNY:          "3",
		})
		if err != nil {
			t.Fatalf("change recharge rate: %v", err)
		}
		if changed.USDPerCNY != "3.000000000000" {
			t.Fatalf("changed rate = %s, want 3", changed.USDPerCNY)
		}
		replayAfterRateChange, err := repository.RechargeUser(ctx, params)
		if err != nil {
			t.Fatalf("RechargeUser replay after rate change: %v", err)
		}
		if replayAfterRateChange.USDPerCNYSnapshot == nil ||
			*replayAfterRateChange.USDPerCNYSnapshot != "2.000000000000" ||
			replayAfterRateChange.AmountUSD != "20.000000000000" {
			t.Fatalf("recharge lost rate snapshot: %+v", replayAfterRateChange)
		}
		conflict := params
		conflict.CNYAmount = "11"
		if _, err := repository.RechargeUser(ctx, conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("mismatched operation replay error = %v, want ErrConflict", err)
		}

		overdraft := AdjustUserBalanceParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "reject overdraft", now.Add(2*time.Second)),
			UserID:             user.ID, USDAmount: "-20.000001",
		}
		if _, err := repository.AdjustUserBalance(ctx, overdraft); err == nil {
			t.Fatal("negative adjustment unexpectedly overdraws cash account")
		} else {
			var insufficient *InsufficientFundsError
			if !errors.As(err, &insufficient) {
				t.Fatalf("overdraft error = %v, want InsufficientFundsError", err)
			}
		}
		state, err := repository.GetBillingState(ctx, user.ID, 20, 0)
		if err != nil {
			t.Fatalf("GetBillingState(after overdraft): %v", err)
		}
		if state.BalanceUSD != "20.000000000000" {
			t.Fatalf("balance after rejected adjustment = %s", state.BalanceUSD)
		}
		adjustment, err := repository.AdjustUserBalance(ctx, AdjustUserBalanceParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "correct cash", now.Add(3*time.Second)),
			UserID:             user.ID, USDAmount: "-5",
		})
		if err != nil {
			t.Fatalf("AdjustUserBalance: %v", err)
		}
		if adjustment.BalanceAfterUSD == nil || *adjustment.BalanceAfterUSD != "15.000000000000" {
			t.Fatalf("adjustment = %+v, want balance 15", adjustment)
		}
		var accountBalance, lotBalance string
		if err := repository.db.QueryRowContext(ctx, `SELECT
			(SELECT balance_usd::text FROM billing_accounts WHERE user_id = $1),
			(SELECT coalesce(sum(remaining_usd), 0)::text
			 FROM billing_cash_credit_lots WHERE user_id = $1)`, user.ID,
		).Scan(&accountBalance, &lotBalance); err != nil {
			t.Fatalf("read adjustment account/lot totals: %v", err)
		}
		if accountBalance != lotBalance || accountBalance != "15.000000000000" {
			t.Fatalf("adjustment account/lot mismatch = %s/%s", accountBalance, lotBalance)
		}
		var operationCount, ledgerCount, auditCount int
		if err := repository.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM billing_operations WHERE operation_id = $1),
			(SELECT count(*) FROM billing_ledger_entries WHERE operation_id = $1),
			(SELECT count(*) FROM audit_events
			 WHERE event_type = 'billing.recharged' AND metadata->>'operation_id' = $1::text)`,
			write.OperationID,
		).Scan(&operationCount, &ledgerCount, &auditCount); err != nil {
			t.Fatalf("read idempotent recharge records: %v", err)
		}
		if operationCount != 1 || ledgerCount != 1 || auditCount != 1 {
			t.Fatalf("replayed recharge records = operation %d ledger %d audit %d",
				operationCount, ledgerCount, auditCount)
		}
	})

	t.Run("subscription update reopens immediately and delete disables", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "subscription-"+suffix)
		firstAt := now.Add(10 * time.Second)
		first, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "start day plan", firstAt),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "5", PeriodCount: 5,
		})
		if err != nil {
			t.Fatalf("PutSubscription(first): %v", err)
		}
		secondAt := firstAt.Add(time.Hour)
		secondParams := PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "change day plan", secondAt),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "7", PeriodCount: 2,
		}
		second, err := repository.PutSubscription(ctx, secondParams)
		if err != nil {
			t.Fatalf("PutSubscription(second): %v", err)
		}
		replay, err := repository.PutSubscription(ctx, secondParams)
		if err != nil {
			t.Fatalf("PutSubscription replay: %v", err)
		}
		if first.PeriodID == nil || second.PeriodID == nil || replay.PeriodID == nil ||
			*first.PeriodID == *second.PeriodID || *replay.PeriodID != *second.PeriodID ||
			second.ID == "" || replay.ID != second.ID ||
			second.RemainingUSD != "7.000000000000" || second.PeriodStartsAt == nil ||
			!second.PeriodStartsAt.Equal(secondAt) || second.PeriodEndsAt == nil ||
			!second.PeriodEndsAt.Equal(secondAt.Add(24*time.Hour)) || second.PeriodCount != 2 ||
			second.CurrentPeriodNumber != 1 || second.ExpiresAt == nil ||
			!second.ExpiresAt.Equal(secondAt.Add(48*time.Hour)) || replay.PeriodCount != 2 ||
			replay.ExpiresAt == nil || !replay.ExpiresAt.Equal(*second.ExpiresAt) {
			t.Fatalf("subscription was not reopened immediately: first=%+v second=%+v replay=%+v", first, second, replay)
		}
		var oldClosed time.Time
		var oldReason, oldRemaining string
		if err := repository.db.QueryRowContext(ctx, `SELECT closed_at, close_reason,
			remaining_usd::text FROM billing_subscription_periods WHERE id = $1`,
			*first.PeriodID).Scan(&oldClosed, &oldReason, &oldRemaining); err != nil {
			t.Fatalf("read old subscription period: %v", err)
		}
		if !oldClosed.Equal(secondAt) || oldReason != "modified" || oldRemaining != "5.000000000000" {
			t.Fatalf("old period close = %v, %q, remaining %s", oldClosed, oldReason, oldRemaining)
		}

		boundAt := secondAt.Add(30 * time.Second)
		boundRequestID := billingIntegrationRequestID(suffix, "subscription-bound", 1)
		if _, err := repository.ReserveBilling(ctx, BillingReservationParams{
			RequestID: boundRequestID, UserID: user.ID, APIKeyID: key.ID,
			Model: "billing-priced-model", InputUSDPerMillion: "1",
			CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: boundAt,
		}); err != nil {
			t.Fatalf("ReserveBilling(before disable): %v", err)
		}

		deleteAt := secondAt.Add(time.Minute)
		deleteParams := DeleteSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "stop day plan", deleteAt),
			UserID:             user.ID, Tier: BillingTierDay,
		}
		deleted, err := repository.DeleteSubscription(ctx, deleteParams)
		if err != nil {
			t.Fatalf("DeleteSubscription: %v", err)
		}
		if deleted.Enabled || deleted.ID != second.ID || deleted.RemainingUSD != "0.000000000000" {
			t.Fatalf("deleted subscription remains enabled: %+v", deleted)
		}
		billingIntegrationComplete(t, ctx, repository, user, device, key,
			boundRequestID, boundAt, 1_000_000, "")
		settled, err := repository.SettleBilling(ctx, boundRequestID, deleteAt.Add(time.Second))
		if err != nil {
			t.Fatalf("settle admission-bound disabled period: %v", err)
		}
		if settled.ChargedUSD == nil || *settled.ChargedUSD != "1.000000000000" {
			t.Fatalf("disabled bound period was not charged: %+v", settled)
		}
		putReplayAfterCharge, err := repository.PutSubscription(ctx, secondParams)
		if err != nil {
			t.Fatalf("PutSubscription replay after charge: %v", err)
		}
		if putReplayAfterCharge.ID != second.ID ||
			putReplayAfterCharge.RemainingUSD != second.RemainingUSD ||
			putReplayAfterCharge.PeriodCount != second.PeriodCount {
			t.Fatalf("subscription replay changed after charge: first=%+v replay=%+v", second, putReplayAfterCharge)
		}
		periodConflict := secondParams
		periodConflict.PeriodCount = 3
		if _, err := repository.PutSubscription(ctx, periodConflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("period-count replay mismatch error = %v, want ErrConflict", err)
		}
		deleteReplay, err := repository.DeleteSubscription(ctx, deleteParams)
		if err != nil {
			t.Fatalf("DeleteSubscription replay after charge: %v", err)
		}
		if deleteReplay.ID != deleted.ID || deleteReplay.RemainingUSD != deleted.RemainingUSD ||
			deleteReplay.Enabled != deleted.Enabled {
			t.Fatalf("disable replay changed after charge: first=%+v replay=%+v", deleted, deleteReplay)
		}
		var enabled bool
		var disabledAt, closedAt time.Time
		if err := repository.db.QueryRowContext(ctx, `
			SELECT s.enabled, s.disabled_at, p.closed_at
			FROM billing_subscriptions s
			JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID,
		).Scan(&enabled, &disabledAt, &closedAt); err != nil {
			t.Fatalf("read disabled subscription: %v", err)
		}
		if enabled || !disabledAt.Equal(deleteAt) || !closedAt.Equal(deleteAt) {
			t.Fatalf("disable state = enabled %v, disabled %v, closed %v", enabled, disabledAt, closedAt)
		}
		state, err := repository.GetBillingState(ctx, user.ID, 20, 0)
		if err != nil {
			t.Fatalf("GetBillingState(disabled): %v", err)
		}
		if state.Subscriptions[0].Enabled || state.Subscriptions[0].RemainingUSD != "0.000000000000" {
			t.Fatalf("disabled subscription exposes unusable quota: %+v", state.Subscriptions[0])
		}
	})

	t.Run("periodless legacy disable replay keeps its operation snapshot", func(t *testing.T) {
		user := globalUsageIntegrationUser(t, ctx, repository, "bill-periodless-"+suffix, UserRoleMember)
		base := now.Add(5 * time.Hour)
		legacyExpiry := base.Add(-time.Hour)
		subscriptionID, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.db.ExecContext(ctx, `INSERT INTO billing_subscriptions
			(id,user_id,tier,enabled,allowance_usd,period_count,current_period_number,
			 expires_at,created_at,updated_at,disabled_at)
			VALUES ($1,$2,'month',false,5,1,1,$3,$4,$4,$3)`, subscriptionID,
			user.ID, legacyExpiry, base.Add(-2*time.Hour)); err != nil {
			t.Fatalf("create periodless legacy subscription: %v", err)
		}

		deleteParams := DeleteSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "replay periodless disable", base),
			UserID:             user.ID, Tier: BillingTierMonth,
		}
		first, err := repository.DeleteSubscription(ctx, deleteParams)
		if err != nil {
			t.Fatalf("DeleteSubscription(periodless): %v", err)
		}
		if first.PeriodID != nil || first.PeriodCount != 1 || first.CurrentPeriodNumber != 1 ||
			first.ExpiresAt == nil || !first.ExpiresAt.Equal(legacyExpiry) {
			t.Fatalf("periodless disable response = %+v", first)
		}

		reopened, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "reopen periodless subscription", base.Add(time.Hour)),
			UserID:             user.ID, Tier: BillingTierMonth, AllowanceUSD: "9", PeriodCount: 3,
		})
		if err != nil {
			t.Fatalf("PutSubscription(after periodless disable): %v", err)
		}
		if reopened.PeriodCount != 3 || reopened.ExpiresAt == nil || reopened.ExpiresAt.Equal(legacyExpiry) {
			t.Fatalf("reopened subscription did not change configuration: %+v", reopened)
		}

		replay, err := repository.DeleteSubscription(ctx, deleteParams)
		if err != nil {
			t.Fatalf("DeleteSubscription(periodless replay): %v", err)
		}
		if replay.ID != first.ID || replay.PeriodID != nil ||
			replay.PeriodCount != first.PeriodCount ||
			replay.CurrentPeriodNumber != first.CurrentPeriodNumber ||
			replay.ExpiresAt == nil || !replay.ExpiresAt.Equal(*first.ExpiresAt) {
			t.Fatalf("periodless disable replay changed after reopen: first=%+v replay=%+v", first, replay)
		}
	})

	t.Run("legacy subscription set fingerprint replays only as migrated one period", func(t *testing.T) {
		user := globalUsageIntegrationUser(t, ctx, repository, "bill-legacy-set-"+suffix, UserRoleMember)
		base := now.Add(6 * time.Hour)
		write := billingIntegrationWrite(t, owner.ID, "replay legacy subscription set", base)
		params := PutSubscriptionParams{
			BillingWriteParams: write,
			UserID:             user.ID, Tier: BillingTierWeek, AllowanceUSD: "7", PeriodCount: 1,
		}
		first, err := repository.PutSubscription(ctx, params)
		if err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		legacyFingerprint := billingOperationFingerprint("subscription_set", owner.ID,
			user.ID, BillingTierWeek, write.Reason, params.AllowanceUSD)
		if _, err := repository.db.ExecContext(ctx, `UPDATE billing_operations
			SET request_fingerprint = $2 WHERE operation_id = $1`,
			write.OperationID, legacyFingerprint); err != nil {
			t.Fatalf("simulate pre-0004 subscription fingerprint: %v", err)
		}

		replay, err := repository.PutSubscription(ctx, params)
		if err != nil {
			t.Fatalf("PutSubscription(legacy replay): %v", err)
		}
		if replay.ID != first.ID || replay.PeriodCount != 1 || replay.CurrentPeriodNumber != 1 ||
			replay.PeriodID == nil || first.PeriodID == nil || *replay.PeriodID != *first.PeriodID ||
			replay.ExpiresAt == nil || first.ExpiresAt == nil || !replay.ExpiresAt.Equal(*first.ExpiresAt) {
			t.Fatalf("legacy subscription replay changed: first=%+v replay=%+v", first, replay)
		}
		var periods int
		if err := repository.db.QueryRowContext(ctx, `SELECT count(*)
			FROM billing_subscription_periods WHERE subscription_id = $1`, first.ID).Scan(&periods); err != nil {
			t.Fatalf("count legacy replay periods: %v", err)
		}
		if periods != 1 {
			t.Fatalf("legacy replay period rows = %d, want 1", periods)
		}

		mismatch := params
		mismatch.PeriodCount = 2
		if _, err := repository.PutSubscription(ctx, mismatch); !errors.Is(err, ErrConflict) {
			t.Fatalf("legacy replay with period_count=2 error = %v, want ErrConflict", err)
		}
	})

	t.Run("one finite period expires at its original boundary and bound work settles", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "finite-one-"+suffix)
		base := now.Add(10 * 24 * time.Hour)
		created, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "one finite day", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "2", PeriodCount: 1,
		})
		if err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		if created.ExpiresAt == nil || !created.ExpiresAt.Equal(base.Add(24*time.Hour)) ||
			created.CurrentPeriodNumber != 1 || created.PeriodCount != 1 {
			t.Fatalf("created finite subscription = %+v", created)
		}

		boundRequestID := billingIntegrationRequestID(suffix, "finite-one-bound", 1)
		if _, err := repository.ReserveBilling(ctx, BillingReservationParams{
			RequestID: boundRequestID, UserID: user.ID, APIKeyID: key.ID,
			Model: "billing-priced-model", InputUSDPerMillion: "1",
			CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
			Now: base.Add(24*time.Hour - time.Second),
		}); err != nil {
			t.Fatalf("ReserveBilling(before expiry): %v", err)
		}
		expiredRequestID := billingIntegrationRequestID(suffix, "finite-one-expired", 1)
		_, err = repository.ReserveBilling(ctx, BillingReservationParams{
			RequestID: expiredRequestID, UserID: user.ID, APIKeyID: key.ID,
			Model: "billing-priced-model", InputUSDPerMillion: "1",
			CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
			Now: base.Add(24 * time.Hour),
		})
		var insufficient *InsufficientFundsError
		if !errors.As(err, &insufficient) {
			t.Fatalf("ReserveBilling(at expiry) error = %v, want InsufficientFundsError", err)
		}

		var enabled bool
		var disabledAt, expiresAt, closedAt time.Time
		var remaining string
		var renewalCount int
		if err := repository.db.QueryRowContext(ctx, `SELECT s.enabled, s.disabled_at,
			s.expires_at, p.closed_at, p.remaining_usd::text,
			(SELECT count(*) FROM billing_ledger_entries l
			 WHERE l.user_id = s.user_id AND l.subscription_tier = s.tier
			   AND l.entry_type = 'subscription_renewal')
			FROM billing_subscriptions s
			JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID,
		).Scan(&enabled, &disabledAt, &expiresAt, &closedAt, &remaining, &renewalCount); err != nil {
			t.Fatalf("read expired finite subscription: %v", err)
		}
		wantExpiry := base.Add(24 * time.Hour)
		if enabled || !disabledAt.Equal(wantExpiry) || !expiresAt.Equal(wantExpiry) ||
			!closedAt.Equal(wantExpiry) || remaining != "2.000000000000" || renewalCount != 0 {
			t.Fatalf("expired finite state = enabled %v disabled %v expires %v closed %v remaining %s renewals %d",
				enabled, disabledAt, expiresAt, closedAt, remaining, renewalCount)
		}

		requestedAt := base.Add(24*time.Hour - time.Second)
		billingIntegrationComplete(t, ctx, repository, user, device, key,
			boundRequestID, requestedAt, 1_000_000, "")
		settled, err := repository.SettleBilling(ctx, boundRequestID, wantExpiry.Add(time.Second))
		if err != nil {
			t.Fatalf("SettleBilling(bound before expiry): %v", err)
		}
		if settled.ChargedUSD == nil || *settled.ChargedUSD != "1.000000000000" {
			t.Fatalf("expired bound period did not settle: %+v", settled)
		}
	})

	t.Run("failed admission rolls back quota and converges finite expiry", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "finite-admission-"+suffix)
		base := now.Add(15 * 24 * time.Hour)
		if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "one finite admission day", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "2", PeriodCount: 1,
		}); err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}

		expiresAt := base.Add(24 * time.Hour)
		requestID := billingIntegrationRequestID(suffix, "finite-admission-expired", 1)
		_, err := repository.AdmitRequest(ctx,
			billingIntegrationAdmission(user, device, key, requestID, expiresAt))
		var insufficient *InsufficientFundsError
		if !errors.As(err, &insufficient) {
			t.Fatalf("AdmitRequest(at expiry) error = %v, want InsufficientFundsError", err)
		}

		var enabled bool
		var disabledAt, closedAt time.Time
		var quotaReservations, billingReservations, usageRequests int
		if err := repository.db.QueryRowContext(ctx, `SELECT s.enabled, s.disabled_at,
			p.closed_at,
			(SELECT count(*) FROM quota_reservations q WHERE q.request_id = $2),
			(SELECT count(*) FROM billing_reservations b WHERE b.request_id = $2),
			(SELECT count(*) FROM usage_requests u WHERE u.request_id = $2)
			FROM billing_subscriptions s
			JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID, requestID,
		).Scan(&enabled, &disabledAt, &closedAt, &quotaReservations,
			&billingReservations, &usageRequests); err != nil {
			t.Fatalf("read failed-admission expiry state: %v", err)
		}
		if enabled || !disabledAt.Equal(expiresAt) || !closedAt.Equal(expiresAt) {
			t.Fatalf("failed admission expiry = enabled %v disabled %v closed %v, want false %v %v",
				enabled, disabledAt, closedAt, expiresAt, expiresAt)
		}
		if quotaReservations != 0 || billingReservations != 0 || usageRequests != 0 {
			t.Fatalf("failed admission left request rows = quota %d billing %d usage %d",
				quotaReservations, billingReservations, usageRequests)
		}
	})

	t.Run("finite periods renew exactly to their configured limit", func(t *testing.T) {
		user, _, key := billingIntegrationPrincipal(t, ctx, repository, "finite-three-"+suffix)
		base := now.Add(20 * 24 * time.Hour)
		if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "three finite days", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "1", PeriodCount: 3,
		}); err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		for period := 2; period <= 3; period++ {
			requestID := billingIntegrationRequestID(suffix, "finite-three", period)
			if _, err := repository.ReserveBilling(ctx, BillingReservationParams{
				RequestID: requestID, UserID: user.ID, APIKeyID: key.ID,
				Model: "billing-priced-model", InputUSDPerMillion: "1",
				CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
				Now: base.Add(time.Duration(period-1) * 24 * time.Hour),
			}); err != nil {
				t.Fatalf("ReserveBilling(period %d): %v", period, err)
			}
		}
		_, err := repository.ReserveBilling(ctx, BillingReservationParams{
			RequestID: billingIntegrationRequestID(suffix, "finite-three-expired", 1),
			UserID:    user.ID, APIKeyID: key.ID, Model: "billing-priced-model",
			InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
			Now: base.Add(3 * 24 * time.Hour),
		})
		var insufficient *InsufficientFundsError
		if !errors.As(err, &insufficient) {
			t.Fatalf("final boundary error = %v, want InsufficientFundsError", err)
		}
		var enabled bool
		var current, renewals, periods int
		var disabledAt time.Time
		if err := repository.db.QueryRowContext(ctx, `SELECT s.enabled,
			s.current_period_number, s.disabled_at,
			(SELECT count(*) FROM billing_ledger_entries l
			 WHERE l.user_id = s.user_id AND l.subscription_tier = s.tier
			   AND l.entry_type = 'subscription_renewal'),
			(SELECT count(*) FROM billing_subscription_periods p
			 WHERE p.subscription_id = s.id)
			FROM billing_subscriptions s WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID,
		).Scan(&enabled, &current, &disabledAt, &renewals, &periods); err != nil {
			t.Fatalf("read finite renewal counts: %v", err)
		}
		if enabled || current != 3 || renewals != 2 || periods != 3 ||
			!disabledAt.Equal(base.Add(3*24*time.Hour)) {
			t.Fatalf("finite renewal state = enabled %v current %d renewals %d periods %d disabled %v",
				enabled, current, renewals, periods, disabledAt)
		}
	})

	t.Run("idle periods advance the sequence without changing final expiry", func(t *testing.T) {
		user, _, key := billingIntegrationPrincipal(t, ctx, repository, "finite-idle-"+suffix)
		base := now.Add(30 * 24 * time.Hour)
		if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "five finite days", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "4", PeriodCount: 5,
		}); err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		if _, err := repository.ReserveBilling(ctx, BillingReservationParams{
			RequestID: billingIntegrationRequestID(suffix, "finite-idle", 1),
			UserID:    user.ID, APIKeyID: key.ID, Model: "billing-priced-model",
			InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
			Now: base.Add(3*24*time.Hour + time.Hour),
		}); err != nil {
			t.Fatalf("ReserveBilling(after idle periods): %v", err)
		}
		var current, periodNumber, periodCount, periodRows int
		var startsAt, expiresAt time.Time
		var remaining string
		if err := repository.db.QueryRowContext(ctx, `SELECT s.current_period_number,
			s.expires_at, p.period_number, p.period_count, p.starts_at,
			p.remaining_usd::text,
			(SELECT count(*) FROM billing_subscription_periods allp
			 WHERE allp.subscription_id = s.id)
			FROM billing_subscriptions s
			JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID,
		).Scan(&current, &expiresAt, &periodNumber, &periodCount, &startsAt, &remaining, &periodRows); err != nil {
			t.Fatalf("read idle-period state: %v", err)
		}
		if current != 4 || periodNumber != 4 || periodCount != 5 || periodRows != 2 ||
			!startsAt.Equal(base.Add(3*24*time.Hour)) || !expiresAt.Equal(base.Add(5*24*time.Hour)) ||
			remaining != "4.000000000000" {
			t.Fatalf("idle-period state = current %d snapshot %d/%d rows %d starts %v expires %v remaining %s",
				current, periodNumber, periodCount, periodRows, startsAt, expiresAt, remaining)
		}
	})

	t.Run("zero period count remains unlimited across idle periods", func(t *testing.T) {
		user, _, key := billingIntegrationPrincipal(t, ctx, repository, "unlimited-"+suffix)
		base := now.Add(40 * 24 * time.Hour)
		created, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "unlimited day", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "3", PeriodCount: 0,
		})
		if err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		if created.ExpiresAt != nil || created.PeriodCount != 0 {
			t.Fatalf("unlimited response = %+v", created)
		}
		if _, err := repository.ReserveBilling(ctx, BillingReservationParams{
			RequestID: billingIntegrationRequestID(suffix, "unlimited-idle", 1),
			UserID:    user.ID, APIKeyID: key.ID, Model: "billing-priced-model",
			InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
			Now: base.Add(200*24*time.Hour + time.Hour),
		}); err != nil {
			t.Fatalf("ReserveBilling(unlimited after idle): %v", err)
		}
		var enabled bool
		var current, snapshotCount int
		var expiresAt sql.NullTime
		if err := repository.db.QueryRowContext(ctx, `SELECT s.enabled,
			s.current_period_number, s.expires_at, p.period_count
			FROM billing_subscriptions s
			JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID,
		).Scan(&enabled, &current, &expiresAt, &snapshotCount); err != nil {
			t.Fatalf("read unlimited state: %v", err)
		}
		if !enabled || current != 201 || expiresAt.Valid || snapshotCount != 0 {
			t.Fatalf("unlimited state = enabled %v current %d expires %v snapshot %d",
				enabled, current, expiresAt, snapshotCount)
		}
	})

	t.Run("concurrent boundaries do not duplicate renewal or cross the limit", func(t *testing.T) {
		user, _, key := billingIntegrationPrincipal(t, ctx, repository, "finite-concurrent-"+suffix)
		base := now.Add(50 * 24 * time.Hour)
		if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "two concurrent days", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "2", PeriodCount: 2,
		}); err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		runConcurrent := func(label string, at time.Time, wantInsufficient bool) {
			t.Helper()
			errorsByRequest := make(chan error, 2)
			var group sync.WaitGroup
			for index := 1; index <= 2; index++ {
				index := index
				group.Add(1)
				go func() {
					defer group.Done()
					_, err := repository.ReserveBilling(ctx, BillingReservationParams{
						RequestID: billingIntegrationRequestID(suffix, label, index),
						UserID:    user.ID, APIKeyID: key.ID, Model: "billing-priced-model",
						InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
						Now: at,
					})
					errorsByRequest <- err
				}()
			}
			group.Wait()
			close(errorsByRequest)
			for err := range errorsByRequest {
				var insufficient *InsufficientFundsError
				if (!wantInsufficient && err != nil) ||
					(wantInsufficient && !errors.As(err, &insufficient)) {
					t.Fatalf("concurrent %s error = %v, want insufficient %v", label, err, wantInsufficient)
				}
			}
		}
		runConcurrent("finite-concurrent-renew", base.Add(24*time.Hour), false)
		runConcurrent("finite-concurrent-expire", base.Add(48*time.Hour), true)

		var enabled bool
		var current, renewals, periods int
		if err := repository.db.QueryRowContext(ctx, `SELECT s.enabled,
			s.current_period_number,
			(SELECT count(*) FROM billing_ledger_entries l
			 WHERE l.user_id = s.user_id AND l.subscription_tier = s.tier
			   AND l.entry_type = 'subscription_renewal'),
			(SELECT count(*) FROM billing_subscription_periods p
			 WHERE p.subscription_id = s.id)
			FROM billing_subscriptions s WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID,
		).Scan(&enabled, &current, &renewals, &periods); err != nil {
			t.Fatalf("read concurrent subscription state: %v", err)
		}
		if enabled || current != 2 || renewals != 1 || periods != 2 {
			t.Fatalf("concurrent subscription state = enabled %v current %d renewals %d periods %d",
				enabled, current, renewals, periods)
		}
	})

	t.Run("maintenance converges untouched finite subscriptions at planned expiry", func(t *testing.T) {
		user := globalUsageIntegrationUser(t, ctx, repository, "bill-maintenance-"+suffix, UserRoleMember)
		base := now.Add(60 * 24 * time.Hour)
		if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "four untouched days", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "6", PeriodCount: 4,
		}); err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		expiresAt := base.Add(4 * 24 * time.Hour)
		count, err := repository.ExpireBillingSubscriptions(ctx, expiresAt.Add(time.Hour), 1000)
		if err != nil {
			t.Fatalf("ExpireBillingSubscriptions: %v", err)
		}
		if count < 1 {
			t.Fatalf("expired subscription count = %d, want at least 1", count)
		}
		var enabled bool
		var current, periodNumber, periodCount int
		var disabledAt, startsAt, endsAt, closedAt time.Time
		if err := repository.db.QueryRowContext(ctx, `SELECT s.enabled,
			s.current_period_number, s.disabled_at, p.period_number, p.period_count,
			p.starts_at, p.ends_at, p.closed_at
			FROM billing_subscriptions s
			JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1 AND s.tier = 'day'`, user.ID,
		).Scan(&enabled, &current, &disabledAt, &periodNumber, &periodCount,
			&startsAt, &endsAt, &closedAt); err != nil {
			t.Fatalf("read maintenance-expired subscription: %v", err)
		}
		if enabled || current != 1 || periodNumber != current || periodCount != 4 ||
			!startsAt.Equal(base) || !endsAt.Equal(base.Add(24*time.Hour)) ||
			!closedAt.Equal(endsAt) || !disabledAt.Equal(expiresAt) {
			t.Fatalf("maintenance expiry = enabled %v current %d snapshot %d/%d starts %v ends %v closed %v disabled %v",
				enabled, current, periodNumber, periodCount, startsAt, endsAt, closedAt, disabledAt)
		}
	})

	t.Run("settlement drains day week month then cash and is idempotent", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "split-"+suffix)
		base := now.Add(24 * time.Hour)
		billingIntegrationSetRate(t, ctx, repository, owner.ID, "2", base)
		billingIntegrationRecharge(t, ctx, repository, owner.ID, user.ID, "5", base.Add(time.Second))
		for index, tier := range []string{BillingTierDay, BillingTierWeek, BillingTierMonth} {
			allowance := strconv.Itoa(index + 1)
			if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
				BillingWriteParams: billingIntegrationWrite(t, owner.ID, "create "+tier+" plan", base.Add(time.Duration(index+2)*time.Second)),
				UserID:             user.ID, Tier: tier, AllowanceUSD: allowance,
			}); err != nil {
				t.Fatalf("PutSubscription(%s): %v", tier, err)
			}
		}
		requestID := billingIntegrationRequestID(suffix, "split", 1)
		billingIntegrationReserveAndComplete(t, ctx, repository, user, device, key,
			requestID, base.Add(10*time.Second), 1_000_000, "8", "upstream-different-model")
		settled, err := repository.SettleBilling(ctx, requestID, base.Add(12*time.Second))
		if err != nil {
			t.Fatalf("SettleBilling: %v", err)
		}
		replayed, err := repository.SettleBilling(ctx, requestID, base.Add(13*time.Second))
		if err != nil {
			t.Fatalf("SettleBilling replay: %v", err)
		}
		if settled.ActualCostUSD == nil || *settled.ActualCostUSD != "8.000000000000" ||
			settled.ChargedUSD == nil || *settled.ChargedUSD != "8.000000000000" ||
			settled.UncoveredUSD == nil || *settled.UncoveredUSD != "0.000000000000" ||
			replayed.SettledAt == nil || settled.SettledAt == nil || !replayed.SettledAt.Equal(*settled.SettledAt) {
			t.Fatalf("unexpected settlement/replay: settled=%+v replay=%+v", settled, replayed)
		}
		billingIntegrationAssertAllocations(t, ctx, repository, requestID,
			[]string{"day", "week", "month", "cash"},
			[]string{"1.000000000000", "2.000000000000", "3.000000000000", "2.000000000000"})
		state, err := repository.GetBillingState(ctx, user.ID, 50, 0)
		if err != nil {
			t.Fatalf("GetBillingState(after split): %v", err)
		}
		if state.BalanceUSD != "8.000000000000" {
			t.Fatalf("cash balance after split = %s, want 8", state.BalanceUSD)
		}
		for _, subscription := range state.Subscriptions {
			if subscription.Enabled && subscription.RemainingUSD != "0.000000000000" {
				t.Fatalf("%s remaining = %s, want zero", subscription.Tier, subscription.RemainingUSD)
			}
		}
		var ledgerModel string
		var usageLedgerCount int
		if err := repository.db.QueryRowContext(ctx, `SELECT model, count(*) OVER ()
			FROM billing_ledger_entries WHERE request_id = $1`, requestID,
		).Scan(&ledgerModel, &usageLedgerCount); err != nil {
			t.Fatalf("read usage ledger: %v", err)
		}
		if ledgerModel != "billing-priced-model" || usageLedgerCount != 1 {
			t.Fatalf("usage ledger model/count = %q/%d", ledgerModel, usageLedgerCount)
		}
	})

	t.Run("single request can exceed its bound subscription", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "uncovered-"+suffix)
		base := now.Add(48 * time.Hour)
		if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "small day plan", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "1",
		}); err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		requestID := billingIntegrationRequestID(suffix, "uncovered", 1)
		billingIntegrationReserveAndComplete(t, ctx, repository, user, device, key,
			requestID, base.Add(time.Second), 1_000_000, "2", "")
		settled, err := repository.SettleBilling(ctx, requestID, base.Add(3*time.Second))
		if err != nil {
			t.Fatalf("SettleBilling: %v", err)
		}
		if settled.ChargedUSD == nil || *settled.ChargedUSD != "1.000000000000" ||
			settled.UncoveredUSD == nil || *settled.UncoveredUSD != "1.000000000000" {
			t.Fatalf("over-limit settlement = %+v", settled)
		}
	})

	t.Run("admission cash cutoff excludes later recharge", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "cutoff-"+suffix)
		base := now.Add(72 * time.Hour)
		billingIntegrationSetRate(t, ctx, repository, owner.ID, "2", base)
		billingIntegrationRecharge(t, ctx, repository, owner.ID, user.ID, "0.5", base.Add(time.Second))
		requestID := billingIntegrationRequestID(suffix, "cutoff", 1)
		reservation, err := repository.ReserveBilling(ctx, BillingReservationParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: "billing-priced-model",
			InputUSDPerMillion: "2", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
			Now: base.Add(2 * time.Second),
		})
		if err != nil {
			t.Fatalf("ReserveBilling: %v", err)
		}
		if reservation.CashLotCutoff == nil || *reservation.CashLotCutoff != 1 {
			t.Fatalf("cash cutoff = %v, want first lot", reservation.CashLotCutoff)
		}
		billingIntegrationRecharge(t, ctx, repository, owner.ID, user.ID, "5", base.Add(3*time.Second))
		billingIntegrationComplete(t, ctx, repository, user, device, key,
			requestID, base.Add(2*time.Second), 1_000_000, "")
		settled, err := repository.SettleBilling(ctx, requestID, base.Add(5*time.Second))
		if err != nil {
			t.Fatalf("SettleBilling: %v", err)
		}
		if settled.ChargedUSD == nil || *settled.ChargedUSD != "1.000000000000" ||
			settled.UncoveredUSD == nil || *settled.UncoveredUSD != "1.000000000000" {
			t.Fatalf("cash cutoff settlement = %+v", settled)
		}
		state, err := repository.GetBillingState(ctx, user.ID, 20, 0)
		if err != nil {
			t.Fatalf("GetBillingState: %v", err)
		}
		if state.BalanceUSD != "10.000000000000" {
			t.Fatalf("later recharge balance = %s, want untouched 10", state.BalanceUSD)
		}
		var laterRemaining string
		if err := repository.db.QueryRowContext(ctx, `SELECT remaining_usd::text
			FROM billing_cash_credit_lots WHERE user_id = $1 AND lot_sequence = 2`,
			user.ID).Scan(&laterRemaining); err != nil {
			t.Fatalf("read later credit lot: %v", err)
		}
		if laterRemaining != "10.000000000000" {
			t.Fatalf("later credit lot remaining = %s", laterRemaining)
		}
	})

	t.Run("concurrent settlements never make cash negative", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "concurrent-"+suffix)
		base := now.Add(96 * time.Hour)
		billingIntegrationSetRate(t, ctx, repository, owner.ID, "2", base)
		billingIntegrationRecharge(t, ctx, repository, owner.ID, user.ID, "1.5", base.Add(time.Second))
		requestIDs := []string{
			billingIntegrationRequestID(suffix, "concurrent", 1),
			billingIntegrationRequestID(suffix, "concurrent", 2),
		}
		for index, requestID := range requestIDs {
			billingIntegrationReserveAndComplete(t, ctx, repository, user, device, key,
				requestID, base.Add(time.Duration(index+2)*time.Second), 1_000_000, "2", "")
		}
		start := make(chan struct{})
		errorsByRequest := make([]error, len(requestIDs))
		var wait sync.WaitGroup
		for index, requestID := range requestIDs {
			wait.Add(1)
			go func(index int, requestID string) {
				defer wait.Done()
				<-start
				_, errorsByRequest[index] = repository.SettleBilling(ctx, requestID, base.Add(10*time.Second))
			}(index, requestID)
		}
		close(start)
		wait.Wait()
		for index, err := range errorsByRequest {
			if err != nil {
				t.Fatalf("concurrent settlement %d: %v", index, err)
			}
		}
		var balance, minimumLot, charged, uncovered string
		if err := repository.db.QueryRowContext(ctx, `SELECT
			(SELECT balance_usd::text FROM billing_accounts WHERE user_id = $1),
			(SELECT min(remaining_usd)::text FROM billing_cash_credit_lots WHERE user_id = $1),
			(SELECT sum(charged_usd)::text FROM billing_reservations WHERE request_id IN ($2,$3)),
			(SELECT sum(uncovered_usd)::text FROM billing_reservations WHERE request_id IN ($2,$3))`,
			user.ID, requestIDs[0], requestIDs[1]).Scan(&balance, &minimumLot, &charged, &uncovered); err != nil {
			t.Fatalf("read concurrent settlement totals: %v", err)
		}
		if balance != "0.000000000000" || minimumLot != "0.000000000000" ||
			charged != "3.000000000000" || uncovered != "1.000000000000" {
			t.Fatalf("concurrent totals = balance %s, lot %s, charged %s, uncovered %s",
				balance, minimumLot, charged, uncovered)
		}
		for _, requestID := range requestIDs {
			if _, err := repository.SettleBilling(ctx, requestID, base.Add(11*time.Second)); err != nil {
				t.Fatalf("idempotent settlement %s: %v", requestID, err)
			}
		}
	})

	t.Run("joint admission rollback settlement retry and stale release", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "atomic-"+suffix)
		past := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
		zeroRequestID := billingIntegrationRequestID(suffix, "atomic-zero", 1)
		zeroAdmission := billingIntegrationAdmission(user, device, key, zeroRequestID, past)
		if _, err := repository.AdmitRequest(ctx, zeroAdmission); err == nil {
			t.Fatal("zero-balance admission unexpectedly succeeded")
		} else {
			var insufficient *InsufficientFundsError
			if !errors.As(err, &insufficient) {
				t.Fatalf("zero-balance admission error = %v, want InsufficientFundsError", err)
			}
		}
		var rateWindows, counters, quotaReservations, billingReservations, usageRows int
		if err := repository.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM quota_rate_windows WHERE window_start = $1
			 AND scope_id IN ($2,$3)),
			(SELECT count(*) FROM quota_counters WHERE quota_day = $1::date
			 AND scope_id IN ($2,$3)),
			(SELECT count(*) FROM quota_reservations WHERE request_id = $4),
			(SELECT count(*) FROM billing_reservations WHERE request_id = $4),
			(SELECT count(*) FROM usage_requests WHERE request_id = $4)`,
			past.Truncate(time.Minute), user.ID, key.ID, zeroRequestID,
		).Scan(&rateWindows, &counters, &quotaReservations, &billingReservations, &usageRows); err != nil {
			t.Fatalf("read rolled-back admission state: %v", err)
		}
		if rateWindows != 0 || counters != 0 || quotaReservations != 0 ||
			billingReservations != 0 || usageRows != 0 {
			t.Fatalf("failed admission left state: rate=%d counters=%d quota=%d billing=%d usage=%d",
				rateWindows, counters, quotaReservations, billingReservations, usageRows)
		}

		billingIntegrationSetRate(t, ctx, repository, owner.ID, "1", past.Add(time.Second))
		billingIntegrationRecharge(t, ctx, repository, owner.ID, user.ID, "10", past.Add(2*time.Second))
		terminalRequestID := billingIntegrationRequestID(suffix, "atomic-terminal", 1)
		terminalAt := past.Add(3 * time.Second)
		if _, err := repository.AdmitRequest(ctx,
			billingIntegrationAdmission(user, device, key, terminalRequestID, terminalAt)); err != nil {
			t.Fatalf("AdmitRequest(terminal): %v", err)
		}
		if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: terminalRequestID, State: "completed", HTTPStatus: 200,
			CompletedAt: terminalAt.Add(time.Second), InputTokens: 1_000_000,
		}); err != nil {
			t.Fatalf("CompleteUsageRequest(terminal): %v", err)
		}

		staleRequestID := billingIntegrationRequestID(suffix, "atomic-stale", 1)
		staleAt := past.Add(5 * time.Second)
		if _, err := repository.AdmitRequest(ctx,
			billingIntegrationAdmission(user, device, key, staleRequestID, staleAt)); err != nil {
			t.Fatalf("AdmitRequest(stale): %v", err)
		}
		released, err := repository.ReleaseStaleQuotaReservations(ctx,
			past.Add(6*time.Second), 100)
		if err != nil {
			t.Fatalf("ReleaseStaleQuotaReservations: %v", err)
		}
		if released < 1 {
			t.Fatalf("stale releases = %d, want in-progress request to be released", released)
		}
		billingIntegrationAssertReservationStates(t, ctx, repository,
			staleRequestID, "released", "released")
		billingIntegrationAssertReservationStates(t, ctx, repository,
			terminalRequestID, "reserved", "reserved")

		settled, err := repository.RetryUnsettledRequests(ctx, 100)
		if err != nil {
			t.Fatalf("RetryUnsettledRequests: %v", err)
		}
		if settled < 1 {
			t.Fatalf("retried settlements = %d, want terminal request", settled)
		}
		billingIntegrationAssertReservationStates(t, ctx, repository,
			terminalRequestID, "settled", "settled")
		var completed, reserved int64
		if err := repository.db.QueryRowContext(ctx, `SELECT requests_completed,
			requests_reserved FROM quota_counters WHERE quota_day = $1::date
			AND scope_type = 'user' AND scope_id = $2`, terminalAt, user.ID,
		).Scan(&completed, &reserved); err != nil {
			t.Fatalf("read joint-settlement quota counter: %v", err)
		}
		if completed != 1 || reserved != 1 {
			t.Fatalf("joint quota counters = completed %d, reserved %d", completed, reserved)
		}
		var actualCost, charged, uncovered string
		if err := repository.db.QueryRowContext(ctx, `SELECT actual_cost_usd::text,
			charged_usd::text, uncovered_usd::text FROM billing_reservations
			WHERE request_id = $1`, terminalRequestID,
		).Scan(&actualCost, &charged, &uncovered); err != nil {
			t.Fatalf("read recovered billing settlement: %v", err)
		}
		if actualCost != "1.000000000000" || charged != "1.000000000000" ||
			uncovered != "0.000000000000" {
			t.Fatalf("recovered billing = cost %s charged %s uncovered %s",
				actualCost, charged, uncovered)
		}
	})

	t.Run("v2 internal zero admits without funds and settles concurrently", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "iz-"+suffix)
		base := now.Add(120 * time.Hour)
		model := "codex-auto-review"
		snapshot := billingIntegrationV2Snapshot(t, model, config.ModelPricing{
			CacheWriteMode: config.CacheWriteIncludedInInput, MaxInputTokens: 272_000,
			LongContextThresholdTokens: 272_000,
			ServiceTiers: map[string]config.ServiceTierPricing{
				config.PricingTierStandard: {Short: &config.TokenPricing{
					InputUSDPerMillion: "0", CachedInputUSDPerMillion: "0",
					OutputUSDPerMillion: "0",
				}},
			},
		})
		requestID := billingIntegrationRequestID(suffix, "internal-zero", 1)
		params := billingIntegrationAdmission(user, device, key, requestID, base)
		params.Usage.Model = model
		params.Usage.RequestedServiceTier = "default"
		params.Usage.PricingRuleVersion = config.PricingSchemaV2
		params.Billing = &BillingReservationParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: model,
			PricingRuleVersion: config.PricingSchemaV2, BillingMode: BillingModeInternalZero,
			PricingCatalogAsOf: "2026-08-20", PricingModel: model,
			PricingSnapshot: snapshot, CacheWriteMode: config.CacheWriteIncludedInInput,
			RequestedServiceTier: "default", Now: base,
		}
		admission, err := repository.AdmitRequest(ctx, params)
		if err != nil {
			t.Fatalf("AdmitRequest(internal zero without funds): %v", err)
		}
		if admission.Billing == nil || admission.Billing.DayPeriodID != nil ||
			admission.Billing.WeekPeriodID != nil || admission.Billing.MonthPeriodID != nil ||
			admission.Billing.CashLotCutoff != nil {
			t.Fatalf("internal-zero admission unexpectedly bound funds: %+v", admission.Billing)
		}
		if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: requestID, State: "completed", HTTPStatus: 200,
			CompletedAt: base.Add(time.Second), InputTokens: 120,
			CachedInputTokens: 20, CacheWriteTokens: 30, CacheWriteTokensPresent: true,
			OutputTokens: 40, ActualModel: model,
		}); err != nil {
			t.Fatalf("CompleteUsageRequest(internal zero): %v", err)
		}
		start := make(chan struct{})
		errorsByAttempt := make([]error, 2)
		var wait sync.WaitGroup
		for index := range errorsByAttempt {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				<-start
				errorsByAttempt[index] = repository.SettleRequest(ctx, requestID, base.Add(2*time.Second))
			}(index)
		}
		close(start)
		wait.Wait()
		for index, err := range errorsByAttempt {
			if err != nil {
				t.Fatalf("concurrent internal-zero settlement %d: %v", index, err)
			}
		}
		var ledgerCount int
		var amount, charged, uncovered, balance string
		var inputTokens, cachedTokens, cacheWriteTokens, outputTokens, actualQuotaTokens int64
		var pricingTier, contextClass, cacheWriteMode string
		var pricingVersion int
		if err := repository.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM billing_ledger_entries WHERE request_id = $1),
			l.amount_usd::text,l.charged_usd::text,l.uncovered_usd::text,
			l.input_tokens,l.cached_input_tokens,l.cache_write_tokens,l.output_tokens,
			l.pricing_service_tier,l.context_class,l.cache_write_mode,l.pricing_rule_version,
			(SELECT balance_usd::text FROM billing_accounts WHERE user_id = $2),
			q.actual_tokens
			FROM billing_ledger_entries l JOIN quota_reservations q USING (request_id)
			WHERE l.request_id = $1`, requestID, user.ID,
		).Scan(&ledgerCount, &amount, &charged, &uncovered, &inputTokens, &cachedTokens,
			&cacheWriteTokens, &outputTokens, &pricingTier, &contextClass, &cacheWriteMode,
			&pricingVersion, &balance, &actualQuotaTokens); err != nil {
			t.Fatalf("read internal-zero settlement: %v", err)
		}
		if ledgerCount != 1 || amount != "0.000000000000" || charged != "0.000000000000" ||
			uncovered != "0.000000000000" || balance != "0.000000000000" ||
			inputTokens != 120 || cachedTokens != 20 || cacheWriteTokens != 30 || outputTokens != 40 ||
			actualQuotaTokens != 160 || pricingTier != config.PricingTierStandard ||
			contextClass != config.ContextClassShort || cacheWriteMode != config.CacheWriteIncludedInInput ||
			pricingVersion != config.PricingSchemaV2 {
			t.Fatalf("internal-zero settlement mismatch: ledger=%d amount=%s charged=%s uncovered=%s balance=%s tokens=%d/%d/%d/%d quota=%d tier=%s context=%s cache=%s version=%d",
				ledgerCount, amount, charged, uncovered, balance, inputTokens, cachedTokens,
				cacheWriteTokens, outputTokens, actualQuotaTokens, pricingTier, contextClass,
				cacheWriteMode, pricingVersion)
		}
		var completedRequests, usedTokens int64
		if err := repository.db.QueryRowContext(ctx, `SELECT requests_completed,tokens_used
			FROM quota_counters WHERE quota_day=$1::date AND scope_type='user' AND scope_id=$2`,
			base, user.ID).Scan(&completedRequests, &usedTokens); err != nil {
			t.Fatalf("read internal-zero user quota: %v", err)
		}
		if completedRequests != 1 || usedTokens != 160 {
			t.Fatalf("internal-zero quota = requests %d tokens %d, want 1/160",
				completedRequests, usedTokens)
		}
	})

	t.Run("v2 fallback persists applied prices through usage retention", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "v2-fallback-"+suffix)
		base := now.Add(144 * time.Hour)
		if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "fund v2 fallback test", base),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "5",
		}); err != nil {
			t.Fatalf("PutSubscription(v2 fallback): %v", err)
		}
		writePrice := func(value string) *string { return &value }
		model := "gpt-v2-priced"
		snapshot := billingIntegrationV2Snapshot(t, model, config.ModelPricing{
			CacheWriteMode: config.CacheWriteSeparate, MaxInputTokens: 1_050_000,
			LongContextThresholdTokens: 272_000,
			ServiceTiers: map[string]config.ServiceTierPricing{
				config.PricingTierStandard: {Short: &config.TokenPricing{
					InputUSDPerMillion: "5", CachedInputUSDPerMillion: "0.5",
					CacheWriteUSDPerMillion: writePrice("6.25"), OutputUSDPerMillion: "30",
				}},
				config.PricingTierFlex: {Short: &config.TokenPricing{
					InputUSDPerMillion: "2.5", CachedInputUSDPerMillion: "0.25",
					CacheWriteUSDPerMillion: writePrice("3.125"), OutputUSDPerMillion: "15",
				}},
				config.PricingTierFast: {Short: &config.TokenPricing{
					InputUSDPerMillion: "10", CachedInputUSDPerMillion: "1",
					CacheWriteUSDPerMillion: writePrice("12.5"), OutputUSDPerMillion: "60",
				}},
			},
		})
		requestID := billingIntegrationRequestID(suffix, "v2-fallback", 1)
		params := billingIntegrationAdmission(user, device, key, requestID, base.Add(time.Second))
		params.Usage.Model = model
		params.Usage.RequestedServiceTier = "flex"
		params.Usage.PricingRuleVersion = config.PricingSchemaV2
		params.Billing = &BillingReservationParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: model,
			PricingRuleVersion: config.PricingSchemaV2,
			BillingMode:        BillingModeOpenAIAPIEquivalent, PricingCatalogAsOf: "2026-08-20",
			PricingModel: model, PricingSnapshot: snapshot,
			CacheWriteMode: config.CacheWriteSeparate, RequestedServiceTier: "flex",
			Now: base.Add(time.Second),
		}
		if _, err := repository.AdmitRequest(ctx, params); err != nil {
			t.Fatalf("AdmitRequest(v2 fallback): %v", err)
		}
		actualModel := "gpt-v2-upstream"
		completedAt := base.Add(2 * time.Second)
		if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: requestID, State: "completed", HTTPStatus: 200,
			CompletedAt: completedAt, InputTokens: 100_000, CachedInputTokens: 10_000,
			OutputTokens: 10_000, ActualModel: actualModel,
		}); err != nil {
			t.Fatalf("CompleteUsageRequest(v2 fallback): %v", err)
		}
		if err := repository.SettleRequest(ctx, requestID, base.Add(3*time.Second)); err != nil {
			t.Fatalf("SettleRequest(v2 fallback): %v", err)
		}
		fallbackReason := "missing_cache_write_tokens,missing_service_tier"
		var actualCost, charged, uncovered, reservationFallback string
		var actualCacheWrite int64
		var appliedInput, appliedCached, appliedWrite, appliedOutput string
		var pricingTier, contextClass string
		if err := repository.db.QueryRowContext(ctx, `SELECT actual_cost_usd::text,
			charged_usd::text,uncovered_usd::text,actual_cache_write_tokens,
			applied_input_usd_per_million::text,applied_cached_input_usd_per_million::text,
			applied_cache_write_usd_per_million::text,applied_output_usd_per_million::text,
			pricing_service_tier,context_class,pricing_fallback_reason
			FROM billing_reservations WHERE request_id=$1`, requestID,
		).Scan(&actualCost, &charged, &uncovered, &actualCacheWrite, &appliedInput,
			&appliedCached, &appliedWrite, &appliedOutput, &pricingTier, &contextClass,
			&reservationFallback); err != nil {
			t.Fatalf("read v2 fallback reservation: %v", err)
		}
		if actualCost != "1.735000000000" || charged != "1.735000000000" ||
			uncovered != "0.000000000000" || actualCacheWrite != 90_000 ||
			appliedInput != "10.000000000000" || appliedCached != "1.000000000000" ||
			appliedWrite != "12.500000000000" || appliedOutput != "60.000000000000" ||
			pricingTier != config.PricingTierMaxPublished || contextClass != config.ContextClassShort ||
			reservationFallback != fallbackReason {
			t.Fatalf("v2 fallback reservation mismatch: cost=%s/%s/%s write=%d prices=%s/%s/%s/%s tier=%s context=%s fallback=%s",
				actualCost, charged, uncovered, actualCacheWrite, appliedInput, appliedCached,
				appliedWrite, appliedOutput, pricingTier, contextClass, reservationFallback)
		}
		var usageCacheWrite int64
		var usageCacheWritePresent bool
		var usagePricingTier, usageContext, usageFallback string
		if err := repository.db.QueryRowContext(ctx, `SELECT cache_write_tokens,
			cache_write_tokens_present,pricing_service_tier,context_class,pricing_fallback_reason
			FROM usage_requests WHERE request_id=$1`, requestID,
		).Scan(&usageCacheWrite, &usageCacheWritePresent, &usagePricingTier,
			&usageContext, &usageFallback); err != nil {
			t.Fatalf("read v2 fallback usage metadata: %v", err)
		}
		if usageCacheWrite != 90_000 || usageCacheWritePresent ||
			usagePricingTier != config.PricingTierMaxPublished ||
			usageContext != config.ContextClassShort || usageFallback != fallbackReason {
			t.Fatalf("v2 fallback usage mismatch: write=%d present=%t tier=%s context=%s fallback=%s",
				usageCacheWrite, usageCacheWritePresent, usagePricingTier, usageContext, usageFallback)
		}
		var ledgerID int64
		var ledgerActualModel, ledgerRequestedTier, ledgerPricingTier, ledgerFallback string
		var ledgerUsageAt time.Time
		if err := repository.db.QueryRowContext(ctx, `SELECT id,actual_model,
			requested_service_tier,pricing_service_tier,pricing_fallback_reason,usage_requested_at
			FROM billing_ledger_entries WHERE request_id=$1`, requestID,
		).Scan(&ledgerID, &ledgerActualModel, &ledgerRequestedTier, &ledgerPricingTier,
			&ledgerFallback, &ledgerUsageAt); err != nil {
			t.Fatalf("read v2 fallback ledger metadata: %v", err)
		}
		if ledgerActualModel != actualModel || ledgerRequestedTier != "flex" ||
			ledgerPricingTier != config.PricingTierMaxPublished || ledgerFallback != fallbackReason ||
			!ledgerUsageAt.Equal(base.Add(time.Second)) {
			t.Fatalf("v2 fallback ledger metadata = model %s requested %s pricing %s fallback %s usage %v",
				ledgerActualModel, ledgerRequestedTier, ledgerPricingTier, ledgerFallback, ledgerUsageAt)
		}
		if _, err := repository.db.ExecContext(ctx, `UPDATE billing_ledger_entries
			SET actual_cost_usd=999 WHERE id=$1`, ledgerID); err == nil {
			t.Fatal("immutable usage ledger unexpectedly allowed an amount update")
		}
		if err := repository.AggregateUsageMonth(ctx, base, "UTC"); err != nil {
			t.Fatalf("AggregateUsageMonth(v2 retention): %v", err)
		}
		deleted, err := repository.DeleteUsageRequestsBefore(ctx, completedAt.Add(91*24*time.Hour), 100_000)
		if err != nil {
			t.Fatalf("DeleteUsageRequestsBefore(v2 retention): %v", err)
		}
		if deleted < 1 {
			t.Fatalf("retention deleted %d usage rows, want at least the v2 request", deleted)
		}
		var usageCount int
		if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM usage_requests
			WHERE request_id=$1`, requestID).Scan(&usageCount); err != nil {
			t.Fatalf("count retained v2 usage detail: %v", err)
		}
		if usageCount != 0 {
			t.Fatalf("v2 usage detail count after retention = %d, want zero", usageCount)
		}
		var retainedCost, retainedFallback string
		var retainedWrite int64
		if err := repository.db.QueryRowContext(ctx, `SELECT actual_cost_usd::text,
			cache_write_tokens,pricing_fallback_reason FROM billing_ledger_entries WHERE id=$1`,
			ledgerID).Scan(&retainedCost, &retainedWrite, &retainedFallback); err != nil {
			t.Fatalf("read immutable ledger after retention: %v", err)
		}
		if retainedCost != "1.735000000000" || retainedWrite != 90_000 || retainedFallback != fallbackReason {
			t.Fatalf("ledger after retention = cost %s write %d fallback %s",
				retainedCost, retainedWrite, retainedFallback)
		}
		var monthlyInput, monthlyCached, monthlyWrite, monthlyOutput int64
		if err := repository.db.QueryRowContext(ctx, `SELECT coalesce(sum(input_tokens),0),
			coalesce(sum(cached_input_tokens),0),coalesce(sum(cache_write_tokens),0),
			coalesce(sum(output_tokens),0) FROM usage_monthly
			WHERE user_id=$1 AND model=$2`, user.ID, actualModel,
		).Scan(&monthlyInput, &monthlyCached, &monthlyWrite, &monthlyOutput); err != nil {
			t.Fatalf("read monthly usage after retention: %v", err)
		}
		if monthlyInput != 100_000 || monthlyCached != 10_000 ||
			monthlyWrite != 90_000 || monthlyOutput != 10_000 {
			t.Fatalf("monthly usage after retention = %d/%d/%d/%d",
				monthlyInput, monthlyCached, monthlyWrite, monthlyOutput)
		}
		liveFrom := time.Date(base.Year(), base.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		reportRows, err := repository.GlobalUsage(ctx, time.Time{},
			completedAt.Add(91*24*time.Hour), actualModel, true, liveFrom)
		if err != nil {
			t.Fatalf("GlobalUsage(after retention): %v", err)
		}
		report := findGlobalUsageIntegrationRow(t, reportRows, user.ID)
		if report.RequestCount != 1 || report.InputTokens != 100_000 ||
			report.CachedInputTokens != 10_000 || report.CacheWriteTokens != 90_000 ||
			report.OutputTokens != 10_000 || report.LedgerTokens != 110_000 ||
			report.ActualCostUSD != "1.735000000000" ||
			report.ChargedUSD != "1.735000000000" || report.UncoveredUSD != "0.000000000000" {
			t.Fatalf("global usage after retention did not reconcile monthly tokens and immutable ledger: %+v",
				report)
		}
		breakdown, err := repository.GlobalPricingBreakdown(ctx,
			base.Add(-time.Second), completedAt.Add(91*24*time.Hour), actualModel, false)
		if err != nil {
			t.Fatalf("GlobalPricingBreakdown(after retention): %v", err)
		}
		wantBreakdown := map[string]bool{
			"service_tier/" + config.PricingTierMaxPublished: false,
			"context_class/" + config.ContextClassShort:      false,
			"fallback/missing_cache_write_tokens":            false,
			"fallback/missing_service_tier":                  false,
		}
		for _, row := range breakdown {
			key := row.Dimension + "/" + row.Value
			if _, ok := wantBreakdown[key]; ok && row.RequestCount == 1 &&
				row.CacheWriteTokens == 90_000 && row.ActualCostUSD == "1.735000000000" {
				wantBreakdown[key] = true
			}
		}
		for key, found := range wantBreakdown {
			if !found {
				t.Errorf("missing immutable pricing breakdown %s in %+v", key, breakdown)
			}
		}
	})
}

func billingIntegrationV2Snapshot(t *testing.T, model string, rule config.ModelPricing) []byte {
	t.Helper()
	pricing := config.UsagePricing{
		SchemaVersion: config.PricingSchemaV2,
		FallbackPolicy: config.PricingFallbackPolicy{
			UnknownServiceTier:      config.FallbackMaxPublished,
			MissingPriceCombination: config.FallbackMaxPublished,
			MissingCacheWriteTokens: config.FallbackAllUncachedAsWrite,
		},
		Models: map[string]config.ModelPricing{model: rule},
	}
	snapshot, _, ok, err := pricing.ModelSnapshot(model)
	if err != nil || !ok {
		t.Fatalf("build v2 pricing snapshot for %s: ok=%t err=%v", model, ok, err)
	}
	if _, err := config.ParsePricingSnapshot(snapshot); err != nil {
		t.Fatalf("validate v2 pricing snapshot for %s: %v", model, err)
	}
	return snapshot
}

func billingIntegrationAccountMigration(t *testing.T, ctx context.Context, repository *Store, suffix string) {
	t.Helper()
	connection, err := repository.db.Conn(ctx)
	if err != nil {
		t.Fatalf("dedicated migration connection: %v", err)
	}
	schema := "billing_accounts_" + suffix
	if _, err := connection.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		_ = connection.Close()
		t.Fatalf("create migration test schema: %v", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = connection.ExecContext(cleanupCtx, `RESET search_path`)
		_ = connection.Close()
		_, _ = repository.db.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`)
	}()
	if _, err := connection.ExecContext(ctx, `SET search_path TO `+schema+`, public`); err != nil {
		t.Fatalf("set migration test search path: %v", err)
	}
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatalf("EmbeddedMigrations: %v", err)
	}
	byName := make(map[string]string, len(migrations))
	for _, migration := range migrations {
		byName[migration.Name] = migration.SQL
	}
	if byName["0001_initial.sql"] == "" || byName["0002_billing.sql"] == "" ||
		byName["0004_subscription_period_limits.sql"] == "" ||
		byName["0005_official_token_pricing.sql"] == "" ||
		byName["0006_api_key_lifecycle.sql"] == "" {
		t.Fatalf("billing migration set is incomplete: %v", byName)
	}
	if _, err := connection.ExecContext(ctx, byName["0001_initial.sql"]); err != nil {
		t.Fatalf("apply isolated 0001: %v", err)
	}
	legacyID, _ := newUUID()
	legacyHandle := make([]byte, 32)
	legacyHandle[0] = 1
	if _, err := connection.ExecContext(ctx, `INSERT INTO users
		(id,username,display_name,webauthn_user_id,role)
		VALUES ($1,$2,$2,$3,'member')`, legacyID, "legacy-"+suffix, legacyHandle); err != nil {
		t.Fatalf("create pre-billing user: %v", err)
	}
	if _, err := connection.ExecContext(ctx, byName["0002_billing.sql"]); err != nil {
		t.Fatalf("apply isolated 0002: %v", err)
	}
	var legacyBalance string
	if err := connection.QueryRowContext(ctx, `SELECT balance_usd::text
		FROM billing_accounts WHERE user_id = $1`, legacyID).Scan(&legacyBalance); err != nil {
		t.Fatalf("read backfilled account: %v", err)
	}
	newID, _ := newUUID()
	newHandle := make([]byte, 32)
	newHandle[0] = 2
	if _, err := connection.ExecContext(ctx, `INSERT INTO users
		(id,username,display_name,webauthn_user_id,role)
		VALUES ($1,$2,$2,$3,'member')`, newID, "trigger-"+suffix, newHandle); err != nil {
		t.Fatalf("create post-billing user: %v", err)
	}
	var newBalance string
	if err := connection.QueryRowContext(ctx, `SELECT balance_usd::text
		FROM billing_accounts WHERE user_id = $1`, newID).Scan(&newBalance); err != nil {
		t.Fatalf("read trigger-created account: %v", err)
	}
	if legacyBalance != "0.000000000000" || newBalance != "0.000000000000" {
		t.Fatalf("initial balances = legacy %s, new %s", legacyBalance, newBalance)
	}

	migrationNow := time.Now().UTC().Truncate(time.Microsecond)
	activeSubscriptionID, _ := newUUID()
	activePeriodID, _ := newUUID()
	activeStart := migrationNow.Add(-time.Hour)
	activeEnd := activeStart.Add(24 * time.Hour)
	if _, err := connection.ExecContext(ctx, `INSERT INTO billing_subscriptions
		(id,user_id,tier,enabled,allowance_usd,created_at,updated_at)
		VALUES ($1,$2,'day',true,5,$3,$3)`, activeSubscriptionID, legacyID, activeStart); err != nil {
		t.Fatalf("create active pre-0004 subscription: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO billing_subscription_periods
		(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,created_at)
		VALUES ($1,$2,$3,'day',$4,$5,5,3.5,$4)`, activePeriodID,
		activeSubscriptionID, legacyID, activeStart, activeEnd); err != nil {
		t.Fatalf("create active pre-0004 period: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `UPDATE billing_subscriptions
		SET current_period_id = $2 WHERE id = $1`, activeSubscriptionID, activePeriodID); err != nil {
		t.Fatalf("bind active pre-0004 period: %v", err)
	}

	expiredSubscriptionID, _ := newUUID()
	expiredPeriodID, _ := newUUID()
	expiredStart := migrationNow.Add(-8 * 24 * time.Hour)
	expiredEnd := expiredStart.Add(7 * 24 * time.Hour)
	if _, err := connection.ExecContext(ctx, `INSERT INTO billing_subscriptions
		(id,user_id,tier,enabled,allowance_usd,created_at,updated_at)
		VALUES ($1,$2,'week',true,8,$3,$3)`, expiredSubscriptionID, newID, expiredStart); err != nil {
		t.Fatalf("create expired pre-0004 subscription: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO billing_subscription_periods
		(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,created_at)
		VALUES ($1,$2,$3,'week',$4,$5,8,6,$4)`, expiredPeriodID,
		expiredSubscriptionID, newID, expiredStart, expiredEnd); err != nil {
		t.Fatalf("create expired pre-0004 period: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `UPDATE billing_subscriptions
		SET current_period_id = $2 WHERE id = $1`, expiredSubscriptionID, expiredPeriodID); err != nil {
		t.Fatalf("bind expired pre-0004 period: %v", err)
	}

	periodlessSubscriptionID, _ := newUUID()
	periodlessDisabledAt := migrationNow.Add(-2 * time.Hour)
	if _, err := connection.ExecContext(ctx, `INSERT INTO billing_subscriptions
		(id,user_id,tier,enabled,allowance_usd,created_at,updated_at,disabled_at)
		VALUES ($1,$2,'month',false,9,$3,$3,$3)`, periodlessSubscriptionID,
		legacyID, periodlessDisabledAt); err != nil {
		t.Fatalf("create disabled periodless pre-0004 subscription: %v", err)
	}
	var periodlessLedgerID int64
	if err := connection.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries
		(user_id,entry_type,amount_usd,subscription_tier,reason,created_at)
		VALUES ($1,'subscription_disable',9,'month','legacy periodless disable',$2)
		RETURNING id`, legacyID, periodlessDisabledAt).Scan(&periodlessLedgerID); err != nil {
		t.Fatalf("create periodless pre-0004 disable ledger: %v", err)
	}

	if _, err := connection.ExecContext(ctx, byName["0004_subscription_period_limits.sql"]); err != nil {
		t.Fatalf("apply isolated 0004: %v", err)
	}
	var activeEnabled bool
	var activePeriodCount, activeNumber, activeSnapshotCount, activeSnapshotNumber int
	var migratedStart, migratedEnd, activeExpiry time.Time
	var activeRemaining string
	if err := connection.QueryRowContext(ctx, `SELECT s.enabled, s.period_count,
		s.current_period_number, s.expires_at, p.starts_at, p.ends_at,
		p.remaining_usd::text, p.period_count, p.period_number
		FROM billing_subscriptions s
		JOIN billing_subscription_periods p ON p.id = s.current_period_id
		WHERE s.id = $1`, activeSubscriptionID).Scan(&activeEnabled, &activePeriodCount,
		&activeNumber, &activeExpiry, &migratedStart, &migratedEnd, &activeRemaining,
		&activeSnapshotCount, &activeSnapshotNumber); err != nil {
		t.Fatalf("read migrated active subscription: %v", err)
	}
	if !activeEnabled || activePeriodCount != 1 || activeNumber != 1 ||
		activeSnapshotCount != 1 || activeSnapshotNumber != 1 ||
		!activeExpiry.Equal(activeEnd) || !migratedStart.Equal(activeStart) ||
		!migratedEnd.Equal(activeEnd) || activeRemaining != "3.500000000000" {
		t.Fatalf("migrated active subscription changed: enabled %v period %d/%d snapshot %d/%d expiry %v start %v end %v remaining %s",
			activeEnabled, activeNumber, activePeriodCount, activeSnapshotNumber,
			activeSnapshotCount, activeExpiry, migratedStart, migratedEnd, activeRemaining)
	}
	var expiredEnabled bool
	var disabledAt, closedAt, expiredExpiry time.Time
	var closeReason string
	if err := connection.QueryRowContext(ctx, `SELECT s.enabled, s.disabled_at,
		s.expires_at, p.closed_at, p.close_reason
		FROM billing_subscriptions s
		JOIN billing_subscription_periods p ON p.id = s.current_period_id
		WHERE s.id = $1`, expiredSubscriptionID).Scan(&expiredEnabled, &disabledAt,
		&expiredExpiry, &closedAt, &closeReason); err != nil {
		t.Fatalf("read migrated expired subscription: %v", err)
	}
	if expiredEnabled || !disabledAt.Equal(expiredEnd) || !expiredExpiry.Equal(expiredEnd) ||
		!closedAt.Equal(expiredEnd) || closeReason != "expired" {
		t.Fatalf("migrated expired subscription = enabled %v disabled %v expiry %v closed %v reason %q",
			expiredEnabled, disabledAt, expiredExpiry, closedAt, closeReason)
	}
	var snapshotPeriodCount, snapshotPeriodNumber int
	var snapshotExpiry time.Time
	if err := connection.QueryRowContext(ctx, `SELECT period_count,
		current_period_number, expires_at
		FROM billing_subscription_operation_snapshots
		WHERE ledger_entry_id = $1`, periodlessLedgerID,
	).Scan(&snapshotPeriodCount, &snapshotPeriodNumber, &snapshotExpiry); err != nil {
		t.Fatalf("read migrated periodless operation snapshot: %v", err)
	}
	if snapshotPeriodCount != 1 || snapshotPeriodNumber != 1 ||
		!snapshotExpiry.Equal(periodlessDisabledAt) {
		t.Fatalf("migrated periodless snapshot = period %d/%d expiry %v, want 1/1 %v",
			snapshotPeriodNumber, snapshotPeriodCount, snapshotExpiry, periodlessDisabledAt)
	}

	legacyDeviceID, _ := newUUID()
	legacyKeyID, _ := newUUID()
	legacyKeyHash := sha256.Sum256([]byte("pre-0005-key-" + suffix))
	legacyKeyPrefix := "cgk_mig_" + suffix
	if _, err := connection.ExecContext(ctx, `INSERT INTO devices
		(id,user_id,name,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$4)`, legacyDeviceID, legacyID,
		"pre-0005-device-"+suffix, migrationNow); err != nil {
		t.Fatalf("create pre-0005 device: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO api_keys
		(id,public_id,key_prefix,key_hash,user_id,device_id,name,created_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, legacyKeyID,
		"migrationkey"+suffix, legacyKeyPrefix, legacyKeyHash[:], legacyID,
		legacyDeviceID, "pre-0005-key", migrationNow, migrationNow.Add(90*24*time.Hour)); err != nil {
		t.Fatalf("create pre-0005 API key: %v", err)
	}
	legacyRequestID := "pre-0005-v1-" + suffix
	if _, err := connection.ExecContext(ctx, `INSERT INTO billing_reservations
		(request_id,user_id,api_key_id,requested_model,input_usd_per_million,
		 cached_input_usd_per_million,output_usd_per_million,day_period_id,created_at)
		VALUES ($1,$2,$3,'legacy-priced-model',2,0.5,3,$4,$5)`,
		legacyRequestID, legacyID, legacyKeyID, activePeriodID, migrationNow); err != nil {
		t.Fatalf("create pre-0005 v1 reservation: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO usage_requests
		(request_id,user_id,device_id,api_key_id,key_prefix,model,endpoint,requested_at)
		VALUES ($1,$2,$3,$4,$5,'legacy-priced-model','responses',$6)`,
		legacyRequestID, legacyID, legacyDeviceID, legacyKeyID,
		legacyKeyPrefix, migrationNow); err != nil {
		t.Fatalf("create pre-0005 in-flight usage: %v", err)
	}
	historicalRequestID := "pre-0005-ledger-" + suffix
	if _, err := connection.ExecContext(ctx, `INSERT INTO billing_ledger_entries
		(user_id,entry_type,amount_usd,cash_delta_usd,request_id,model,input_tokens,
		 cached_input_tokens,output_tokens,actual_cost_usd,charged_usd,uncovered_usd,
		 reason,created_at)
		VALUES ($1,'usage_charge',1.25,0,$2,'historic-model',1000000,0,0,
		 1.25,1.25,0,'pre-0005 immutable history',$3)`,
		legacyID, historicalRequestID, migrationNow.Add(-time.Minute)); err != nil {
		t.Fatalf("create pre-0005 historical ledger: %v", err)
	}
	var ledgerCountBefore int64
	var ledgerAmountBefore string
	if err := connection.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(amount_usd),0)::text FROM billing_ledger_entries`,
	).Scan(&ledgerCountBefore, &ledgerAmountBefore); err != nil {
		t.Fatalf("snapshot pre-0005 ledger totals: %v", err)
	}

	if _, err := connection.ExecContext(ctx, byName["0005_official_token_pricing.sql"]); err != nil {
		t.Fatalf("apply isolated 0005: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `UPDATE api_keys
		SET status='revoked',revoked_at=$2,revoke_reason='migration test'
		WHERE id=$1`, legacyKeyID, migrationNow); err != nil {
		t.Fatalf("revoke pre-0006 API key: %v", err)
	}
	if _, err := connection.ExecContext(ctx, byName["0006_api_key_lifecycle.sql"]); err != nil {
		t.Fatalf("apply isolated 0006: %v", err)
	}
	var activeKeyCount, historyCount int
	var secretAvailable bool
	if err := connection.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM api_keys WHERE id=$1),
		(SELECT count(*) FROM api_key_history WHERE id=$1),
		EXISTS (SELECT 1 FROM api_keys WHERE id=$1 AND secret_ciphertext IS NOT NULL)`,
		legacyKeyID,
	).Scan(&activeKeyCount, &historyCount, &secretAvailable); err != nil {
		t.Fatalf("read migrated API key lifecycle: %v", err)
	}
	if activeKeyCount != 0 || historyCount != 1 || secretAvailable {
		t.Fatalf("migrated API key lifecycle = active %d history %d secret %v, want 0/1/false",
			activeKeyCount, historyCount, secretAvailable)
	}
	if _, err := connection.ExecContext(ctx, `UPDATE api_key_history
		SET key_prefix='mutated-prefix' WHERE id=$1`, legacyKeyID); err == nil {
		t.Fatal("immutable API key history accepted an update")
	}
	if _, err := connection.ExecContext(ctx,
		`DELETE FROM api_key_history WHERE id=$1`, legacyKeyID,
	); err == nil {
		t.Fatal("immutable API key history accepted a delete")
	}
	var pricingVersion int
	var billingMode, inputPrice, cachedPrice, outputPrice string
	var catalog, pricingModel, snapshot, cacheWriteMode sql.NullString
	if err := connection.QueryRowContext(ctx, `SELECT pricing_rule_version,billing_mode,
		input_usd_per_million::text,cached_input_usd_per_million::text,
		output_usd_per_million::text,pricing_catalog_as_of::text,pricing_model,
		pricing_snapshot::text,cache_write_mode
		FROM billing_reservations WHERE request_id = $1`, legacyRequestID,
	).Scan(&pricingVersion, &billingMode, &inputPrice, &cachedPrice, &outputPrice,
		&catalog, &pricingModel, &snapshot, &cacheWriteMode); err != nil {
		t.Fatalf("read migrated v1 reservation: %v", err)
	}
	if pricingVersion != config.PricingSchemaV1 || billingMode != BillingModeLegacy ||
		inputPrice != "2.000000000000" || cachedPrice != "0.500000000000" ||
		outputPrice != "3.000000000000" || catalog.Valid || pricingModel.Valid ||
		snapshot.Valid || cacheWriteMode.Valid {
		t.Fatalf("migrated v1 reservation changed pricing: version=%d mode=%s prices=%s/%s/%s catalog=%v model=%v snapshot=%v cache=%v",
			pricingVersion, billingMode, inputPrice, cachedPrice, outputPrice,
			catalog, pricingModel, snapshot, cacheWriteMode)
	}
	var ledgerCountAfter int64
	var ledgerAmountAfter string
	if err := connection.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(amount_usd),0)::text FROM billing_ledger_entries`,
	).Scan(&ledgerCountAfter, &ledgerAmountAfter); err != nil {
		t.Fatalf("read post-0005 ledger totals: %v", err)
	}
	if ledgerCountAfter != ledgerCountBefore || ledgerAmountAfter != ledgerAmountBefore {
		t.Fatalf("0005 rewrote immutable ledger totals: before=%d/%s after=%d/%s",
			ledgerCountBefore, ledgerAmountBefore, ledgerCountAfter, ledgerAmountAfter)
	}
	var historicalVersion int
	var historicalUsageAt sql.NullTime
	var historicalAppliedPrice sql.NullString
	if err := connection.QueryRowContext(ctx, `SELECT pricing_rule_version,
		usage_requested_at,applied_input_usd_per_million::text
		FROM billing_ledger_entries WHERE request_id = $1`, historicalRequestID,
	).Scan(&historicalVersion, &historicalUsageAt, &historicalAppliedPrice); err != nil {
		t.Fatalf("read migrated historical ledger metadata: %v", err)
	}
	if historicalVersion != config.PricingSchemaV1 || historicalUsageAt.Valid || historicalAppliedPrice.Valid {
		t.Fatalf("historical ledger was relabeled: version=%d usage_at=%v applied=%v",
			historicalVersion, historicalUsageAt, historicalAppliedPrice)
	}

	settledAt := migrationNow.Add(time.Second)
	if _, err := connection.ExecContext(ctx, `UPDATE usage_requests
		SET state='completed',http_status=200,completed_at=$2,input_tokens=1000000,
		cached_input_tokens=0,output_tokens=0
		WHERE request_id=$1`, legacyRequestID, settledAt); err != nil {
		t.Fatalf("complete migrated v1 usage: %v", err)
	}
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin migrated v1 settlement: %v", err)
	}
	settled, settleErr := settleBillingTx(ctx, tx, legacyRequestID, settledAt.Add(time.Second))
	if settleErr != nil {
		_ = tx.Rollback()
		t.Fatalf("settle migrated v1 reservation: %v", settleErr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit migrated v1 settlement: %v", err)
	}
	if settled.PricingRuleVersion != config.PricingSchemaV1 ||
		settled.ActualCostUSD == nil || *settled.ActualCostUSD != "2.000000000000" ||
		settled.ChargedUSD == nil || *settled.ChargedUSD != "2.000000000000" ||
		settled.UncoveredUSD == nil || *settled.UncoveredUSD != "0.000000000000" ||
		settled.ActualCacheWriteTokens != nil || settled.AppliedInputUSDPerMillion != nil {
		t.Fatalf("migrated v1 settlement used v2 semantics: %+v", settled)
	}
	var settledLedgerCount int
	var settledLedgerVersion int
	var settledLedgerApplied sql.NullString
	if err := connection.QueryRowContext(ctx, `SELECT count(*),min(pricing_rule_version),
		min(applied_input_usd_per_million)::text
		FROM billing_ledger_entries WHERE request_id = $1`, legacyRequestID,
	).Scan(&settledLedgerCount, &settledLedgerVersion, &settledLedgerApplied); err != nil {
		t.Fatalf("read migrated v1 settlement ledger: %v", err)
	}
	if settledLedgerCount != 1 || settledLedgerVersion != config.PricingSchemaV1 || settledLedgerApplied.Valid {
		t.Fatalf("migrated v1 ledger = count %d version %d applied %v",
			settledLedgerCount, settledLedgerVersion, settledLedgerApplied)
	}
}

func billingIntegrationPrincipal(t *testing.T, ctx context.Context, repository *Store, suffix string) (User, Device, APIKey) {
	t.Helper()
	user := globalUsageIntegrationUser(t, ctx, repository, "bill-"+suffix, UserRoleMember)
	digest := sha256.Sum256([]byte(suffix))
	keySuffix := fmt.Sprintf("%x", digest[:8])
	device, key := globalUsageIntegrationKey(t, ctx, repository, user, keySuffix)
	return user, device, key
}

func billingIntegrationWrite(t *testing.T, actorID, reason string, at time.Time) BillingWriteParams {
	t.Helper()
	operationID, err := newUUID()
	if err != nil {
		t.Fatalf("new operation UUID: %v", err)
	}
	return BillingWriteParams{OperationID: operationID, ActorUserID: actorID, Reason: reason, At: at}
}

func billingIntegrationSetRate(t *testing.T, ctx context.Context, repository *Store, actorID, rate string, at time.Time) {
	t.Helper()
	if _, err := repository.SetRechargeRate(ctx, SetRechargeRateParams{
		BillingWriteParams: billingIntegrationWrite(t, actorID, "set test recharge rate", at),
		USDPerCNY:          rate,
	}); err != nil {
		t.Fatalf("SetRechargeRate(%s): %v", rate, err)
	}
}

func billingIntegrationRecharge(t *testing.T, ctx context.Context, repository *Store, actorID, userID, cny string, at time.Time) BillingLedgerEntry {
	t.Helper()
	entry, err := repository.RechargeUser(ctx, RechargeUserParams{
		BillingWriteParams: billingIntegrationWrite(t, actorID, "test recharge", at),
		UserID:             userID,
		CNYAmount:          cny,
	})
	if err != nil {
		t.Fatalf("RechargeUser(%s): %v", cny, err)
	}
	return entry
}

func billingIntegrationRequestID(suffix, scenario string, sequence int) string {
	return fmt.Sprintf("billing-%s-%s-%02d", suffix, scenario, sequence)
}

func billingIntegrationAdmission(user User, device Device, key APIKey, requestID string, at time.Time) AdmitRequestParams {
	return AdmitRequestParams{
		Quota: ReserveQuotaParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID,
			Day: at, Now: at, LeaseTTL: time.Minute,
			Limits: QuotaLimits{
				KeyRequestsPerMinute: 100, UserRequestsPerMinute: 100,
				KeyConcurrent: 10, UserConcurrent: 10, GlobalConcurrent: 100,
				KeyDailyRequests: 100, UserDailyRequests: 100,
			},
		},
		Usage: BeginUsageRequestParams{
			RequestID: requestID, UserID: user.ID, DeviceID: device.ID,
			APIKeyID: key.ID, Model: "billing-priced-model", Endpoint: "responses",
			RequestedAt: at,
		},
		Billing: &BillingReservationParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID,
			Model: "billing-priced-model", InputUSDPerMillion: "1",
			CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: at,
		},
	}
}

func billingIntegrationAssertReservationStates(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	requestID string,
	wantQuota string,
	wantBilling string,
) {
	t.Helper()
	var quotaState, billingState string
	if err := repository.db.QueryRowContext(ctx, `SELECT q.state, b.state
		FROM quota_reservations q JOIN billing_reservations b USING (request_id)
		WHERE q.request_id = $1`, requestID).Scan(&quotaState, &billingState); err != nil {
		t.Fatalf("read reservation states for %s: %v", requestID, err)
	}
	if quotaState != wantQuota || billingState != wantBilling {
		t.Fatalf("reservation states for %s = quota %s, billing %s; want %s, %s",
			requestID, quotaState, billingState, wantQuota, wantBilling)
	}
}

func billingIntegrationReserveAndComplete(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	user User,
	device Device,
	key APIKey,
	requestID string,
	at time.Time,
	inputTokens int64,
	inputPrice string,
	actualModel string,
) BillingReservation {
	t.Helper()
	reservation, err := repository.ReserveBilling(ctx, BillingReservationParams{
		RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: "billing-priced-model",
		InputUSDPerMillion: inputPrice, CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: at,
	})
	if err != nil {
		t.Fatalf("ReserveBilling(%s): %v", requestID, err)
	}
	billingIntegrationComplete(t, ctx, repository, user, device, key, requestID, at, inputTokens, actualModel)
	return reservation
}

func billingIntegrationComplete(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	user User,
	device Device,
	key APIKey,
	requestID string,
	requestedAt time.Time,
	inputTokens int64,
	actualModel string,
) {
	t.Helper()
	if _, err := repository.BeginUsageRequest(ctx, BeginUsageRequestParams{
		RequestID: requestID, UserID: user.ID, DeviceID: device.ID, APIKeyID: key.ID,
		Model: "billing-priced-model", Endpoint: "responses", RequestedAt: requestedAt,
	}); err != nil {
		t.Fatalf("BeginUsageRequest(%s): %v", requestID, err)
	}
	if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: requestID, State: "completed", HTTPStatus: 200,
		CompletedAt: requestedAt.Add(time.Second), InputTokens: inputTokens,
		ActualModel: actualModel,
	}); err != nil {
		t.Fatalf("CompleteUsageRequest(%s): %v", requestID, err)
	}
}

func billingIntegrationAssertAllocations(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	requestID string,
	wantSources []string,
	wantAmounts []string,
) {
	t.Helper()
	rows, err := repository.db.QueryContext(ctx, `SELECT source_type, amount_usd::text
		FROM billing_charge_allocations WHERE request_id = $1 ORDER BY allocation_order`, requestID)
	if err != nil {
		t.Fatalf("list billing allocations: %v", err)
	}
	defer rows.Close()
	var sources, amounts []string
	for rows.Next() {
		var source, amount string
		if err := rows.Scan(&source, &amount); err != nil {
			t.Fatalf("scan billing allocation: %v", err)
		}
		sources = append(sources, source)
		amounts = append(amounts, amount)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate billing allocations: %v", err)
	}
	if fmt.Sprint(sources) != fmt.Sprint(wantSources) || fmt.Sprint(amounts) != fmt.Sprint(wantAmounts) {
		t.Fatalf("allocations = %v %v, want %v %v", sources, amounts, wantSources, wantAmounts)
	}
}
