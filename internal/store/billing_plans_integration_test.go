//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type billingPlanFixture struct {
	s           *Store
	ctx         context.Context
	actor, user User
	device      Device
	key         APIKey
	now         time.Time
}

func newBillingPlanFixture(t *testing.T) *billingPlanFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	actor := globalUsageIntegrationUser(t, ctx, s, "plan-actor", UserRoleMember)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "plan-user")
	return &billingPlanFixture{s: s, ctx: ctx, actor: actor, user: user, device: device, key: key, now: now}
}

func (f *billingPlanFixture) fund(t *testing.T, amount string) {
	t.Helper()
	_, err := f.s.AdjustUserBalance(f.ctx, AdjustUserBalanceParams{
		BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "test plan funds", f.now),
		UserID:             f.user.ID, USDAmount: amount,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *billingPlanFixture) plan(t *testing.T, tier string, minimum int) BillingPlan {
	t.Helper()
	p, err := f.s.CreateBillingPlan(f.ctx, CreateBillingPlanParams{
		BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "test plan", f.now),
		Name:               "Test plan", Tier: tier, PriceUSD: "2", AllowanceUSD: "10", MinPeriodCount: minimum, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *billingPlanFixture) purchase(t *testing.T, p BillingPlan, version int64, count int) PurchaseBillingPlanParams {
	t.Helper()
	return PurchaseBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, f.user.ID, "purchase plan", f.now),
		UserID: f.user.ID, PlanID: p.ID, PlanVersion: p.Version, SubscriptionConfigVersion: version, PeriodCount: count}
}

func (f *billingPlanFixture) renewal(t *testing.T, p BillingPlan, sub BillingSubscriptionState, count int) RenewBillingSubscriptionParams {
	t.Helper()
	return RenewBillingSubscriptionParams{BillingWriteParams: billingIntegrationWrite(t, f.user.ID, "renew plan", f.now),
		UserID: f.user.ID, Tier: p.Tier, PlanVersion: p.Version, SubscriptionConfigVersion: sub.ConfigVersion, PeriodCount: count}
}

func (f *billingPlanFixture) edit(t *testing.T, p BillingPlan) UpdateBillingPlanParams {
	t.Helper()
	return UpdateBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "edit plan", f.now),
		PlanID: p.ID, Version: p.Version, Name: p.Name, Tier: p.Tier, PriceUSD: p.PriceUSD,
		AllowanceUSD: p.AllowanceUSD, MinPeriodCount: p.MinPeriodCount, Active: p.Active}
}

func (f *billingPlanFixture) state(t *testing.T, tier string) BillingSubscriptionState {
	t.Helper()
	s, err := f.s.GetBillingState(f.ctx, f.user.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range s.Subscriptions {
		if sub.Tier == tier {
			return sub
		}
	}
	t.Fatalf("missing subscription %s: %+v", tier, s)
	return BillingSubscriptionState{}
}

func assertBillingPlanReplay(t *testing.T, first, replay any) {
	t.Helper()
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(replay)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("operation replay changed\nfirst: %s\nreplay: %s", a, b)
	}
}

func TestBillingPlanPurchaseAndRenewalPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "500")
	manual, err := f.s.PutSubscription(f.ctx, PutSubscriptionParams{
		BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "manual subscription", f.now.Add(-time.Hour)),
		UserID:             f.user.ID, Tier: BillingTierDay, AllowanceUSD: "40", PeriodCount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manual.Plan != nil || manual.CanRenew {
		t.Fatalf("manual subscription bound: %+v", manual)
	}
	p := f.plan(t, BillingTierDay, 2)
	params := f.purchase(t, p, manual.ConfigVersion, 2)
	first, err := f.s.PurchaseBillingPlan(f.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	sub := first.Subscription
	if sub.Plan == nil || sub.Plan.ID != p.ID || !sub.CanRenew || sub.CurrentPeriodNumber != 1 || sub.PeriodCount != 2 ||
		!equalAllocationDecimal(sub.RemainingUSD, "10") || !sub.PeriodStartsAt.Equal(f.now) ||
		!sub.ExpiresAt.Equal(f.now.Add(48*time.Hour)) || sub.ConfigVersion <= manual.ConfigVersion ||
		!equalAllocationDecimal(first.TotalUSD, "4") || !equalAllocationDecimal(first.BalanceUSD, "496") {
		t.Fatalf("purchase did not reopen exact plan rights: %+v", first)
	}
	requestID := "plan-consume-before-renewal"
	billingIntegrationReserveAndComplete(t, f.ctx, f.s, f.user, f.device, f.key, requestID, f.now, 3_000_000, "1", "billing-priced-model")
	if _, err := f.s.SettleBilling(f.ctx, requestID, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	before := f.state(t, p.Tier)
	if !equalAllocationDecimal(before.RemainingUSD, "7") {
		t.Fatalf("consume: %+v", before)
	}
	renew := f.renewal(t, p, before, 99)
	renewed, err := f.s.RenewBillingSubscription(f.ctx, renew)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Subscription.PeriodCount != 101 || renewed.Subscription.CurrentPeriodNumber != 1 ||
		*renewed.Subscription.PeriodID != *before.PeriodID || !equalAllocationDecimal(renewed.Subscription.RemainingUSD, "7") ||
		!renewed.Subscription.ExpiresAt.Equal(before.ExpiresAt.Add(99*24*time.Hour)) ||
		!equalAllocationDecimal(renewed.TotalUSD, "198") {
		t.Fatalf("renewal lost current rights: %+v", renewed)
	}
	var historicCount int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT period_count FROM billing_subscription_periods WHERE id=$1`, before.PeriodID).Scan(&historicCount); err != nil {
		t.Fatal(err)
	}
	if historicCount != 2 {
		t.Fatalf("renewal rewrote original period snapshot: %d", historicCount)
	}
	replayed, err := f.s.PurchaseBillingPlan(f.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	assertBillingPlanReplay(t, first, replayed)
	replayRenew, err := f.s.RenewBillingSubscription(f.ctx, renew)
	if err != nil {
		t.Fatal(err)
	}
	assertBillingPlanReplay(t, renewed, replayRenew)
	// Disabling must snapshot the extended 101-period configuration, rather than
	// reconstruct the original two-period purchase from its current period row.
	disable := DeleteSubscriptionParams{BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "disable renewed subscription", f.now.Add(time.Minute)), UserID: f.user.ID, Tier: p.Tier}
	disabled, err := f.s.DeleteSubscription(f.ctx, disable)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.PeriodCount != 101 || !disabled.ExpiresAt.Equal(renewed.Subscription.ExpiresAt.UTC()) {
		t.Fatalf("disable lost renewed configuration: %+v", disabled)
	}
	if _, err := f.s.PutSubscription(f.ctx, PutSubscriptionParams{
		BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "manual restart after disable", f.now.Add(2*time.Minute)),
		UserID:             f.user.ID, Tier: p.Tier, AllowanceUSD: "20", PeriodCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if reopened := f.state(t, p.Tier); reopened.Plan != nil || reopened.CanRenew {
		t.Fatalf("manual restart kept binding: %+v", reopened)
	}
	disableReplay, err := f.s.DeleteSubscription(f.ctx, disable)
	if err != nil {
		t.Fatal(err)
	}
	assertBillingPlanReplay(t, disabled, disableReplay)
}

func TestBillingPlanReplacementPostgresIntegration(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_plan_%t", same), func(t *testing.T) {
			f := newBillingPlanFixture(t)
			f.fund(t, "100")
			p := f.plan(t, BillingTierWeek, 1)
			first, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, p, 0, 3))
			if err != nil {
				t.Fatal(err)
			}
			if !same {
				p = f.plan(t, BillingTierWeek, 1)
			}
			next := f.purchase(t, p, first.Subscription.ConfigVersion, 1)
			next.At = f.now.Add(time.Hour)
			second, err := f.s.PurchaseBillingPlan(f.ctx, next)
			if err != nil {
				t.Fatal(err)
			}
			if second.Subscription.PeriodCount != 1 || second.Subscription.CurrentPeriodNumber != 1 ||
				!second.Subscription.PeriodStartsAt.Equal(next.At) || !second.Subscription.ExpiresAt.Equal(next.At.Add(7*24*time.Hour)) ||
				*second.Subscription.PeriodID == *first.Subscription.PeriodID || second.Subscription.Plan.ID != p.ID ||
				!equalAllocationDecimal(second.BalanceUSD, "92") {
				t.Fatalf("replacement: %+v", second)
			}
		})
	}
}

func TestBillingPlanInvalidationPostgresIntegration(t *testing.T) {
	changes := []struct {
		name   string
		change func(*UpdateBillingPlanParams)
	}{
		{"name", func(p *UpdateBillingPlanParams) { p.Name = "Renamed" }},
		{"price", func(p *UpdateBillingPlanParams) { p.PriceUSD = "3" }},
		{"tier", func(p *UpdateBillingPlanParams) { p.Tier = BillingTierMonth }},
		{"allowance", func(p *UpdateBillingPlanParams) { p.AllowanceUSD = "20" }},
		{"minimum", func(p *UpdateBillingPlanParams) { p.MinPeriodCount = 2 }},
		{"unlist", func(p *UpdateBillingPlanParams) { p.Active = false }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			f := newBillingPlanFixture(t)
			f.fund(t, "100")
			p := f.plan(t, BillingTierDay, 1)
			params := f.purchase(t, p, 0, 2)
			first, err := f.s.PurchaseBillingPlan(f.ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			noop, err := f.s.UpdateBillingPlan(f.ctx, f.edit(t, p))
			if err != nil {
				t.Fatal(err)
			}
			if noop.Version != p.Version || !f.state(t, p.Tier).CanRenew {
				t.Fatal("no-op edit invalidated binding")
			}
			edit := f.edit(t, p)
			change.change(&edit)
			changed, err := f.s.UpdateBillingPlan(f.ctx, edit)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Version <= p.Version {
				t.Fatal("edit did not increment plan version")
			}
			after := f.state(t, p.Tier)
			if after.Plan != nil || after.CanRenew || after.PeriodCount != first.Subscription.PeriodCount ||
				!after.ExpiresAt.Equal(*first.Subscription.ExpiresAt) || after.RemainingUSD != first.Subscription.RemainingUSD {
				t.Fatalf("edit changed rights or left binding: %+v", after)
			}
			if _, err := f.s.RenewBillingSubscription(f.ctx, f.renewal(t, p, after, 2)); !errors.Is(err, ErrBillingPlanChanged) {
				t.Fatalf("renew invalid binding: %v", err)
			}
			restore := f.edit(t, p)
			restore.Version = changed.Version
			if _, err := f.s.UpdateBillingPlan(f.ctx, restore); err != nil {
				t.Fatal(err)
			}
			if f.state(t, p.Tier).CanRenew {
				t.Fatal("restoring old settings restored historical binding")
			}
			replay, err := f.s.PurchaseBillingPlan(f.ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			assertBillingPlanReplay(t, first, replay)
		})
	}
}

func TestBillingPlanFailureRollbackPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	p := f.plan(t, BillingTierDay, 2)
	params := f.purchase(t, p, 0, 2)
	if _, err := f.s.PurchaseBillingPlan(f.ctx, params); err == nil {
		t.Fatal("purchase with no funds succeeded")
	} else {
		var insufficient *InsufficientFundsError
		if !errors.As(err, &insufficient) {
			t.Fatalf("insufficient error: %v", err)
		}
	}
	var artifacts int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM billing_operations WHERE operation_id=$1) + (SELECT count(*) FROM billing_subscriptions WHERE user_id=$2) + (SELECT count(*) FROM billing_ledger_entries WHERE operation_id=$1)`, params.OperationID, f.user.ID).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 0 {
		t.Fatalf("failed purchase left %d artifacts", artifacts)
	}
	f.fund(t, "100")
	for _, count := range []int{0, 1, 100} {
		bad := f.purchase(t, p, 0, count)
		if _, err := f.s.PurchaseBillingPlan(f.ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("count %d: %v", count, err)
		}
	}
	first, err := f.s.PurchaseBillingPlan(f.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{0, 1, 100} {
		if _, err := f.s.RenewBillingSubscription(f.ctx, f.renewal(t, p, first.Subscription, count)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("renew count %d: %v", count, err)
		}
	}
	changed := params
	changed.PeriodCount = 3
	if _, err := f.s.PurchaseBillingPlan(f.ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotent payload: %v", err)
	}
	stale := f.purchase(t, p, 0, 2)
	if _, err := f.s.PurchaseBillingPlan(f.ctx, stale); !errors.Is(err, ErrBillingPlanChanged) {
		t.Fatalf("stale config version: %v", err)
	}
	expired := f.renewal(t, p, first.Subscription, 2)
	expired.At = *first.Subscription.ExpiresAt
	if _, err := f.s.RenewBillingSubscription(f.ctx, expired); !errors.Is(err, ErrBillingPlanChanged) {
		t.Fatalf("expired subscription: %v", err)
	}
}

func TestBillingPlanAdminRestartInvalidatesConfirmationPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "100")
	p := f.plan(t, BillingTierDay, 1)
	firstParams := f.purchase(t, p, 0, 2)
	first, err := f.s.PurchaseBillingPlan(f.ctx, firstParams)
	if err != nil {
		t.Fatal(err)
	}
	pendingPurchase := f.purchase(t, p, first.Subscription.ConfigVersion, 3)
	pendingRenewal := f.renewal(t, p, first.Subscription, 3)
	manual, err := f.s.PutSubscription(f.ctx, PutSubscriptionParams{
		BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "restart after buyer confirmation", f.now),
		UserID:             f.user.ID, Tier: p.Tier, AllowanceUSD: "50", PeriodCount: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manual.Plan != nil || manual.CanRenew || manual.ConfigVersion == first.Subscription.ConfigVersion {
		t.Fatalf("manual restart did not clear binding and change confirmation version: %+v", manual)
	}
	if _, err := f.s.PurchaseBillingPlan(f.ctx, pendingPurchase); !errors.Is(err, ErrBillingPlanChanged) {
		t.Fatalf("stale purchase after restart: %v", err)
	}
	if _, err := f.s.RenewBillingSubscription(f.ctx, pendingRenewal); !errors.Is(err, ErrBillingPlanChanged) {
		t.Fatalf("stale renewal after restart: %v", err)
	}
	if _, err := f.s.RenewBillingSubscription(f.ctx, f.renewal(t, p, manual, 1)); !errors.Is(err, ErrBillingPlanChanged) {
		t.Fatalf("manual subscription renewal: %v", err)
	}
	replay, err := f.s.PurchaseBillingPlan(f.ctx, firstParams)
	if err != nil {
		t.Fatal(err)
	}
	assertBillingPlanReplay(t, first, replay)
	confirmed, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, p, manual.ConfigVersion, 1))
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Subscription.Plan == nil || !confirmed.Subscription.CanRenew || !equalAllocationDecimal(confirmed.BalanceUSD, "94") {
		t.Fatalf("freshly confirmed purchase did not restore binding: %+v", confirmed)
	}
}

func TestBillingPlanConcurrentReplayPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "100")
	p := f.plan(t, BillingTierDay, 1)
	params := f.purchase(t, p, 0, 1)
	var wg sync.WaitGroup
	results := make(chan BillingPlanTransaction, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := f.s.PurchaseBillingPlan(f.ctx, params)
			results <- result
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *BillingPlanTransaction
	for result := range results {
		if first == nil {
			first = &result
		} else {
			assertBillingPlanReplay(t, *first, result)
		}
	}
	s, err := f.s.GetBillingState(f.ctx, f.user.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalAllocationDecimal(s.BalanceUSD, "98") {
		t.Fatalf("replay charged repeatedly: %s", s.BalanceUSD)
	}
}

func TestBillingPlanFIFOAndInflightPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "5")
	f.fund(t, "10")
	manual, err := f.s.PutSubscription(f.ctx, PutSubscriptionParams{BillingWriteParams: billingIntegrationWrite(t, f.actor.ID, "old rights", f.now.Add(-time.Hour)), UserID: f.user.ID, Tier: BillingTierDay, AllowanceUSD: "40", PeriodCount: 5})
	if err != nil {
		t.Fatal(err)
	}
	requestID := "plan-pending-before-purchase"
	reserved, err := f.s.ReserveBilling(f.ctx, billingSourcePreferenceReservation(f.user, f.key, requestID, f.now))
	if err != nil {
		t.Fatal(err)
	}
	if reserved.DayPeriodID == nil || *reserved.DayPeriodID != *manual.PeriodID {
		t.Fatal("missing old request binding")
	}
	if err := f.s.SetBillingSourceDisabled(f.ctx, SetBillingSourceDisabledParams{UserID: f.user.ID, Source: BillingTierDay, Disabled: true, At: f.now}); err != nil {
		t.Fatal(err)
	}
	p := f.plan(t, BillingTierDay, 1)
	purchase, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, p, manual.ConfigVersion, 3))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.db.QueryContext(f.ctx, `SELECT remaining_usd::text FROM billing_cash_credit_lots WHERE user_id=$1 ORDER BY lot_sequence`, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lots []string
	for rows.Next() {
		var amount string
		if err := rows.Scan(&amount); err != nil {
			t.Fatal(err)
		}
		lots = append(lots, amount)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(lots) != 2 || !equalAllocationDecimal(lots[0], "0") || !equalAllocationDecimal(lots[1], "9") {
		t.Fatalf("purchase did not drain FIFO cash: %v", lots)
	}
	billingIntegrationComplete(t, f.ctx, f.s, f.user, f.device, f.key, requestID, f.now, 2_000_000, "billing-priced-model")
	if _, err := f.s.SettleBilling(f.ctx, requestID, f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after := f.state(t, p.Tier)
	if *after.PeriodID != *purchase.Subscription.PeriodID || !equalAllocationDecimal(after.RemainingUSD, "10") {
		t.Fatalf("old request consumed new rights: %+v", after)
	}
	billingIntegrationAssertAllocations(t, f.ctx, f.s, requestID, []string{"day"}, []string{"2.000000000000"})
	state, err := f.s.GetBillingState(f.ctx, f.user.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !state.SourceDisabled.Day || !equalAllocationDecimal(state.BalanceUSD, "9") {
		t.Fatalf("source or cash changed: %+v", state)
	}
}

func TestBillingPlanAuditRollbackPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	f.fund(t, "100")
	p := f.plan(t, BillingTierDay, 1)
	if _, err := f.s.db.ExecContext(f.ctx, `CREATE FUNCTION reject_plan_test_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit failure'; END $$; CREATE TRIGGER reject_plan_test_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_plan_test_audit()`); err != nil {
		t.Fatal(err)
	}
	params := f.purchase(t, p, 0, 2)
	if _, err := f.s.PurchaseBillingPlan(f.ctx, params); err == nil {
		t.Fatal("purchase accepted failed audit")
	}
	var balance, lots string
	var artifacts int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT balance_usd::text, (SELECT sum(remaining_usd)::text FROM billing_cash_credit_lots WHERE user_id=$1), (SELECT count(*) FROM billing_subscriptions WHERE user_id=$1)+(SELECT count(*) FROM billing_operations WHERE operation_id=$2)+(SELECT count(*) FROM billing_ledger_entries WHERE operation_id=$2) FROM billing_accounts WHERE user_id=$1`, f.user.ID, params.OperationID).Scan(&balance, &lots, &artifacts); err != nil {
		t.Fatal(err)
	}
	if !equalAllocationDecimal(balance, "100") || !equalAllocationDecimal(lots, "100") || artifacts != 0 {
		t.Fatalf("audit rollback: balance=%s lots=%s artifacts=%d", balance, lots, artifacts)
	}
}

func TestBillingPlanEditOrderingPostgresIntegration(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		for _, editFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("renewal_%t_edit_first_%t", renewal, editFirst), func(t *testing.T) {
				f := newBillingPlanFixture(t)
				f.fund(t, "100")
				p := f.plan(t, BillingTierDay, 1)
				var sub BillingSubscriptionState
				if renewal {
					bought, err := f.s.PurchaseBillingPlan(f.ctx, f.purchase(t, p, 0, 1))
					if err != nil {
						t.Fatal(err)
					}
					sub = bought.Subscription
				}
				purchase := f.purchase(t, p, 0, 1)
				renew := f.renewal(t, p, sub, 1)
				edit := f.edit(t, p)
				edit.PriceUSD = "3"
				ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
				defer cancel()
				tx, err := f.s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var blocker int
				if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM billing_plans WHERE id=$1 FOR UPDATE`, p.ID).Scan(&blocker); err != nil {
					t.Fatal(err)
				}
				edits := make(chan error, 1)
				sales := make(chan error, 1)
				startEdit := func() { go func() { _, err := f.s.UpdateBillingPlan(ctx, edit); edits <- err }() }
				startSale := func() {
					go func() {
						var err error
						if renewal {
							_, err = f.s.RenewBillingSubscription(ctx, renew)
						} else {
							_, err = f.s.PurchaseBillingPlan(ctx, purchase)
						}
						sales <- err
					}()
				}
				if editFirst {
					startEdit()
				} else {
					startSale()
				}
				billingSourcePreferenceWaitForLock(t, ctx, f.s, blocker, 1)
				if editFirst {
					startSale()
				} else {
					startEdit()
				}
				billingSourcePreferenceWaitForLock(t, ctx, f.s, blocker, 2)
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := <-edits; err != nil {
					t.Fatalf("edit: %v", err)
				}
				saleErr := <-sales
				if editFirst {
					if !errors.Is(saleErr, ErrBillingPlanChanged) {
						t.Fatalf("sale after edit: %v", saleErr)
					}
				} else if saleErr != nil {
					t.Fatalf("sale before edit: %v", saleErr)
				}
				state, err := f.s.GetBillingState(f.ctx, f.user.ID, 50, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, current := range state.Subscriptions {
					if current.Plan != nil || current.CanRenew {
						t.Fatalf("edited plan binding remained: %+v", current)
					}
				}
				want := "98"
				if editFirst {
					want = "100"
				}
				if renewal {
					if editFirst {
						want = "98"
					} else {
						want = "96"
					}
				}
				if !equalAllocationDecimal(state.BalanceUSD, want) {
					t.Fatalf("sale ordering balance=%s want=%s", state.BalanceUSD, want)
				}
			})
		}
	}
}

