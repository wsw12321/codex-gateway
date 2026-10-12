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

	var upgrade Migration
	for _, migration := range migrations {
		if migration.Name == "0034_multi_subscriptions.sql" {
			upgrade = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	user := globalUsageIntegrationUser(t, ctx, s, "multi-legacy", UserRoleMember)
	start := now.Add(-time.Hour)
	subscriptionID, _ := newUUID()
	periodID, _ := newUUID()
	var planID string
	if err := s.db.QueryRowContext(ctx, `INSERT INTO billing_plans(name,price_usd,tier,allowance_usd,active,updated_by_user_id) VALUES('Legacy plan',2,'day',10,true,$1) RETURNING id`, user.ID).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscriptions(id,user_id,tier,allowance_usd,period_count,current_period_number,expires_at,created_at,updated_at,plan_id,plan_version) VALUES($1,$2,'day',10,5,1,$3,$4,$4,$5,1)`, subscriptionID, user.ID, start.Add(5*24*time.Hour), start, planID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscription_periods(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,period_number,period_count) VALUES($1,$2,$3,'day',$4,$5,10,6,1,5)`, periodID, subscriptionID, user.ID, start, start.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET current_period_id=$2 WHERE id=$1`, subscriptionID, periodID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts SET day_source_disabled=true WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	var ledgerID int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries(user_id,entry_type,reason,transaction_snapshot) VALUES($1,'plan_purchase','legacy immutable snapshot','{"plan":{"id":"legacy","name":"Original sale"}}') RETURNING id`, user.ID).Scan(&ledgerID); err != nil {
		t.Fatal(err)
	}
	readHistory := func() string {
		var value string
		if err := s.db.QueryRowContext(ctx, `SELECT to_jsonb(l)::text FROM billing_ledger_entries l WHERE id=$1`, ledgerID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	originalHistory := readHistory()
	if _, err := s.db.ExecContext(ctx, upgrade.SQL); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingSubscriptionModels(ctx); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty catalog initialization=%v", err)
	}
	if err := s.SyncModelAccessCatalog(ctx, []string{"gpt-old", "gemini-old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingSubscriptionModels(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := s.GetBillingState(ctx, user.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Subscriptions) != 1 {
		t.Fatalf("subscriptions=%+v", state.Subscriptions)
	}
	sub := state.Subscriptions[0]
	if sub.ID != subscriptionID || sub.PeriodID == nil || *sub.PeriodID != periodID || sub.Plan != nil || sub.CanRenew || sub.ChargeEnabled || sub.Priority != 1 || state.CashPreferences.Priority != 100 || len(sub.ModelIDs) != 2 || len(sub.Entitlements) != 1 || !equalAllocationDecimal(sub.RemainingUSD, "6") || !sub.PeriodStartsAt.Equal(start) || !sub.ExpiresAt.Equal(start.Add(5*24*time.Hour)) {
		t.Fatalf("migration changed rights: %+v", sub)
	}
	plans, err := s.ListBillingPlans(ctx, true)
	if err != nil || len(plans) != 0 {
		t.Fatalf("legacy catalog retained: %v %v", plans, err)
	}
	if err := s.SyncModelAccessCatalog(ctx, []string{"gpt-old", "gemini-old", "claude-new"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingSubscriptionModels(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := s.GetBillingState(ctx, user.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Subscriptions[0].ModelIDs) != 2 {
		t.Fatal("new catalog model expanded migrated scope")
	}
	if got := readHistory(); got != originalHistory {
		t.Fatalf("migration rewrote history: %s", got)
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
