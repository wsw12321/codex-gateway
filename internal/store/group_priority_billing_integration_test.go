//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestGroupPriorityBillingPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 20, MaxIdleConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	suffix := fmt.Sprint(now.UnixNano())
	actor := globalUsageIntegrationUser(t, ctx, s, "priority-actor-"+suffix, UserRoleMember)
	write := func() BillingWriteParams {
		return billingIntegrationWrite(t, actor.ID, "group funding integration", now)
	}
	create := func(name, limit string, memberLimit *string) Group {
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: name, LimitUSD: limit,
			MemberLimitUSD: memberLimit, MemberLimitSet: true, Period: "week"})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	add := func(g Group, users ...User) {
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, Action: "add", UserIDs: ids}); err != nil {
			t.Fatal(err)
		}
	}
	credit := func(user User, amount string) {
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: user.ID, USDAmount: amount}); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(user User, device Device, key APIKey, scenario string, sequence int, amount int64) BillingReservation {
		id := billingIntegrationRequestID(suffix, scenario, sequence)
		return billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, id, now, amount*1000000, "1", "billing-priced-model")
	}
	settle := func(r BillingReservation, group, personal, uncovered string) BillingReservation {
		got, err := s.SettleBilling(ctx, r.RequestID, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		groupBillingAssertSplit(t, got, group, personal, uncovered)
		return got
	}
	get := func(g Group) Group {
		got, err := s.GetGroup(ctx, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ptr := func(value string) *string { return &value }

	t.Run("group only admission and cancellation with disabled personal sources", func(t *testing.T) {
		g := create("Group only", "100", ptr("50"))
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "priority-only-"+suffix)
		add(g, u)
		credit(u, "7")
		for _, source := range []string{"day", "week", "month", "cash"} {
			if err := s.SetBillingSourceDisabled(ctx, SetBillingSourceDisabledParams{UserID: u.ID, Source: source, Disabled: true, At: now}); err != nil {
				t.Fatal(err)
			}
		}
		r := complete(u, d, k, "only", 1, 20)
		if r.FundingRuleVersion != 2 || r.GroupPeriodID == nil || r.CashLotCutoff != nil || r.DayPeriodID != nil || r.WeekPeriodID != nil || r.MonthPeriodID != nil {
			t.Fatalf("group-only bindings = %+v", r)
		}
		settled := settle(r, "20", "0", "0")
		replayed, err := s.SettleBilling(ctx, r.RequestID, now.Add(2*time.Second))
		if err != nil || replayed.SettledAt == nil || !replayed.SettledAt.Equal(*settled.SettledAt) {
			t.Fatalf("settlement replay = %+v %v", replayed, err)
		}
		billingIntegrationAssertAllocations(t, ctx, s, r.RequestID, []string{"group"}, []string{"20.000000000000"})
		cancelledID := billingIntegrationRequestID(suffix, "cancel", 1)
		admitted, err := s.AdmitRequest(ctx, billingIntegrationAdmission(u, d, k, cancelledID, now))
		if err != nil || admitted.Billing == nil || admitted.Billing.GroupPeriodID == nil {
			t.Fatalf("group-only request admission = %+v %v", admitted, err)
		}
		if err := s.ReleaseRequest(ctx, cancelledID, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		state, err := s.GetBillingState(ctx, u.ID, 20, 0)
		if err != nil || state.BalanceUSD != "7.000000000000" || state.GroupMemberUsedUSD == nil ||
			*state.GroupMemberUsedUSD != "20.000000000000" || state.GroupMemberRemainingUSD == nil || *state.GroupMemberRemainingUSD != "30.000000000000" {
			t.Fatalf("group-only state = %+v %v", state, err)
		}
		if len(state.Ledger) == 0 || state.Ledger[0].GroupChargedUSD == nil || *state.Ledger[0].GroupChargedUSD != "20.000000000000" ||
			state.Ledger[0].PersonalChargedUSD == nil || *state.Ledger[0].PersonalChargedUSD != "0.000000000000" {
			t.Fatalf("group ledger split = %+v", state.Ledger)
		}
		if got := get(g); got.UsedUSD != "20.000000000000" || got.Members[0].UsedUSD != "20.000000000000" {
			t.Fatalf("cancellation/replay changed usage = %+v", got)
		}
	})

	t.Run("member boundary shares remainder with personal sources in order", func(t *testing.T) {
		g := create("Member boundary", "1000", ptr("100"))
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "priority-member-"+suffix)
		add(g, u)
		settle(complete(u, d, k, "member", 1, 95), "95", "0", "0")
		for i, tier := range []string{"day", "week", "month"} {
			if _, err := s.PutSubscription(ctx, PutSubscriptionParams{BillingWriteParams: write(), UserID: u.ID,
				Tier: tier, AllowanceUSD: fmt.Sprint(i + 2), PeriodCount: 1}); err != nil {
				t.Fatal(err)
			}
		}
		credit(u, "10")
		r := complete(u, d, k, "member", 2, 20)
		settle(r, "5", "15", "0")
		billingIntegrationAssertAllocations(t, ctx, s, r.RequestID, []string{"group", "day", "week", "month", "cash"},
			[]string{"5.000000000000", "2.000000000000", "3.000000000000", "4.000000000000", "6.000000000000"})
		if got := get(g); got.UsedUSD != "100.000000000000" || got.Members[0].UsedUSD != "100.000000000000" {
			t.Fatalf("member boundary totals = %+v", got)
		}
		r = complete(u, d, k, "member", 3, 1)
		if r.GroupPeriodID != nil {
			t.Fatal("exhausted member acquired group funding")
		}
		settle(r, "0", "1", "0")
	})

	t.Run("group boundary and insufficient accepted funding record uncovered cost", func(t *testing.T) {
		g := create("Group boundary", "5", nil)
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "priority-boundary-"+suffix)
		add(g, u)
		credit(u, "3")
		r := complete(u, d, k, "boundary", 1, 10)
		settle(r, "5", "3", "2")
		billingIntegrationAssertAllocations(t, ctx, s, r.RequestID, []string{"group", "cash"}, []string{"5.000000000000", "3.000000000000"})
		if got := get(g); got.UsedUSD != "5.000000000000" || got.RemainingUSD != "0.000000000000" {
			t.Fatalf("group boundary exceeded = %+v", got)
		}
		id := billingIntegrationRequestID(suffix, "all-empty", 1)
		_, err := s.AdmitRequest(ctx, billingIntegrationAdmission(u, d, k, id, now))
		var insufficient *InsufficientFundsError
		if !errors.As(err, &insufficient) {
			t.Fatalf("all-empty admission = %v", err)
		}
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_reservations WHERE request_id=$1)
			+(SELECT count(*) FROM quota_reservations WHERE request_id=$1)+(SELECT count(*) FROM usage_requests WHERE request_id=$1)`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected admission records = %d %v", count, err)
		}
	})

	t.Run("unavailable group sources fall back to personal", func(t *testing.T) {
		for _, scenario := range []string{"group-zero", "member-zero", "group-exhausted", "future"} {
			t.Run(scenario, func(t *testing.T) {
				limit, member := "5", (*string)(nil)
				if scenario == "group-zero" {
					limit = "0"
				}
				if scenario == "member-zero" {
					member = ptr("0")
				}
				g := create("Fallback "+scenario, limit, member)
				u, d, k := billingIntegrationPrincipal(t, ctx, s, "priority-fallback-"+scenario+suffix)
				add(g, u)
				credit(u, "10")
				if scenario == "group-exhausted" {
					if _, err := s.db.ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=limit_usd WHERE id=$1`, g.PeriodID); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "future" {
					start := now.Add(time.Hour)
					if _, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name,
						LimitUSD: "5", Period: g.Period, StartsAt: &start}); err != nil {
						t.Fatal(err)
					}
				}
				r := complete(u, d, k, scenario, 1, 2)
				if r.GroupID != nil || r.GroupPeriodID != nil {
					t.Fatal("unavailable group bound at admission")
				}
				// Funding that becomes available later must not be retroactively
				// attached to an accepted personal-only request.
				if _, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name,
					LimitUSD: "100", Period: g.Period, StartsAt: &now, MemberLimitSet: true}); err != nil {
					t.Fatal(err)
				}
				settle(r, "0", "2", "0")
			})
		}
	})

	t.Run("limits lowered after acceptance preserve bindings and avoid overshoot", func(t *testing.T) {
		g := create("Bound limits", "20", ptr("10"))
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "priority-lowered-"+suffix)
		add(g, u)
		credit(u, "20")
		settle(complete(u, d, k, "lowered", 1, 5), "5", "0", "0")
		r := complete(u, d, k, "lowered", 2, 8)
		g, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name,
			LimitUSD: "4", MemberLimitUSD: ptr("3"), MemberLimitSet: true, Period: g.Period})
		if err != nil || g.PeriodID != *r.GroupPeriodID {
			t.Fatalf("limits-only edit = %+v %v", g, err)
		}
		settle(r, "0", "8", "0")
		if got := get(g); got.UsedUSD != "5.000000000000" || got.Members[0].UsedUSD != "5.000000000000" {
			t.Fatalf("lowering reset usage = %+v", got)
		}
	})

	t.Run("parallel settlements enforce same-member and shared group caps", func(t *testing.T) {
		for _, multiple := range []bool{false, true} {
			t.Run(fmt.Sprintf("multiple-members-%t", multiple), func(t *testing.T) {
				g := create("Concurrent cap", "5", ptr("3"))
				const count = 12
				ids := make([]string, 0, count)
				var u User
				var d Device
				var k APIKey
				for i := 0; i < count; i++ {
					if multiple || i == 0 {
						u, d, k = billingIntegrationPrincipal(t, ctx, s, fmt.Sprintf("priority-concurrent-%t-%d-%s", multiple, i, suffix))
						add(g, u)
						credit(u, "20")
					}
					r := complete(u, d, k, fmt.Sprintf("concurrent-%t", multiple), i, 1)
					ids = append(ids, r.RequestID)
				}
				errs := make(chan error, count)
				var wg sync.WaitGroup
				for _, id := range ids {
					wg.Add(1)
					go func(id string) {
						defer wg.Done()
						_, err := s.SettleBilling(ctx, id, now)
						if err == nil {
							_, err = s.SettleBilling(ctx, id, now)
						}
						errs <- err
					}(id)
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
				want := "3.000000000000"
				if multiple {
					want = "5.000000000000"
				}
				got := get(g)
				if got.UsedUSD != want {
					t.Fatalf("concurrent group used = %s, want %s", got.UsedUSD, want)
				}
				var total, groupSum, personalSum string
				var allocations, ledger int
				if err := s.db.QueryRowContext(ctx, `SELECT
					(SELECT sum(used_usd)::text FROM group_member_usage WHERE period_id=$1),
					(SELECT sum(group_charged_usd)::text FROM billing_reservations WHERE group_period_id=$1),
					(SELECT sum(personal_charged_usd)::text FROM billing_reservations WHERE group_period_id=$1),
					(SELECT count(*) FROM billing_charge_allocations WHERE group_period_id=$1),
					(SELECT count(*) FROM billing_ledger_entries WHERE group_period_id=$1)`, g.PeriodID).Scan(&total, &groupSum, &personalSum, &allocations, &ledger); err != nil {
					t.Fatal(err)
				}
				if total != want || groupSum != want || ledger != count || allocations != 3 && !multiple || allocations != 5 && multiple {
					t.Fatalf("concurrent accounting member=%s group=%s personal=%s allocations=%d ledger=%d", total, groupSum, personalSum, allocations, ledger)
				}
				wantPersonal := "9.000000000000"
				if multiple {
					wantPersonal = "7.000000000000"
				}
				if personalSum != wantPersonal {
					t.Fatalf("concurrent personal paid = %s, want %s", personalSum, wantPersonal)
				}
			})
		}
	})
}

func groupBillingAssertSplit(t *testing.T, r BillingReservation, group, personal, uncovered string) {
	t.Helper()
	for name, value := range map[string]struct {
		got  *string
		want string
	}{
		"group": {r.GroupChargedUSD, group}, "personal": {r.PersonalChargedUSD, personal}, "uncovered": {r.UncoveredUSD, uncovered},
	} {
		if value.got == nil || !equalAllocationDecimal(*value.got, value.want) {
			t.Fatalf("%s payment = %v, want %s: %+v", name, value.got, value.want, r)
		}
	}
	covered, err := billingAdd(group, personal)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := billingAdd(covered, uncovered)
	if err != nil {
		t.Fatal(err)
	}
	if r.ChargedUSD == nil || *r.ChargedUSD != covered || r.ActualCostUSD == nil || *r.ActualCostUSD != actual {
		t.Fatalf("funding identity violated = %+v", r)
	}
}