func TestBillingPlanHistoryCleanupPostgresIntegration(t *testing.T) {
	f := newBillingPlanFixture(t)
	old := f.now.AddDate(0, 0, -10)
	f.now = old
	f.fund(t, "10")
	p := f.plan(t, BillingTierDay, 1)
	params := f.purchase(t, p, 0, 2)
	first, err := f.s.PurchaseBillingPlan(f.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	renew := f.renewal(t, p, first.Subscription, 1)
	if _, err := f.s.RenewBillingSubscription(f.ctx, renew); err != nil {
		t.Fatal(err)
	}
	now := old.AddDate(0, 0, 10)
	f.s.now = func() time.Time { return now }
	if _, err := f.s.ExpireBillingSubscriptions(f.ctx, now, 100); err != nil {
		t.Fatal(err)
	}
	cutoff, err := InformationCutoff(now, 2)
	if err != nil {
		t.Fatal(err)
	}
	job := informationIntegrationDrain(t, f.ctx, f.s, f.actor.ID, cutoff)
	if job.Report.DeleteCounts["billing_operations"] < 3 {
		t.Fatalf("plan history not cleaned: %+v", job.Report)
	}
	if _, err := f.s.PurchaseBillingPlan(f.ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("cleaned purchase replay: %v", err)
	}
	if _, err := f.s.RenewBillingSubscription(f.ctx, renew); !errors.Is(err, ErrConflict) {
		t.Fatalf("cleaned renewal replay: %v", err)
	}
	plans, err := f.s.ListBillingPlans(f.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].ID != p.ID {
		t.Fatalf("cleanup deleted catalog: %+v", plans)
	}
}
