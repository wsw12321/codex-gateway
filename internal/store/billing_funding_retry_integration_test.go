//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestBillingFundingRetryUsesFutureModelEntitlementsPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "100")
	if err := f.s.SyncModelAccessCatalog(f.ctx, []string{"billing-priced-model", "future-model", "unfunded-model"}); err != nil {
		t.Fatal(err)
	}
	plan, err := f.s.CreateBillingPlan(f.ctx, CreateBillingPlanParams{
		BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "initial model scope", f.now),
		Name:               "Initial daily", Tier: BillingTierDay, ModelIDs: []string{"billing-priced-model"}, PriceUSD: "1", AllowanceUSD: "10", MinPeriodCount: 1, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bought, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, plan, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	edit := f.edit(t, plan)
	edit.Tier, edit.PeriodDays, edit.ModelIDs = BillingTierCustom, 3, []string{"future-model"}
	updated, err := f.s.UpdateBillingPlan(f.ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	appended, err := f.s.RenewBillingSubscription(f.ctx, f.renewal(t, updated, bought.Subscription, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetBillingCashPreferences(f.ctx, SetBillingCashPreferencesParams{
		BillingWriteParams: billingIntegrationWrite(t, f.user.ID, "fund only from subscriptions", f.now), UserID: f.user.ID, Priority: 100, ChargeEnabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	checkRetry := func(requestID, model string, at time.Time, want time.Duration) {
		t.Helper()
		params := billingSourcePreferenceReservation(f.user, f.key, requestID, at)
		params.Model = model
		_, err := f.s.ReserveBilling(f.ctx, params)
		var insufficient *InsufficientFundsError
		if !errors.As(err, &insufficient) || insufficient.RetryAfter != want {
			t.Fatalf("%s: retry=%+v error=%v, want %s", requestID, insufficient, err, want)
		}
		var count int
		if err := f.s.db.QueryRowContext(f.ctx, `SELECT count(*) FROM billing_reservations WHERE request_id=$1`, requestID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected admission persisted: count=%d error=%v", count, err)
		}
	}
	at := f.now.Add(time.Hour)
	checkRetry("retry-future-scope", "future-model", at, 47*time.Hour)
	checkRetry("retry-unfunded-scope", "unfunded-model", at, 0)
	if _, err := f.s.SetBillingSubscriptionPreferences(f.ctx, SetBillingSubscriptionPreferencesParams{
		BillingWriteParams: billingIntegrationWrite(t, f.user.ID, "disable future funding", at), UserID: f.user.ID, SubscriptionID: appended.Subscription.ID, Priority: 1, ChargeEnabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	checkRetry("retry-disabled-future-scope", "future-model", at, 0)
	if _, err := f.s.SetBillingSubscriptionPreferences(f.ctx, SetBillingSubscriptionPreferencesParams{
		BillingWriteParams: billingIntegrationWrite(t, f.user.ID, "restore future funding", at), UserID: f.user.ID, SubscriptionID: appended.Subscription.ID, Priority: 1, ChargeEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Crossing into the new segment changes eligibility exactly at its start.
	segmentStart := f.now.Add(2 * 24 * time.Hour)
	checkRetry("retry-future-before-boundary", "future-model", segmentStart.Add(-time.Microsecond), time.Microsecond)
	consumeCycle := func(requestID string, requestedAt time.Time) {
		t.Helper()
		params := billingSourcePreferenceReservation(f.user, f.key, requestID, requestedAt)
		params.Model = "future-model"
		reserved, err := f.s.ReserveBilling(f.ctx, params)
		if err != nil || len(reserved.OrderedFundingSources) != 1 || reserved.OrderedFundingSources[0].SubscriptionID != appended.Subscription.ID {
			t.Fatalf("new segment admission: %+v %v", reserved, err)
		}
		billingIntegrationComplete(t, f.ctx, f.s, f.user, f.device, f.key, requestID, requestedAt, 10_000_000, "future-model")
		settled, err := f.s.SettleBilling(f.ctx, requestID, requestedAt.Add(time.Second))
		if err != nil || *settled.ChargedUSD != "10.000000000000" {
			t.Fatalf("consume current period: %+v %v", settled, err)
		}
	}
	consumeCycle("retry-consume-first-future-cycle", segmentStart)
	checkRetry("retry-current-cycle-exhausted", "future-model", segmentStart.Add(time.Hour), 71*time.Hour)
	checkRetry("retry-old-model-no-longer-eligible", "billing-priced-model", segmentStart.Add(time.Hour), 0)
	consumeCycle("retry-consume-final-future-cycle", segmentStart.Add(3*24*time.Hour))
	checkRetry("retry-final-cycle-has-no-renewal", "future-model", segmentStart.Add(3*24*time.Hour+time.Hour), 0)
}

func TestMultiSubscriptionMigrationPreservesV2FundingPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	defer admin.Close()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"funding_v2_upgrade_" + strings.ReplaceAll(id, "-", "")}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg.RuntimeParams["search_path"] = strings.Trim(schema, `"`)
	s := New(stdlib.OpenDB(*cfg))
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
			t.Fatalf("apply legacy %s: %v", migration.Name, err)
		}
	}
	if upgrade.Name == "" {
		t.Fatal("multi-subscription migration missing")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "v2-inflight")
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "legacy group", now), Name: "Legacy group", LimitUSD: "2", Period: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "legacy membership", now), GroupID: group.ID, Action: "add", UserIDs: []string{user.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "legacy cash", now), UserID: user.ID, USDAmount: "10"}); err != nil {
		t.Fatal(err)
	}
	type legacySubscription struct {
		id, periodID, tier string
		days               int
		allowance          string
	}
	subscriptions := []legacySubscription{{tier: "day", days: 1, allowance: "3"}, {tier: "week", days: 7, allowance: "5"}}
	for i := range subscriptions {
		sub := &subscriptions[i]
		sub.id, err = newUUID()
		if err != nil {
			t.Fatal(err)
		}
		sub.periodID, err = newUUID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscriptions(id,user_id,tier,enabled,allowance_usd,period_count,current_period_number,created_at,updated_at) VALUES($1,$2,$3,true,$4::numeric,0,1,$5,$5)`, sub.id, user.ID, sub.tier, sub.allowance, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscription_periods(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,period_number,period_count,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::numeric,$7::numeric,1,0,$5)`, sub.periodID, sub.id, user.ID, sub.tier, now, now.Add(time.Duration(sub.days)*24*time.Hour), sub.allowance); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET current_period_id=$2 WHERE id=$1`, sub.id, sub.periodID); err != nil {
			t.Fatal(err)
		}
	}
	requestID := "v2-inflight-before-multi-subscription"
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{RequestID: requestID, UserID: user.ID, DeviceID: device.ID, APIKeyID: key.ID, Model: "billing-priced-model", Endpoint: "responses", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_reservations(request_id,user_id,api_key_id,requested_model,input_usd_per_million,cached_input_usd_per_million,output_usd_per_million,day_period_id,week_period_id,cash_lot_cutoff,group_id,group_period_id,funding_rule_version,created_at) VALUES($1,$2,$3,'billing-priced-model',1,0,0,$4,$5,1,$6,$7,2,$8)`, requestID, user.ID, key.ID, subscriptions[0].periodID, subscriptions[1].periodID, group.ID, group.PeriodID, now); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, upgrade.SQL); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncModelAccessCatalog(ctx, []string{"billing-priced-model"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingSubscriptionModels(ctx); err != nil {
		t.Fatal(err)
	}
	// New priorities and toggles affect only new requests. This v2 request must
	// still consume group, day, week, then its original cash cutoff.
	for i, sub := range subscriptions {
		if _, err := s.SetBillingSubscriptionPreferences(ctx, SetBillingSubscriptionPreferencesParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "new source preference", now), UserID: user.ID, SubscriptionID: sub.id, Priority: 100 - i, ChargeEnabled: false}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetBillingCashPreferences(ctx, SetBillingCashPreferencesParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "new cash preference", now), UserID: user.ID, Priority: 1, ChargeEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: requestID, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second), InputTokens: 12_000_000, ActualModel: "billing-priced-model"}); err != nil {
		t.Fatal(err)
	}
	settled, err := s.SettleBilling(ctx, requestID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if settled.FundingRuleVersion != 2 || len(settled.OrderedFundingSources) != 0 {
		t.Fatalf("legacy reservation rewritten: %+v", settled)
	}
	groupBillingAssertSplit(t, settled, "2", "10", "0")
	billingIntegrationAssertAllocations(t, ctx, s, requestID, []string{"group", "day", "week", "cash"}, []string{"2.000000000000", "3.000000000000", "5.000000000000", "2.000000000000"})
	var balance, memberUsed string
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&balance); err != nil || balance != "8.000000000000" {
		t.Fatalf("legacy cash charged=%s: %v", balance, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT used_usd::text FROM group_member_usage WHERE period_id=$1 AND user_id=$2`, group.PeriodID, user.ID).Scan(&memberUsed); err != nil || memberUsed != "2.000000000000" {
		t.Fatalf("legacy group member funding=%s: %v", memberUsed, err)
	}
	replayed, err := s.SettleBilling(ctx, requestID, now.Add(3*time.Second))
	if err != nil || !reflect.DeepEqual(settled, replayed) {
		t.Fatalf("legacy settlement replay changed: %v", err)
	}
}
