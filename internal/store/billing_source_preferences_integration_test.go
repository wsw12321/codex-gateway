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
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestBillingSourcePreferencesPostgresIntegration(t *testing.T) {
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
	base := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	actor := globalUsageIntegrationUser(t, ctx, repository, "source-actor-"+suffix, UserRoleMember)
	billingIntegrationSetRate(t, ctx, repository, actor.ID, "1", base)
	sources := []string{BillingTierDay, BillingTierWeek, BillingTierMonth, "cash"}

	setSource := func(t *testing.T, userID, source string, disabled bool, at time.Time) {
		t.Helper()
		if err := repository.SetBillingSourceDisabled(ctx, SetBillingSourceDisabledParams{
			UserID: userID, Source: source, Disabled: disabled, At: at,
		}); err != nil {
			t.Fatalf("SetBillingSourceDisabled(%s, %t): %v", source, disabled, err)
		}
	}
	fund := func(t *testing.T, userID string, tiers []string, allowance, cash string, periodCount int) {
		t.Helper()
		for _, tier := range tiers {
			if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
				BillingWriteParams: billingIntegrationWrite(t, actor.ID, "source preference test", base),
				UserID:             userID, Tier: tier, AllowanceUSD: allowance, PeriodCount: periodCount,
			}); err != nil {
				t.Fatalf("PutSubscription(%s): %v", tier, err)
			}
		}
		if cash != "" {
			billingIntegrationRecharge(t, ctx, repository, actor.ID, userID, cash, base)
		}
	}
	reserve := func(user User, key APIKey, scenario string, at time.Time) (BillingReservation, error) {
		return repository.ReserveBilling(ctx, billingSourcePreferenceReservation(user, key,
			billingIntegrationRequestID(suffix, scenario, 1), at))
	}

	t.Run("migration defaults for existing and new accounts", func(t *testing.T) {
		billingSourcePreferenceMigration(t, ctx, repository, suffix)
		user := globalUsageIntegrationUser(t, ctx, repository, "source-default-"+suffix, UserRoleMember)
		billingSourcePreferenceAssertState(t, ctx, repository, user.ID, BillingSourceDisabled{})
	})

	t.Run("every source combination preserves order and admission snapshot", func(t *testing.T) {
		for mask := 0; mask < 16; mask++ {
			t.Run(fmt.Sprintf("disabled_%04b", mask), func(t *testing.T) {
				user, device, key := billingIntegrationPrincipal(t, ctx, repository,
					fmt.Sprintf("source-mask-%d-%s", mask, suffix))
				// A user can set preferences before a subscription or cash credit exists.
				for index, source := range sources {
					if mask&(1<<index) != 0 {
						setSource(t, user.ID, source, true, base)
					}
				}
				fund(t, user.ID, sources[:3], "1", "1", 2)
				wantDisabled := BillingSourceDisabled{
					Day: mask&1 != 0, Week: mask&2 != 0, Month: mask&4 != 0, Cash: mask&8 != 0,
				}
				billingSourcePreferenceAssertState(t, ctx, repository, user.ID, wantDisabled)
				requestID := billingIntegrationRequestID(suffix, "source-mask", mask)
				admission, err := repository.AdmitRequest(ctx,
					billingIntegrationAdmission(user, device, key, requestID, base.Add(time.Second)))
				if mask == 15 {
					billingSourcePreferenceAssertInsufficient(t, err, 0)
					var requestRows int
					if err := repository.db.QueryRowContext(ctx, `SELECT
						(SELECT count(*) FROM billing_reservations WHERE request_id = $1) +
						(SELECT count(*) FROM quota_reservations WHERE request_id = $1) +
						(SELECT count(*) FROM usage_requests WHERE request_id = $1)`, requestID).Scan(&requestRows); err != nil {
						t.Fatal(err)
					}
					if requestRows != 0 {
						t.Fatalf("rejected admission left %d request rows", requestRows)
					}
				} else {
					if err != nil || admission.Billing == nil {
						t.Fatalf("AdmitRequest: billing=%+v err=%v", admission.Billing, err)
					}
					billingSourcePreferenceAssertBindings(t, *admission.Billing, mask)
				}

				// Restoring a source must not add it to an already accepted request.
				for _, source := range sources {
					setSource(t, user.ID, source, false, base.Add(2*time.Second))
				}
				billingSourcePreferenceAssertState(t, ctx, repository, user.ID, BillingSourceDisabled{})
				restored, err := reserve(user, key, fmt.Sprintf("source-restored-%d", mask), base.Add(3*time.Second))
				if err != nil {
					t.Fatalf("ReserveBilling(restored): %v", err)
				}
				billingSourcePreferenceAssertBindings(t, restored, 0)
				if mask == 15 {
					return
				}
				if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
					RequestID: requestID, State: "completed", HTTPStatus: 200,
					CompletedAt: base.Add(4 * time.Second), InputTokens: 4_000_000,
				}); err != nil {
					t.Fatalf("CompleteUsageRequest: %v", err)
				}
				settled, err := repository.SettleBilling(ctx, requestID, base.Add(5*time.Second))
				if err != nil {
					t.Fatalf("SettleBilling: %v", err)
				}
				var wantSources, wantAmounts []string
				for index, source := range sources {
					if mask&(1<<index) == 0 {
						wantSources = append(wantSources, source)
						wantAmounts = append(wantAmounts, "1.000000000000")
					}
				}
				billingIntegrationAssertAllocations(t, ctx, repository, requestID, wantSources, wantAmounts)
				if settled.UncoveredUSD == nil || *settled.UncoveredUSD != fmt.Sprintf("%d.000000000000", 4-len(wantSources)) {
					t.Fatalf("settlement used a source restored after admission: %+v", settled)
				}
				if err := repository.SettleQuota(ctx, requestID, 4_000_000, base.Add(5*time.Second)); err != nil {
					t.Fatalf("SettleQuota: %v", err)
				}
			})
		}
	})

	t.Run("disabling every source leaves accepted requests chargeable", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "source-bound-"+suffix)
		fund(t, user.ID, sources[:3], "1", "1", 2)
		reservation, err := reserve(user, key, "source-bound", base.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range sources {
			setSource(t, user.ID, source, true, base.Add(2*time.Second))
		}
		billingIntegrationComplete(t, ctx, repository, user, device, key,
			reservation.RequestID, base.Add(time.Second), 4_000_000, "")
		if _, err := repository.SettleBilling(ctx, reservation.RequestID, base.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		billingIntegrationAssertAllocations(t, ctx, repository, reservation.RequestID,
			sources, []string{"1.000000000000", "1.000000000000", "1.000000000000", "1.000000000000"})
	})

	t.Run("retry time ignores disabled subscriptions", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "source-retry-"+suffix)
		fund(t, user.ID, sources[:3], "1", "", 2)
		used := billingIntegrationReserveAndComplete(t, ctx, repository, user, device, key,
			billingIntegrationRequestID(suffix, "source-deplete", 1), base.Add(time.Second), 3_000_000, "1", "")
		if _, err := repository.SettleBilling(ctx, used.RequestID, base.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Hour)
		_, err := reserve(user, key, "source-retry-day", at)
		billingSourcePreferenceAssertInsufficient(t, err, 23*time.Hour)
		for index, source := range sources[:3] {
			setSource(t, user.ID, source, true, at)
			_, err := reserve(user, key, "source-retry-"+source+"-disabled", at)
			wantRetry := []time.Duration{7*24*time.Hour - time.Hour, 31*24*time.Hour - time.Hour, 0}[index]
			billingSourcePreferenceAssertInsufficient(t, err, wantRetry)
		}
		setSource(t, user.ID, BillingTierDay, false, at)
		_, err = reserve(user, key, "source-retry-restored", at)
		billingSourcePreferenceAssertInsufficient(t, err, 23*time.Hour)
	})

	t.Run("disabled subscriptions renew and expire without restoring spent credits", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "source-period-"+suffix)
		fund(t, user.ID, []string{BillingTierDay}, "2", "", 2)
		used := billingIntegrationReserveAndComplete(t, ctx, repository, user, device, key,
			billingIntegrationRequestID(suffix, "source-period-used", 1), base.Add(time.Second), 1_000_000, "1", "")
		if _, err := repository.SettleBilling(ctx, used.RequestID, base.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		setSource(t, user.ID, BillingTierDay, true, base.Add(3*time.Second))
		setSource(t, user.ID, BillingTierDay, false, base.Add(4*time.Second))
		state := billingSourcePreferenceAssertState(t, ctx, repository, user.ID, BillingSourceDisabled{})
		day := state.Subscriptions[0]
		if day.PeriodID == nil || used.DayPeriodID == nil || *day.PeriodID != *used.DayPeriodID ||
			day.RemainingUSD != "1.000000000000" || day.PeriodStartsAt == nil || !day.PeriodStartsAt.Equal(base) {
			t.Fatalf("restoring reopened the subscription or credited spent funds: %+v", day)
		}
		setSource(t, user.ID, BillingTierDay, true, base.Add(5*time.Second))
		_, err := reserve(user, key, "source-period-disabled-renewal", base.Add(24*time.Hour))
		billingSourcePreferenceAssertInsufficient(t, err, 0)
		// Rejected admission rolls back its renewal writes. A billing read at
		// the same simulated time must still advance the disabled subscription.
		renewalReader := &Store{db: repository.db, now: func() time.Time { return base.Add(24 * time.Hour) }}
		state = billingSourcePreferenceAssertState(t, ctx, renewalReader, user.ID, BillingSourceDisabled{Day: true})
		day = state.Subscriptions[0]
		if !day.Enabled || day.CurrentPeriodNumber != 2 || day.RemainingUSD != "2.000000000000" ||
			day.PeriodID == nil || *day.PeriodID == *used.DayPeriodID || day.PeriodStartsAt == nil ||
			!day.PeriodStartsAt.Equal(base.Add(24*time.Hour)) {
			t.Fatalf("disabled subscription did not renew normally: %+v", day)
		}
		setSource(t, user.ID, BillingTierDay, false, base.Add(25*time.Hour))
		renewed, err := reserve(user, key, "source-period-restored", base.Add(25*time.Hour))
		if err != nil || renewed.DayPeriodID == nil || *renewed.DayPeriodID != *day.PeriodID {
			t.Fatalf("restored subscription lost its renewed period: %+v, %v", renewed, err)
		}
		setSource(t, user.ID, BillingTierDay, true, base.Add(26*time.Hour))
		_, err = reserve(user, key, "source-period-expired", base.Add(48*time.Hour))
		billingSourcePreferenceAssertInsufficient(t, err, 0)
		setSource(t, user.ID, BillingTierDay, false, base.Add(49*time.Hour))
		state = billingSourcePreferenceAssertState(t, ctx, repository, user.ID, BillingSourceDisabled{})
		day = state.Subscriptions[0]
		if day.Enabled || day.ExpiresAt == nil || !day.ExpiresAt.Equal(base.Add(48*time.Hour)) {
			t.Fatalf("restoring revived an expired subscription: %+v", day)
		}
		_, err = reserve(user, key, "source-period-expired-restored", base.Add(49*time.Hour))
		billingSourcePreferenceAssertInsufficient(t, err, 0)
	})

	t.Run("recharge and administrator reopen preserve preferences", func(t *testing.T) {
		user, _, _ := billingIntegrationPrincipal(t, ctx, repository, "source-reopen-"+suffix)
		fund(t, user.ID, sources[:3], "1", "1", 2)
		for _, source := range sources {
			setSource(t, user.ID, source, true, base.Add(time.Second))
		}
		fund(t, user.ID, sources[:3], "3", "2", 3)
		state := billingSourcePreferenceAssertState(t, ctx, repository, user.ID,
			BillingSourceDisabled{Day: true, Week: true, Month: true, Cash: true})
		if state.BalanceUSD != "3.000000000000" {
			t.Fatalf("recharged balance = %s", state.BalanceUSD)
		}
		for _, subscription := range state.Subscriptions {
			if !subscription.Enabled || subscription.RemainingUSD != "3.000000000000" || subscription.PeriodCount != 3 {
				t.Fatalf("administrator did not reopen subscription: %+v", subscription)
			}
		}
		users, err := repository.ListBillingUsers(ctx)
		if err != nil {
			t.Fatalf("ListBillingUsers: %v", err)
		}
		for _, summary := range users {
			if summary.UserID == user.ID {
				if summary.SourceDisabled != state.SourceDisabled {
					t.Fatalf("summary preferences = %+v, want %+v", summary.SourceDisabled, state.SourceDisabled)
				}
				return
			}
		}
		t.Fatal("billing summary omitted user")
	})

	t.Run("preference audit is atomic and creates no financial entry", func(t *testing.T) {
		user, _, _ := billingIntegrationPrincipal(t, ctx, repository, "source-audit-"+suffix)
		digest := sha256.Sum256([]byte("source-session-" + suffix))
		session, err := repository.CreateSession(ctx, CreateSessionParams{
			UserID: user.ID, TokenHash: digest[:], CSRFSecret: digest[:], CreatedAt: base,
			IdleExpiresAt: base.Add(time.Hour), AbsoluteExpiresAt: base.Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		requestID := billingIntegrationRequestID(suffix, "source-audit", 1)
		params := SetBillingSourceDisabledParams{
			UserID: user.ID, Source: BillingTierDay, Disabled: true,
			ActorSessionID: session.ID, RequestID: requestID, SourceIP: "127.0.0.1", At: base,
		}
		if err := repository.SetBillingSourceDisabled(ctx, params); err != nil {
			t.Fatal(err)
		}
		var auditActor, auditSession, sourceIP, subjectType, subjectID, source string
		var disabled, previousDisabled, success bool
		var occurredAt time.Time
		if err := repository.db.QueryRowContext(ctx, `SELECT actor_user_id, actor_session_id,
			host(source_ip), subject_type, subject_id, metadata->>'source',
			(metadata->>'disabled')::boolean, (metadata->>'previous_disabled')::boolean,
			success, occurred_at FROM audit_events
			WHERE event_type = 'billing.source_status_changed' AND request_id = $1`, requestID,
		).Scan(&auditActor, &auditSession, &sourceIP, &subjectType, &subjectID, &source,
			&disabled, &previousDisabled, &success, &occurredAt); err != nil {
			t.Fatalf("read source preference audit: %v", err)
		}
		if auditActor != user.ID || auditSession != session.ID || sourceIP != "127.0.0.1" ||
			subjectType != "billing_account" || subjectID != user.ID || source != BillingTierDay ||
			!disabled || previousDisabled || !success || !occurredAt.Equal(base) {
			t.Fatalf("incorrect source preference audit: actor=%s session=%s source_ip=%s subject=%s/%s source=%s disabled=%t previous=%t success=%t at=%v",
				auditActor, auditSession, sourceIP, subjectType, subjectID, source, disabled, previousDisabled, success, occurredAt)
		}
		// Explicit assignment can be replayed without an operation ID or reason.
		if err := repository.SetBillingSourceDisabled(ctx, params); err != nil {
			t.Fatalf("repeat source preference: %v", err)
		}
		billingSourcePreferenceAssertState(t, ctx, repository, user.ID, BillingSourceDisabled{Day: true})
		missingSession, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		params.ActorSessionID = missingSession
		params.Source = "cash"
		if err := repository.SetBillingSourceDisabled(ctx, params); err == nil {
			t.Fatal("invalid audit session unexpectedly succeeded")
		}
		billingSourcePreferenceAssertState(t, ctx, repository, user.ID, BillingSourceDisabled{Day: true})
		var financialRows int
		if err := repository.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM billing_ledger_entries WHERE user_id = $1) +
			(SELECT count(*) FROM billing_operations WHERE target_user_id = $1)`, user.ID).Scan(&financialRows); err != nil {
			t.Fatal(err)
		}
		if financialRows != 0 {
			t.Fatalf("preference changes created %d financial rows", financialRows)
		}
	})

	t.Run("internal zero requests remain admissible with all sources disabled", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, repository, "source-zero-"+suffix)
		fund(t, user.ID, sources[:3], "1", "1", 2)
		for _, source := range sources {
			setSource(t, user.ID, source, true, base)
		}
		model := "codex-auto-review"
		requestID := billingIntegrationRequestID(suffix, "source-internal-zero", 1)
		params := billingIntegrationAdmission(user, device, key, requestID, base.Add(time.Second))
		params.Usage.Model = model
		params.Usage.PricingRuleVersion = config.PricingSchemaV2
		params.Billing = &BillingReservationParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: model,
			PricingRuleVersion: config.PricingSchemaV2, BillingMode: BillingModeInternalZero,
			PricingCatalogAsOf: "2026-08-20", PricingModel: model,
			CacheWriteMode: config.CacheWriteIncludedInInput, Now: base.Add(time.Second),
			PricingSnapshot: billingIntegrationV2Snapshot(t, model, config.ModelPricing{
				CacheWriteMode: config.CacheWriteIncludedInInput, MaxInputTokens: 272_000,
				LongContextThresholdTokens: 272_000,
				ServiceTiers: map[string]config.ServiceTierPricing{
					config.PricingTierStandard: {Short: &config.TokenPricing{
						InputUSDPerMillion: "0", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
					}},
				},
			}),
		}
		admission, err := repository.AdmitRequest(ctx, params)
		if err != nil || admission.Billing == nil {
			t.Fatalf("internal zero admission: %+v, %v", admission, err)
		}
		billingSourcePreferenceAssertBindings(t, *admission.Billing, 15)
		if err := repository.ReleaseQuota(ctx, requestID, base.Add(2*time.Second)); err != nil {
			t.Fatalf("ReleaseQuota: %v", err)
		}
	})

	t.Run("account lock orders preference changes and request admission", func(t *testing.T) {
		for _, preferenceFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("preference_first_%t", preferenceFirst), func(t *testing.T) {
				user, device, key := billingIntegrationPrincipal(t, ctx, repository,
					fmt.Sprintf("source-lock-%t-%s", preferenceFirst, suffix))
				fund(t, user.ID, nil, "", "1", 0)
				lockCtx, lockCancel := context.WithTimeout(ctx, 10*time.Second)
				defer lockCancel()
				tx, err := repository.db.BeginTx(lockCtx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var blockerPID int
				if err := tx.QueryRowContext(lockCtx, `SELECT pg_backend_pid()
					FROM billing_accounts WHERE user_id = $1 FOR UPDATE`, user.ID).Scan(&blockerPID); err != nil {
					t.Fatal(err)
				}
				preferenceDone := make(chan error, 1)
				type reservationResult struct {
					value RequestAdmission
					err   error
				}
				admissionDone := make(chan reservationResult, 1)
				startPreference := func() {
					go func() {
						preferenceDone <- repository.SetBillingSourceDisabled(lockCtx, SetBillingSourceDisabledParams{
							UserID: user.ID, Source: "cash", Disabled: true, At: base.Add(time.Second),
						})
					}()
				}
				startAdmission := func() {
					go func() {
						value, err := repository.AdmitRequest(lockCtx, billingIntegrationAdmission(user, device, key,
							billingIntegrationRequestID(suffix, fmt.Sprintf("source-lock-%t", preferenceFirst), 1), base.Add(time.Second)))
						admissionDone <- reservationResult{value: value, err: err}
					}()
				}
				if preferenceFirst {
					startPreference()
				} else {
					startAdmission()
				}
				billingSourcePreferenceWaitForLock(t, lockCtx, repository, blockerPID, 1)
				if preferenceFirst {
					startAdmission()
				} else {
					startPreference()
				}
				billingSourcePreferenceWaitForLock(t, lockCtx, repository, blockerPID, 2)
				if err := tx.Commit(); err != nil {
					t.Fatalf("release account lock: %v", err)
				}
				if err := <-preferenceDone; err != nil {
					t.Fatalf("queued preference update: %v", err)
				}
				admission := <-admissionDone
				if preferenceFirst {
					billingSourcePreferenceAssertInsufficient(t, admission.err, 0)
				} else if admission.err != nil || admission.value.Billing == nil || admission.value.Billing.CashLotCutoff == nil {
					t.Fatalf("earlier admission lost its cash source: %+v, %v", admission.value, admission.err)
				} else if err := repository.ReleaseQuota(ctx, admission.value.Billing.RequestID, base.Add(2*time.Second)); err != nil {
					t.Fatalf("ReleaseQuota: %v", err)
				}
				billingSourcePreferenceAssertState(t, ctx, repository, user.ID, BillingSourceDisabled{Cash: true})
			})
		}
	})
}

func billingSourcePreferenceReservation(user User, key APIKey, requestID string, at time.Time) BillingReservationParams {
	return BillingReservationParams{
		RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: "billing-priced-model",
		InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: at,
	}
}

func billingSourcePreferenceAssertState(t *testing.T, ctx context.Context, repository *Store,
	userID string, want BillingSourceDisabled,
) BillingState {
	t.Helper()
	state, err := repository.GetBillingState(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("GetBillingState: %v", err)
	}
	if state.SourceDisabled != want {
		t.Fatalf("source preferences = %+v, want %+v", state.SourceDisabled, want)
	}
	return state
}

func billingSourcePreferenceAssertBindings(t *testing.T, value BillingReservation, disabledMask int) {
	t.Helper()
	bound := []bool{value.DayPeriodID != nil, value.WeekPeriodID != nil, value.MonthPeriodID != nil, value.CashLotCutoff != nil}
	for index, present := range bound {
		if present != (disabledMask&(1<<index) == 0) {
			t.Fatalf("reservation bindings = %v, disabled mask = %04b", bound, disabledMask)
		}
	}
}

func billingSourcePreferenceAssertInsufficient(t *testing.T, err error, retry time.Duration) {
	t.Helper()
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("admission error = %v, want InsufficientFundsError", err)
	}
	if insufficient.RetryAfter != retry {
		t.Fatalf("retry = %v, want %v", insufficient.RetryAfter, retry)
	}
}

func billingSourcePreferenceWaitForLock(t *testing.T, ctx context.Context, repository *Store, blockerPID, want int) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		// Follow the wait chain because the second row-lock waiter can be
		// blocked by the first waiter rather than directly by our transaction.
		err := repository.db.QueryRowContext(ctx, `WITH RECURSIVE waiting AS (
			SELECT pid FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))
			UNION
			SELECT a.pid FROM pg_stat_activity a JOIN waiting w
			ON w.pid = ANY(pg_blocking_pids(a.pid))
		) SELECT count(*) FROM waiting`, blockerPID).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe billing account lock: %v", err)
		}
		if waiting >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d account lock waiters: %v", want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func billingSourcePreferenceMigration(t *testing.T, ctx context.Context, repository *Store, suffix string) {
	t.Helper()
	connection, err := repository.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schema := "billing_sources_" + suffix
	if _, err := connection.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = connection.ExecContext(cleanupCtx, `RESET search_path`)
		_ = connection.Close()
		_, _ = repository.db.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`)
	}()
	if _, err := connection.ExecContext(ctx, `SET search_path TO `+schema+`, public`); err != nil {
		t.Fatal(err)
	}
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var preferenceMigration string
	for _, migration := range migrations {
		if migration.Name == "0010_billing_source_preferences.sql" {
			preferenceMigration = migration.SQL
			break
		}
		if _, err := connection.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply isolated %s: %v", migration.Name, err)
		}
	}
	if preferenceMigration == "" {
		t.Fatal("billing source preference migration is missing")
	}
	insertUser := func(label string) string {
		t.Helper()
		id, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		handle := sha256.Sum256([]byte(label + suffix))
		if _, err := connection.ExecContext(ctx, `INSERT INTO users
			(id, username, display_name, webauthn_user_id, role)
			VALUES ($1, $2, $2, $3, 'member')`, id, label+suffix, handle[:]); err != nil {
			t.Fatalf("create migration user: %v", err)
		}
		return id
	}
	legacyID := insertUser("before-sources-")
	if _, err := connection.ExecContext(ctx, preferenceMigration); err != nil {
		t.Fatalf("apply source preference migration: %v", err)
	}
	newID := insertUser("after-sources-")
	for _, userID := range []string{legacyID, newID} {
		var day, week, month, cash sql.NullBool
		if err := connection.QueryRowContext(ctx, `SELECT day_source_disabled,
			week_source_disabled, month_source_disabled, cash_source_disabled
			FROM billing_accounts WHERE user_id = $1`, userID).Scan(&day, &week, &month, &cash); err != nil {
			t.Fatalf("read migrated preference defaults: %v", err)
		}
		for _, value := range []sql.NullBool{day, week, month, cash} {
			if !value.Valid || value.Bool {
				t.Fatalf("account %s source preference default = %+v, want false", userID, value)
			}
		}
	}
}
