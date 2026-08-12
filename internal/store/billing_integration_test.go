//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
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
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "5",
		})
		if err != nil {
			t.Fatalf("PutSubscription(first): %v", err)
		}
		secondAt := firstAt.Add(time.Hour)
		secondParams := PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, owner.ID, "change day plan", secondAt),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "7",
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
			!second.PeriodEndsAt.Equal(secondAt.Add(24*time.Hour)) {
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
			putReplayAfterCharge.RemainingUSD != second.RemainingUSD {
			t.Fatalf("subscription replay changed after charge: first=%+v replay=%+v", second, putReplayAfterCharge)
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
	if byName["0001_initial.sql"] == "" || byName["0002_billing.sql"] == "" {
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
