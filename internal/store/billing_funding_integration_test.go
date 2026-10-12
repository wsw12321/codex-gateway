//go:build integration

package store

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestOrderedMultiSubscriptionFundingPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	if err := s.SyncModelAccessCatalog(ctx, []string{"billing-priced-model", "other-model"}); err != nil {
		t.Fatal(err)
	}
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "ordered-funding")
	write := func(reason string) BillingWriteParams { return billingIntegrationWrite(t, user.ID, reason, now) }
	create := func(name, tier, allowance string, priority, days int, models []string) BillingSubscriptionState {
		value, err := s.PutSubscription(ctx, PutSubscriptionParams{BillingWriteParams: write(name),
			UserID: user.ID, Name: name, Tier: tier, PeriodDays: days, AllowanceUSD: allowance, ModelIDs: models})
		if err != nil {
			t.Fatal(err)
		}
		if value.Priority != 1 || !value.ChargeEnabled {
			t.Fatalf("incorrect subscription defaults: %+v", value)
		}
		value, err = s.SetBillingSubscriptionPreferences(ctx, SetBillingSubscriptionPreferencesParams{
			BillingWriteParams: write("set priority"), UserID: user.ID, SubscriptionID: value.ID, Priority: priority, ChargeEnabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	models := []string{"billing-priced-model"}
	first := create("first", "custom", "1", 10, 365, models)
	day := create("day", "day", "1", 20, 1, models)
	weekSmall := create("small", "week", "1.000001", 20, 7, models)
	weekLarge := create("large", "week", "1.000002", 20, 7, models)
	lastA := create("last a", "day", "1", 30, 1, models)
	lastB := create("last b", "day", "1", 30, 1, models)
	create("wrong model", "month", "100", 1, 31, []string{"other-model"})
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write("cash"), UserID: user.ID, USDAmount: "2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBillingCashPreferences(ctx, SetBillingCashPreferencesParams{BillingWriteParams: write("cash priority"), UserID: user.ID, Priority: 20, ChargeEnabled: true}); err != nil {
		t.Fatal(err)
	}
	r := billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, "multi-ordered-request", now, 8_000_000, "1", "billing-priced-model")
	lastIDs := []string{lastA.ID, lastB.ID}
	sort.Strings(lastIDs)
	wantIDs := []string{first.ID, weekLarge.ID, weekSmall.ID, day.ID, "", lastIDs[0], lastIDs[1]}
	var ids []string
	for _, source := range r.OrderedFundingSources {
		ids = append(ids, source.SubscriptionID)
	}
	if r.FundingRuleVersion != 3 || !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("ordered sources = %v, want %v; version %d", ids, wantIDs, r.FundingRuleVersion)
	}
	// Disable spending, replace the first subscription's model and period, add
	// new cash, and cross a period boundary after acceptance. None can change
	// the in-flight funding references or permit the newly credited cash.
	if _, err := s.SetBillingCashPreferences(ctx, SetBillingCashPreferencesParams{BillingWriteParams: write("disable cash"), UserID: user.ID, Priority: 1, ChargeEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSubscription(ctx, PutSubscriptionParams{BillingWriteParams: write("reopen"),
		UserID: user.ID, SubscriptionID: first.ID, Name: "replacement", Tier: "day", ModelIDs: []string{"other-model"}, AllowanceUSD: "50"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write("later cash"), UserID: user.ID, USDAmount: "100"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * 24 * time.Hour)
	if _, err := s.GetBillingState(ctx, user.ID, 10, 0); err != nil {
		t.Fatal(err)
	}
	settled, err := s.SettleBilling(ctx, r.RequestID, now)
	if err != nil {
		t.Fatal(err)
	}
	if *settled.ChargedUSD != "8.000000000000" || *settled.UncoveredUSD != "0.000000000000" {
		t.Fatalf("settlement = %+v", settled)
	}
	var balance string
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&balance); err != nil || balance != "100.000000000000" {
		t.Fatalf("admission cash cutoff lost: %s %v", balance, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source_type,subscription_period_id::text,amount_usd::text FROM billing_charge_allocations WHERE request_id=$1 ORDER BY allocation_order`, r.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var typ, amount string
		var period *string
		if err := rows.Scan(&typ, &period, &amount); err != nil {
			t.Fatal(err)
		}
		if i >= len(r.OrderedFundingSources) || typ != r.OrderedFundingSources[i].SourceType || (period != nil && *period != r.OrderedFundingSources[i].PeriodID) {
			t.Fatalf("allocation %d deviated from immutable order", i)
		}
		i++
	}
	if rows.Err() != nil || i != 7 {
		t.Fatalf("allocations = %d, %v", i, rows.Err())
	}
	replayed, err := s.SettleBilling(ctx, r.RequestID, now.Add(time.Minute))
	if err != nil || !reflect.DeepEqual(settled, replayed) {
		t.Fatalf("settlement replay changed: %v", err)
	}
	// Model mismatch has no retry hint even though unrelated subscriptions roll.
	_, err = s.ReserveBilling(ctx, BillingReservationParams{RequestID: "no-model-funding", UserID: user.ID, APIKeyID: key.ID,
		Model: "unfunded-model", InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: now})
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) || insufficient.RetryAfter != 0 {
		t.Fatalf("unmatched model retry = %v", err)
	}
}
