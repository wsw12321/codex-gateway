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

func TestGroupsPostgresIntegration(t *testing.T) {
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
	actor := globalUsageIntegrationUser(t, ctx, s, "group-actor-"+suffix, UserRoleMember)
	write := func() BillingWriteParams { return billingIntegrationWrite(t, actor.ID, "group integration test", now) }
	create := func(name, limit string) Group {
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: name, LimitUSD: limit, Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	members := func(g Group, action string, ids ...string) Group {
		result, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, UserIDs: ids, Action: action})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	credit := func(user User, amount string) {
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: user.ID, USDAmount: amount}); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(user User, key APIKey, id string) BillingReservation {
		r, err := s.ReserveBilling(ctx, BillingReservationParams{RequestID: id, UserID: user.ID, APIKeyID: key.ID, Model: "billing-priced-model", InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	get := func(g Group) Group {
		r, err := s.GetGroup(ctx, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("group pays first and personal funds cover the remainder", func(t *testing.T) {
		g := create("Shared budget", "1")
		u1, d1, k1 := billingIntegrationPrincipal(t, ctx, s, "group-a-"+suffix)
		u2, d2, k2 := billingIntegrationPrincipal(t, ctx, s, "group-b-"+suffix)
		credit(u1, "0.1")
		credit(u2, "3")
		g = members(g, "add", u1.ID, u2.ID)
		r1 := reserve(u1, k1, "group-cost-a-"+suffix)
		r2 := reserve(u2, k2, "group-cost-b-"+suffix)
		released := reserve(u2, k2, "group-release-"+suffix)
		if err := s.ReleaseBilling(ctx, released.RequestID, now); err != nil {
			t.Fatal(err)
		}
		billingIntegrationComplete(t, ctx, s, u1, d1, k1, r1.RequestID, now, 1000000, "billing-priced-model")
		billingIntegrationComplete(t, ctx, s, u2, d2, k2, r2.RequestID, now, 500000, "billing-priced-model")
		settled, err := s.SettleBilling(ctx, r1.RequestID, now)
		if err != nil {
			t.Fatal(err)
		}
		if settled.UncoveredUSD == nil || *settled.UncoveredUSD != "0.000000000000" {
			t.Fatalf("uncovered cost: %+v", settled)
		}
		if _, err := s.SettleBilling(ctx, r2.RequestID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleBilling(ctx, r1.RequestID, now); err != nil {
			t.Fatal(err)
		}
		g = get(g)
		if g.UsedUSD != "1.000000000000" || g.RemainingUSD != "0.000000000000" {
			t.Fatalf("group accumulated: %+v", g)
		}
		request := billingIntegrationAdmission(u1, d1, k1, "group-both-exhausted-"+suffix, now)
		admission, err := s.AdmitRequest(ctx, request)
		if err != nil || admission.Billing.GroupPeriodID != nil {
			t.Fatalf("exhausted group should fall back to personal funds: %+v %v", admission, err)
		}
		if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: request.Quota.RequestID, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second), InputTokens: 200000, ActualModel: "billing-priced-model"}); err != nil {
			t.Fatal(err)
		}
		settled, err = s.SettleBilling(ctx, request.Quota.RequestID, now)
		if err != nil || settled.UncoveredUSD == nil || *settled.UncoveredUSD != "0.100000000000" {
			t.Fatalf("accepted request records uncovered remainder: %+v %v", settled, err)
		}
		if err := s.SettleRequest(ctx, request.Quota.RequestID, now); err != nil {
			t.Fatal(err)
		}
		request = billingIntegrationAdmission(u1, d1, k1, "group-all-exhausted-"+suffix, now)
		_, err = s.AdmitRequest(ctx, request)
		var fundsErr *InsufficientFundsError
		if !errors.As(err, &fundsErr) {
			t.Fatalf("all exhausted must reject: %v", err)
		}
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_reservations WHERE request_id=$1)+(SELECT count(*) FROM quota_reservations WHERE request_id=$1)+(SELECT count(*) FROM usage_requests WHERE request_id=$1)`, request.Quota.RequestID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected admission wrote records: %d %v", count, err)
		}
		g, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "2", Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
		if g.UsedUSD != "1.000000000000" || g.RemainingUSD != "1.000000000000" {
			t.Fatalf("amount edit reset usage: %+v", g)
		}
		admission, err = s.AdmitRequest(ctx, request)
		if err != nil || admission.Billing.GroupPeriodID == nil {
			t.Fatalf("available group admits member without personal funds: %+v %v", admission, err)
		}
		if err := s.ReleaseRequest(ctx, request.Quota.RequestID, now); err != nil {
			t.Fatal(err)
		}
		state, err := s.GetBillingState(ctx, u2.ID, 10, 0)
		if err != nil || state.Group == nil || state.Group.ID != g.ID {
			t.Fatalf("billing group summary: %+v %v", state, err)
		}
		if len(state.Ledger) == 0 || state.Ledger[0].GroupPeriodID == nil {
			t.Fatal("ledger missing group binding")
		}
	})

	t.Run("moving and reopening retain original admission period", func(t *testing.T) {
		g := create("Original", "5")
		next := create("Destination", "5")
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "group-move-"+suffix)
		credit(u, "5")
		members(g, "add", u.ID)
		r := reserve(u, k, "group-old-period-"+suffix)
		members(g, "remove", u.ID)
		members(next, "add", u.ID)
		reset := PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "5", Period: "custom", CustomDays: 2}
		g, err = s.PutGroup(ctx, reset)
		if err != nil {
			t.Fatal(err)
		}
		if r.GroupPeriodID == nil || g.PeriodID == *r.GroupPeriodID {
			t.Fatal("period did not reopen")
		}
		billingIntegrationComplete(t, ctx, s, u, d, k, r.RequestID, now, 2000000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, r.RequestID, now); err != nil {
			t.Fatal(err)
		}
		var oldUsed string
		if err := s.db.QueryRowContext(ctx, `SELECT used_usd::text FROM group_usage_periods WHERE id=$1`, r.GroupPeriodID).Scan(&oldUsed); err != nil || oldUsed != "2.000000000000" {
			t.Fatalf("original period = %s %v", oldUsed, err)
		}
		if get(g).UsedUSD != "0.000000000000" || get(next).UsedUSD != "0.000000000000" {
			t.Fatal("in-flight request charged current group/period")
		}
		replay, err := s.PutGroup(ctx, reset)
		if err != nil || replay.PeriodID != g.PeriodID {
			t.Fatalf("reopen replay = %+v %v", replay, err)
		}
		reset.LimitUSD = "6"
		if _, err := s.PutGroup(ctx, reset); !errors.Is(err, ErrConflict) {
			t.Fatalf("idempotency mismatch: %v", err)
		}
		if _, err := s.ArchiveGroup(ctx, ArchiveGroupParams{BillingWriteParams: write(), GroupID: next.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("archive nonempty: %v", err)
		}
		archived, err := s.ArchiveGroup(ctx, ArchiveGroupParams{BillingWriteParams: write(), GroupID: g.ID})
		if err != nil || archived.ArchivedAt == nil {
			t.Fatalf("archive empty: %v", err)
		}
	})

	t.Run("member limit edits and rejoining retain paid usage", func(t *testing.T) {
		originalNow := now
		defer func() { now = originalNow }()
		cap := "5"
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Member cap", LimitUSD: "50", MemberLimitUSD: &cap, Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "group-cap-edit-"+suffix)
		credit(u, "10")
		members(g, "add", u.ID)
		first := reserve(u, k, "group-cap-edit-first-"+suffix)
		billingIntegrationComplete(t, ctx, s, u, d, k, first.RequestID, now, 3000000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, first.RequestID, now); err != nil {
			t.Fatal(err)
		}
		g = get(g)
		if len(g.Members) != 1 || g.Members[0].UsedUSD != "3.000000000000" || g.Members[0].RemainingUSD == nil || *g.Members[0].RemainingUSD != "2.000000000000" {
			t.Fatalf("member usage: %+v", g)
		}
		members(g, "remove", u.ID)
		g = members(g, "add", u.ID)
		if g.Members[0].UsedUSD != "3.000000000000" || *g.Members[0].RemainingUSD != "2.000000000000" {
			t.Fatalf("rejoining restored allowance: %+v", g)
		}
		g, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "60", Period: "day"})
		if err != nil || g.MemberLimitUSD == nil || *g.MemberLimitUSD != "5.000000000000" || g.UsedUSD != "3.000000000000" {
			t.Fatalf("omitted cap must preserve current limit and usage: %+v %v", g, err)
		}
		cap = "2"
		lower := PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "60", MemberLimitUSD: &cap, Period: "day"}
		g, err = s.PutGroup(ctx, lower)
		if err != nil || g.Members[0].UsedUSD != "3.000000000000" || *g.Members[0].RemainingUSD != "0.000000000000" {
			t.Fatalf("lowered cap: %+v %v", g, err)
		}
		if _, err := s.PutGroup(ctx, lower); err != nil {
			t.Fatalf("cap update replay: %v", err)
		}
		lower.MemberLimitUSD = nil
		lower.MemberLimitSet = true
		if _, err := s.PutGroup(ctx, lower); !errors.Is(err, ErrConflict) {
			t.Fatalf("null replay must conflict with prior amount: %v", err)
		}
		personal := reserve(u, k, "group-cap-edit-personal-"+suffix)
		if personal.GroupPeriodID != nil {
			t.Fatalf("member over cap must use personal funds: %+v", personal)
		}
		billingIntegrationComplete(t, ctx, s, u, d, k, personal.RequestID, now, 1000000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, personal.RequestID, now); err != nil {
			t.Fatal(err)
		}
		cap = "0"
		g, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "60", MemberLimitUSD: &cap, Period: "day"})
		if err != nil || *g.MemberLimitUSD != "0.000000000000" {
			t.Fatalf("zero cap: %+v %v", g, err)
		}
		zero := reserve(u, k, "group-cap-edit-zero-"+suffix)
		if zero.GroupPeriodID != nil {
			t.Fatalf("zero cap must disable group funds: %+v", zero)
		}
		if err := s.ReleaseBilling(ctx, zero.RequestID, now); err != nil {
			t.Fatal(err)
		}
		g, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "60", MemberLimitSet: true, Period: "day"})
		if err != nil || g.MemberLimitUSD != nil || g.Members[0].RemainingUSD != nil || g.Members[0].UsedUSD != "3.000000000000" {
			t.Fatalf("clearing cap must preserve usage: %+v %v", g, err)
		}
		state, err := s.GetBillingState(ctx, u.ID, 10, 0)
		if err != nil || state.GroupMemberUsedUSD == nil || *state.GroupMemberUsedUSD != "3.000000000000" || state.GroupMemberRemainingUSD != nil || state.BalanceUSD != "9.000000000000" {
			t.Fatalf("own unlimited member summary: %+v %v", state, err)
		}
		cap = "6"
		g, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "60", MemberLimitUSD: &cap, Period: "day"})
		if err != nil || *g.Members[0].RemainingUSD != "3.000000000000" {
			t.Fatalf("raised cap: %+v %v", g, err)
		}
		late := reserve(u, k, "group-cap-edit-late-"+suffix)
		oldPeriod := g.PeriodID
		newAnchor := now.Add(-time.Hour)
		g, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "60", Period: "day", StartsAt: &newAnchor})
		if err != nil || g.PeriodID == oldPeriod || g.Members[0].UsedUSD != "0.000000000000" || *g.Members[0].RemainingUSD != "6.000000000000" {
			t.Fatalf("anchor edit starts fresh member period: %+v %v", g, err)
		}
		billingIntegrationComplete(t, ctx, s, u, d, k, late.RequestID, now, 1000000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, late.RequestID, now); err != nil {
			t.Fatal(err)
		}
		var oldUsed, oldLimit string
		if err := s.db.QueryRowContext(ctx, `SELECT m.used_usd::text,p.member_limit_usd::text FROM group_member_usage m JOIN group_usage_periods p ON p.id=m.period_id WHERE m.period_id=$1 AND m.user_id=$2`, oldPeriod, u.ID).Scan(&oldUsed, &oldLimit); err != nil || oldUsed != "4.000000000000" || oldLimit != "6.000000000000" {
			t.Fatalf("old period member snapshot: used=%s limit=%s err=%v", oldUsed, oldLimit, err)
		}
		if get(g).Members[0].UsedUSD != "0.000000000000" {
			t.Fatal("late settlement changed current member usage")
		}
		current := reserve(u, k, "group-cap-edit-current-"+suffix)
		billingIntegrationComplete(t, ctx, s, u, d, k, current.RequestID, now, 1000000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, current.RequestID, now); err != nil {
			t.Fatal(err)
		}
		now = g.PeriodEndsAt
		g = get(g)
		if g.PeriodID == *current.GroupPeriodID || g.Members[0].UsedUSD != "0.000000000000" || *g.Members[0].RemainingUSD != "6.000000000000" {
			t.Fatalf("natural cycle renews member allowance: %+v", g)
		}
	})

	t.Run("group funding preserves subscription and cash balances", func(t *testing.T) {
		g := create("Subscription and cash", "1")
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "group-sub-"+suffix)
		credit(u, "0.3")
		if _, err := s.PutSubscription(ctx, PutSubscriptionParams{BillingWriteParams: write(), UserID: u.ID, Tier: "day", AllowanceUSD: "0.2", PeriodCount: 1}); err != nil {
			t.Fatal(err)
		}
		members(g, "add", u.ID)
		first := reserve(u, k, "group-sub-first-"+suffix)
		second := reserve(u, k, "group-sub-second-"+suffix)
		billingIntegrationComplete(t, ctx, s, u, d, k, first.RequestID, now, 250000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, first.RequestID, now); err != nil {
			t.Fatal(err)
		}
		state, err := s.GetBillingState(ctx, u.ID, 10, 0)
		if err != nil || state.BalanceUSD != "0.300000000000" || state.Subscriptions[0].RemainingUSD != "0.200000000000" {
			t.Fatalf("subscription then cash fallback: %+v %v", state, err)
		}
		billingIntegrationComplete(t, ctx, s, u, d, k, second.RequestID, now, 750000, "billing-priced-model")
		settled, err := s.SettleBilling(ctx, second.RequestID, now)
		if err != nil || *settled.UncoveredUSD != "0.000000000000" {
			t.Fatalf("in-flight uncovered cost: %+v %v", settled, err)
		}
		if get(g).UsedUSD != "1.000000000000" {
			t.Fatal("group should pay the complete covered cost")
		}
	})

	t.Run("model queries are exempt and exhausted group falls back to personal funding", func(t *testing.T) {
		g := create("Blocked", "0")
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "group-endpoints-"+suffix)
		credit(u, "1")
		members(g, "add", u.ID)
		models := billingIntegrationAdmission(u, d, k, "group-models-"+suffix, now)
		models.Usage.Endpoint = "models"
		models.Billing = nil
		models.Quota.Limits.KeyRequestsPerMinute = 1
		if _, err := s.AdmitRequest(ctx, models); err != nil {
			t.Fatalf("nonbillable model query: %v", err)
		}
		if err := s.ReleaseRequest(ctx, models.Quota.RequestID, now); err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []string{"responses", "responses.compact", "gemini.generateContent", "gemini.streamGenerateContent"} {
			request := billingIntegrationAdmission(u, d, k, "group-cap-"+endpoint+suffix, now)
			request.Usage.Endpoint = endpoint
			request.Quota.Limits.KeyRequestsPerMinute = 1
			_, err := s.AdmitRequest(ctx, request)
			if !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("%s must preserve request limits: %v", endpoint, err)
			}
			var groupError *GroupQuotaExceededError
			if errors.As(err, &groupError) {
				t.Fatalf("%s should not reject solely for empty group: %v", endpoint, err)
			}
		}
	})

	t.Run("future start and exact renewal boundary", func(t *testing.T) {
		future := now.Add(time.Hour)
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Future", LimitUSD: "1", Period: "custom", CustomDays: 2, StartsAt: &future})
		if err != nil {
			t.Fatal(err)
		}
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "group-future-"+suffix)
		credit(u, "5")
		members(g, "add", u.ID)
		params := billingIntegrationAdmission(u, d, k, "group-future-request-"+suffix, now)
		personal, err := s.AdmitRequest(ctx, params)
		if err != nil || personal.Billing.GroupPeriodID != nil {
			t.Fatalf("future group should use personal funding: %+v %v", personal, err)
		}
		if err := s.ReleaseRequest(ctx, params.Quota.RequestID, now); err != nil {
			t.Fatal(err)
		}
		params = billingIntegrationAdmission(u, d, k, "group-start-request-"+suffix, future)
		admission, err := s.AdmitRequest(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if admission.Billing.GroupPeriodID == nil || *admission.Billing.GroupPeriodID != g.PeriodID {
			t.Fatal("start boundary snapshot")
		}
		if err := s.ReleaseRequest(ctx, params.Quota.RequestID, future); err != nil {
			t.Fatal(err)
		}
		end := future.Add(2 * 24 * time.Hour)
		params = billingIntegrationAdmission(u, d, k, "group-renew-request-"+suffix, end)
		renewed, err := s.AdmitRequest(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if *renewed.Billing.GroupPeriodID == g.PeriodID {
			t.Fatal("exact end did not renew")
		}
		var start, timeEnd time.Time
		if err := s.db.QueryRowContext(ctx, `SELECT starts_at,ends_at FROM group_usage_periods WHERE id=$1`, renewed.Billing.GroupPeriodID).Scan(&start, &timeEnd); err != nil || !start.Equal(end) || timeEnd.Sub(start) != 48*time.Hour {
			t.Fatalf("renewed period: %v %v %v", start, timeEnd, err)
		}
		if err := s.ReleaseRequest(ctx, params.Quota.RequestID, end); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("membership atomicity and competing group additions", func(t *testing.T) {
		left, right := create("Left", "5"), create("Right", "5")
		u, _, _ := billingIntegrationPrincipal(t, ctx, s, "group-race-"+suffix)
		writes := []BillingWriteParams{write(), write()}
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i, g := range []Group{left, right} {
			wg.Add(1)
			go func(i int, g Group) {
				defer wg.Done()
				_, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: writes[i], GroupID: g.ID, UserIDs: []string{u.ID}, Action: "add"})
				errs <- err
			}(i, g)
		}
		wg.Wait()
		close(errs)
		success, conflict := 0, 0
		for err := range errs {
			if err == nil {
				success++
			} else if errors.Is(err, ErrConflict) {
				conflict++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatalf("competing memberships success=%d conflict=%d", success, conflict)
		}
		if get(left).MemberCount+get(right).MemberCount != 1 {
			t.Fatal("user entered multiple groups")
		}
		fresh, _, _ := billingIntegrationPrincipal(t, ctx, s, "group-atomic-"+suffix)
		missing, _ := newUUID()
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: left.ID, UserIDs: []string{fresh.ID, missing}, Action: "add"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid bulk membership: %v", err)
		}
		state, err := s.GetBillingState(ctx, fresh.ID, 1, 0)
		if err != nil || state.Group != nil {
			t.Fatalf("partial bulk membership: %v", err)
		}
	})

	t.Run("parallel settlements never lose shared usage", func(t *testing.T) {
		g := create("Concurrent", "0.5")
		const count = 12
		ids := make([]string, 0, count)
		for i := 0; i < count; i++ {
			u, d, k := billingIntegrationPrincipal(t, ctx, s, fmt.Sprintf("group-par-%d-%s", i, suffix))
			credit(u, "1")
			members(g, "add", u.ID)
			id := fmt.Sprintf("group-par-%d-%s", i, suffix)
			reserve(u, k, id)
			billingIntegrationComplete(t, ctx, s, u, d, k, id, now, 100000, "billing-priced-model")
			ids = append(ids, id)
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
		if used := get(g).UsedUSD; used != "0.500000000000" {
			t.Fatalf("parallel total = %s", used)
		}
	})

	t.Run("natural renewal and archival preserve in-flight settlements", func(t *testing.T) {
		originalNow := now
		defer func() { now = originalNow }()
		g := create("Renew then archive", "5")
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "group-natural-"+suffix)
		credit(u, "5")
		members(g, "add", u.ID)
		old := reserve(u, k, "group-natural-old-"+suffix)
		now = g.PeriodEndsAt
		current := reserve(u, k, "group-natural-current-"+suffix)
		if *old.GroupPeriodID == *current.GroupPeriodID {
			t.Fatal("natural renewal retained old binding")
		}
		billingIntegrationComplete(t, ctx, s, u, d, k, old.RequestID, originalNow, 1000000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, old.RequestID, now); err != nil {
			t.Fatal(err)
		}
		if get(g).UsedUSD != "0.000000000000" {
			t.Fatal("late previous-cycle settlement charged renewed period")
		}
		members(g, "remove", u.ID)
		if _, err := s.ArchiveGroup(ctx, ArchiveGroupParams{BillingWriteParams: write(), GroupID: g.ID}); err != nil {
			t.Fatal(err)
		}
		billingIntegrationComplete(t, ctx, s, u, d, k, current.RequestID, now, 2000000, "billing-priced-model")
		if _, err := s.SettleBilling(ctx, current.RequestID, now); err != nil {
			t.Fatalf("archival prevented accepted settlement: %v", err)
		}
		archived := get(g)
		if archived.ArchivedAt == nil || archived.UsedUSD != "2.000000000000" {
			t.Fatalf("archived usage: %+v", archived)
		}
		var oldUsage string
		if err := s.db.QueryRowContext(ctx, `SELECT used_usd::text FROM group_usage_periods WHERE id=$1`, old.GroupPeriodID).Scan(&oldUsage); err != nil || oldUsage != "1.000000000000" {
			t.Fatalf("prior usage=%s err=%v", oldUsage, err)
		}
	})
}
