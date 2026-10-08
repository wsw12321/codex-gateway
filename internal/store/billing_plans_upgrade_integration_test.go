//go:build integration

package store

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestBillingPlanMigrationPreservesLegacyPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	configuration, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*configuration)
	defer admin.Close()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "plan_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop plan migration schema: %v", err)
		}
	}()
	configuration.RuntimeParams["search_path"] = schema
	s := New(stdlib.OpenDB(*configuration))
	defer s.Close()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var planMigration Migration
	for _, migration := range migrations {
		if migration.Name == "0024_subscription_plans.sql" {
			planMigration = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply legacy %s: %v", migration.Name, err)
		}
	}
	if planMigration.Name == "" {
		t.Fatal("plan migration missing")
	}
	// Current billing readers require the independent funding metadata while
	// this test keeps the subscription-plan migration unapplied for its fixture.
	for _, migration := range migrations {
		if migration.Name == "0027_group_priority_billing.sql" || migration.Name == "0033_anthropic_billing.sql" {
			if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
				t.Fatalf("apply funding helper schema: %v", err)
			}
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	user := globalUsageIntegrationUser(t, ctx, s, "plan-legacy", UserRoleMember)
	start := now.Add(-time.Hour)
	var legacyDayID, legacyDayPeriod string
	for _, fixture := range []struct {
		tier    string
		count   int
		enabled bool
	}{
		{BillingTierDay, 5, true},
		{BillingTierWeek, 0, true},
		{BillingTierMonth, 2, false},
	} {
		subscriptionID, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		duration, err := billingPeriodDuration(fixture.tier)
		if err != nil {
			t.Fatal(err)
		}
		var expiry, disabledAt any
		if fixture.count > 0 {
			expiry = start.Add(time.Duration(fixture.count) * duration)
		}
		if !fixture.enabled {
			disabledAt = now
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscriptions
			(id,user_id,tier,enabled,allowance_usd,period_count,current_period_number,expires_at,created_at,updated_at,disabled_at)
			VALUES ($1,$2,$3,$4,10,$5,1,$6,$7,$7,$8)`, subscriptionID, user.ID, fixture.tier, fixture.enabled, fixture.count, expiry, start, disabledAt); err != nil {
			t.Fatal(err)
		}
		if !fixture.enabled {
			continue
		}
		periodID, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscription_periods
			(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,period_number,period_count,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,10,6,1,$7,$5)`, periodID, subscriptionID, user.ID, fixture.tier, start, start.Add(duration), fixture.count); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET current_period_id=$2 WHERE id=$1`, subscriptionID, periodID); err != nil {
			t.Fatal(err)
		}
		if fixture.tier == BillingTierDay {
			legacyDayID, legacyDayPeriod = subscriptionID, periodID
		}
	}
	params := PutSubscriptionParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "legacy day subscription", start),
		UserID: user.ID, Tier: BillingTierDay, AllowanceUSD: "10", PeriodCount: 5}
	fingerprint := billingOperationFingerprint("subscription_set", user.ID, user.ID, BillingTierDay, params.Reason, "10", "5")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_operations
		(operation_id,operation_type,actor_user_id,target_user_id,reason,request_fingerprint,created_at)
		VALUES ($1,'subscription_set',$2,$2,$3,$4,$5)`, params.OperationID, user.ID, params.Reason, fingerprint, start); err != nil {
		t.Fatal(err)
	}
	var ledgerID int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries
		(user_id,operation_id,entry_type,amount_usd,subscription_tier,subscription_period_id,reason,actor_user_id,created_at)
		VALUES ($1,$2,'subscription_set',10,'day',$3,$4,$1,$5) RETURNING id`, user.ID, params.OperationID, legacyDayPeriod, params.Reason, start).Scan(&ledgerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_operations SET result_ledger_entry_id=$2 WHERE operation_id=$1`, params.OperationID, ledgerID); err != nil {
		t.Fatal(err)
	}
	readLegacy := func() string {
		t.Helper()
		var rows string
		if err := s.db.QueryRowContext(ctx, `SELECT jsonb_build_object(
			'subscriptions',(SELECT jsonb_agg(to_jsonb(s)-'plan_id'-'plan_version'-'config_version' ORDER BY tier) FROM billing_subscriptions s),
			'periods',(SELECT jsonb_agg(to_jsonb(p) ORDER BY tier) FROM billing_subscription_periods p),
			'ledger',(SELECT jsonb_agg(to_jsonb(l)-'transaction_snapshot' ORDER BY id) FROM billing_ledger_entries l),
			'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM billing_operations o))::text`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := readLegacy()
	if _, err := s.db.ExecContext(ctx, planMigration.SQL); err != nil {
		t.Fatalf("apply plan migration: %v", err)
	}
	if after := readLegacy(); after != before {
		t.Fatalf("migration changed existing rights or history:\nbefore=%s\nafter=%s", before, after)
	}
	state, err := s.GetBillingState(ctx, user.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[int64]bool{}
	for _, sub := range state.Subscriptions {
		if sub.Plan != nil || sub.CanRenew || sub.ConfigVersion <= 0 || versions[sub.ConfigVersion] {
			t.Fatalf("invalid migrated binding or configuration version: %+v", sub)
		}
		versions[sub.ConfigVersion] = true
	}
	replay, err := s.PutSubscription(ctx, params)
	if err != nil {
		t.Fatalf("replay legacy subscription operation: %v", err)
	}
	if replay.ID != legacyDayID || replay.PeriodID == nil || *replay.PeriodID != legacyDayPeriod ||
		replay.PeriodCount != 5 || !equalAllocationDecimal(replay.RemainingUSD, "10") || replay.Plan != nil || replay.CanRenew {
		t.Fatalf("legacy operation replay changed: %+v", replay)
	}
	if after := readLegacy(); after != before {
		t.Fatal("read or replay changed legacy history")
	}
}

func TestBillingPlanRenewalNaturalRolloverPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "100")
	p := f.plan(t, BillingTierDay, 1)
	first, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, p, 0, 3))
	if err != nil {
		t.Fatal(err)
	}
	requestID := "plan-rollover-first-period-charge"
	billingIntegrationReserveAndComplete(t, f.ctx, f.s, f.user, f.device, f.key, requestID, f.now, 4_000_000, "1", "billing-priced-model")
	if _, err := f.s.SettleBilling(f.ctx, requestID, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// The renewal itself is the first action after a natural period boundary.
	renew := f.renewal(t, p, first.Subscription, 1)
	renew.At = f.now.Add(25 * time.Hour)
	renewed, err := f.s.RenewBillingSubscription(f.ctx, renew)
	if err != nil {
		t.Fatal(err)
	}
	sub := renewed.Subscription
	if sub.CurrentPeriodNumber != 2 || sub.PeriodCount != 4 || sub.PeriodID == nil || *sub.PeriodID == *first.Subscription.PeriodID ||
		!sub.PeriodStartsAt.Equal(f.now.Add(24*time.Hour)) || !sub.ExpiresAt.Equal(f.now.Add(4*24*time.Hour)) || !equalAllocationDecimal(sub.RemainingUSD, "10") {
		t.Fatalf("natural rollover renewal: %+v", renewed)
	}
	var oldRemaining string
	var oldCount, currentSnapshotCount int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT p.remaining_usd::text,p.period_count,n.period_count
		FROM billing_subscription_periods p JOIN billing_subscription_periods n ON n.id=$2 WHERE p.id=$1`, first.Subscription.PeriodID, sub.PeriodID).
		Scan(&oldRemaining, &oldCount, &currentSnapshotCount); err != nil {
		t.Fatal(err)
	}
	if !equalAllocationDecimal(oldRemaining, "6") || oldCount != 3 || currentSnapshotCount != 3 {
		t.Fatalf("renewal changed period snapshots: remaining=%s counts=%d/%d", oldRemaining, oldCount, currentSnapshotCount)
	}
	requestID = "plan-rollover-second-period-charge"
	billingIntegrationReserveAndComplete(t, f.ctx, f.s, f.user, f.device, f.key, requestID, renew.At, 2_000_000, "1", "billing-priced-model")
	if _, err := f.s.SettleBilling(f.ctx, requestID, renew.At.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	next := f.renewal(t, p, sub, 1)
	next.At = renew.At.Add(time.Minute)
	second, err := f.s.RenewBillingSubscription(f.ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if *second.Subscription.PeriodID != *sub.PeriodID || second.Subscription.CurrentPeriodNumber != 2 ||
		second.Subscription.PeriodCount != 5 || !equalAllocationDecimal(second.Subscription.RemainingUSD, "8") {
		t.Fatalf("renewal refilled the current natural period: %+v", second)
	}
}

func TestBillingPlanOverflowRollbackPostgresIntegration(t *testing.T) {
	for _, scenario := range []string{"purchase_expiry", "renewal_expiry", "renewal_count"} {
		t.Run(scenario, func(t *testing.T) {
			f := newBillingPlanFixture(t)
			f.fund(t, "100")
			p := f.plan(t, BillingTierDay, 1)
			params := f.purchase(t, p, 0, 1)
			if scenario != "renewal_count" {
				params.At = time.Date(9999, 12, 29, 0, 0, 0, 0, time.UTC)
			}
			var sub BillingSubscriptionState
			if scenario != "purchase_expiry" {
				first, err := f.s.PurchaseBillingPlan(f.ctx, params)
				if err != nil {
					t.Fatal(err)
				}
				sub = first.Subscription
				if scenario == "renewal_count" {
					if _, err := f.s.db.ExecContext(f.ctx, `UPDATE billing_subscriptions SET period_count=$2 WHERE id=$1`, sub.ID, math.MaxInt32); err != nil {
						t.Fatal(err)
					}
				}
			}
			readState := func() string {
				t.Helper()
				var state string
				if err := f.s.db.QueryRowContext(f.ctx, `SELECT jsonb_build_object(
					'account',(SELECT to_jsonb(a) FROM billing_accounts a WHERE user_id=$1),
					'lots',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM billing_cash_credit_lots c WHERE user_id=$1),
					'subscriptions',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM billing_subscriptions s WHERE user_id=$1),
					'periods',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM billing_subscription_periods p WHERE user_id=$1),
					'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM billing_operations o),
					'ledger',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM billing_ledger_entries l),
					'audit',(SELECT count(*) FROM audit_events))::text`, f.user.ID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := readState()
			var err error
			if scenario == "purchase_expiry" {
				params.PeriodCount = 99
				_, err = f.s.PurchaseBillingPlan(f.ctx, params)
			} else {
				renew := f.renewal(t, p, sub, 99)
				renew.At = params.At.Add(time.Hour)
				_, err = f.s.RenewBillingSubscription(f.ctx, renew)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("overflow should reject with ErrInvalid: %v", err)
			}
			if after := readState(); after != before {
				t.Fatalf("overflow left partial transaction artifacts:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}
