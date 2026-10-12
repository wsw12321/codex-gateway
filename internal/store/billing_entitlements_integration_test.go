//go:build integration

package store

import (
	"errors"
	"testing"
	"time"
)

func TestBillingEntitlementsAppendLatestConfigurationPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "100")
	if err := f.s.SyncModelAccessCatalog(f.ctx, []string{"billing-priced-model", "gpt-future", "claude-future"}); err != nil {
		t.Fatal(err)
	}
	plan, err := f.s.CreateBillingPlan(f.ctx, CreateBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "initial plan", f.now), Name: "Daily", Tier: BillingTierDay, ModelIDs: []string{"billing-priced-model"}, PriceUSD: "2", AllowanceUSD: "10", MinPeriodCount: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, plan, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, plan, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Subscription.ID == first.Subscription.ID {
		t.Fatal("repeat purchase replaced subscription")
	}
	if _, err := f.s.DeleteSubscription(f.ctx, DeleteSubscriptionParams{BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "disable duplicate", f.now), UserID: f.user.ID, SubscriptionID: duplicate.Subscription.ID}); err != nil {
		t.Fatal(err)
	}
	boundID := "entitlement-before-plan-edits"
	if _, err := f.s.ReserveBilling(f.ctx, billingSourcePreferenceReservation(f.user, f.key, boundID, f.now)); err != nil {
		t.Fatal(err)
	}
	prefs, err := f.s.SetBillingSubscriptionPreferences(f.ctx, SetBillingSubscriptionPreferencesParams{BillingWriteParams: billingIntegrationWrite(t, f.user.ID, "custom source order", f.now), UserID: f.user.ID, SubscriptionID: first.Subscription.ID, Priority: 37, ChargeEnabled: false})
	if err != nil {
		t.Fatal(err)
	}
	edit := f.edit(t, plan)
	edit.Tier = BillingTierCustom
	edit.PeriodDays = 3
	edit.ModelIDs = []string{"gpt-future"}
	edit.AllowanceUSD = "20"
	edit.PriceUSD = "3"
	custom, err := f.s.UpdateBillingPlan(f.ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.s.RenewBillingSubscription(f.ctx, f.renewal(t, custom, prefs, 2))
	if err != nil {
		t.Fatal(err)
	}
	if second.TotalUSD != "6.000000000000" || second.Subscription.Priority != 37 || second.Subscription.ChargeEnabled || second.Subscription.PeriodDays != 1 || second.Subscription.AllowanceUSD != "10.000000000000" || len(second.Subscription.Entitlements) != 2 || !second.Subscription.ExpiresAt.Equal(f.now.Add(8*24*time.Hour)) {
		t.Fatalf("append changed current rights/preferences: %+v", second)
	}
	edit = f.edit(t, custom)
	edit.Tier = BillingTierWeek
	edit.PeriodDays = 7
	edit.ModelIDs = []string{"claude-future"}
	edit.AllowanceUSD = "30"
	edit.Active = false
	weekly, err := f.s.UpdateBillingPlan(f.ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	third, err := f.s.RenewBillingSubscription(f.ctx, f.renewal(t, weekly, second.Subscription, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Subscription.Entitlements) != 3 || !third.Subscription.ExpiresAt.Equal(f.now.Add(15*24*time.Hour)) {
		t.Fatalf("appended segment order: %+v", third.Subscription)
	}
	if _, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, weekly, 0, 1)); !errors.Is(err, ErrBillingPlanChanged) {
		t.Fatalf("unlisted purchase allowed: %v", err)
	}
	originalNow := f.now
	f.s.now = func() time.Time { return originalNow.Add(2 * 24 * time.Hour) }
	atCustom, err := readBillingPlanTestState(f, first.Subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if atCustom.Tier != BillingTierCustom || atCustom.PeriodDays != 3 || atCustom.CurrentPeriodNumber != 3 || atCustom.RemainingUSD != "20.000000000000" || len(atCustom.ModelIDs) != 1 || atCustom.ModelIDs[0] != "gpt-future" || !atCustom.PeriodEndsAt.Equal(originalNow.Add(5*24*time.Hour)) {
		t.Fatalf("custom boundary activation: %+v", atCustom)
	}
	// Skip all remaining custom cycles and most of the following weekly segment.
	f.s.now = func() time.Time { return originalNow.Add(14 * 24 * time.Hour) }
	atWeekly, err := readBillingPlanTestState(f, first.Subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if atWeekly.Tier != BillingTierWeek || atWeekly.CurrentPeriodNumber != 5 || atWeekly.RemainingUSD != "30.000000000000" || atWeekly.ModelIDs[0] != "claude-future" || !atWeekly.PeriodStartsAt.Equal(originalNow.Add(8*24*time.Hour)) {
		t.Fatalf("long idle segment activation: %+v", atWeekly)
	}
	billingIntegrationComplete(t, f.ctx, f.s, f.user, f.device, f.key, boundID, originalNow, 2_000_000, "billing-priced-model")
	if _, err := f.s.SettleBilling(f.ctx, boundID, originalNow.Add(14*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var oldRemaining string
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT remaining_usd::text FROM billing_subscription_periods WHERE id=$1`, first.Subscription.PeriodID).Scan(&oldRemaining); err != nil {
		t.Fatal(err)
	}
	current, err := readBillingPlanTestState(f, first.Subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldRemaining != "8.000000000000" || current.RemainingUSD != "30.000000000000" {
		t.Fatal("late settlement used a new entitlement period")
	}
	deleted, err := f.s.DeleteBillingPlan(f.ctx, DeleteBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "remove catalog plan", originalNow.Add(14*24*time.Hour)), PlanID: weekly.ID, Version: weekly.Version})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.DeletedAt == nil {
		t.Fatal("plan was not logically deleted")
	}
	current, err = readBillingPlanTestState(f, first.Subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	var historyCount int
	var firstName string
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT count(*),min(plan_snapshot->>'name') FILTER(WHERE first_period_number=1) FROM billing_subscription_entitlements WHERE subscription_id=$1`, first.Subscription.ID).Scan(&historyCount, &firstName); err != nil {
		t.Fatal(err)
	}
	if historyCount != 3 || firstName != "Daily" {
		t.Fatal("historical entitlement snapshots changed")
	}
	if current.Plan != nil || current.CanRenew || current.RemainingUSD != "30.000000000000" || len(current.Entitlements) != 1 || current.Entitlements[0].Plan.Tier != BillingTierWeek {
		t.Fatal("plan deletion changed purchased rights/history")
	}
}
