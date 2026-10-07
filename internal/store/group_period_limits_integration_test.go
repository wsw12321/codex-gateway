//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestGroupPeriodLimitsPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 10, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Microsecond)
	now := base
	s.now = func() time.Time { return now }
	actor := globalUsageIntegrationUser(t, ctx, s, fmt.Sprintf("group-term-actor-%d", base.UnixNano()), UserRoleMember)
	write := func() BillingWriteParams { return billingIntegrationWrite(t, actor.ID, "group term test", now) }
	ptr := func(count int) *int { return &count }
	create := func(count *int) Group {
		t.Helper()
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Term", LimitUSD: "10", Period: "day", PeriodCount: count})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	edit := func(g Group, count *int) (Group, error) {
		return s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "10", Period: g.Period,
			CustomDays: g.CustomDays, PeriodCount: count})
	}
	get := func(g Group) Group {
		t.Helper()
		result, err := s.GetGroup(ctx, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertTerm := func(g Group, current, count int, expiry *time.Time) {
		t.Helper()
		if g.CurrentPeriodNumber != current || g.PeriodCount != count || (g.ExpiresAt == nil) != (expiry == nil) ||
			(expiry != nil && !g.ExpiresAt.Equal(*expiry)) {
			t.Fatalf("term = %d/%d %v; want %d/%d %v", g.CurrentPeriodNumber, g.PeriodCount, g.ExpiresAt, current, count, expiry)
		}
	}

	t.Run("default final expiry retains period and counters", func(t *testing.T) {
		now = base
		g := create(nil)
		end := base.Add(24 * time.Hour)
		assertTerm(g, 1, 1, &end)
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, UserIDs: []string{actor.ID}, Action: "add"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=7 WHERE id=$1`, g.PeriodID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO group_member_usage(period_id,user_id,used_usd) VALUES($1,$2,3)`, g.PeriodID, actor.ID); err != nil {
			t.Fatal(err)
		}
		now = end
		expired := get(g)
		assertTerm(expired, 1, 1, &end)
		if expired.PeriodID != g.PeriodID || expired.UsedUSD != "7.000000000000" || expired.MemberCount != 1 || len(expired.Members) != 1 || expired.Members[0].UsedUSD != "3.000000000000" {
			t.Fatalf("expiry changed period, usage or membership: %+v", expired)
		}
		now = end.Add(100 * 24 * time.Hour)
		if after := get(g); after.PeriodID != g.PeriodID {
			t.Fatalf("idle expiry issued another allowance: %+v", after)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, UserIDs: []string{actor.ID}, Action: "remove"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("count edits retain usage and idle periods count", func(t *testing.T) {
		now = base
		g := create(ptr(5))
		if _, err := s.db.ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=2 WHERE id=$1`, g.PeriodID); err != nil {
			t.Fatal(err)
		}
		now = base.Add(time.Hour)
		expanded, err := edit(g, ptr(7))
		if err != nil || expanded.PeriodID != g.PeriodID || expanded.UsedUSD != "2.000000000000" || !expanded.PeriodStartsAt.Equal(g.PeriodStartsAt) || !expanded.PeriodEndsAt.Equal(g.PeriodEndsAt) {
			t.Fatalf("count-only edit reset period: %+v %v", expanded, err)
		}
		end := base.Add(7 * 24 * time.Hour)
		assertTerm(expanded, 1, 7, &end)
		now = base.Add(2*24*time.Hour + time.Hour)
		third := get(expanded)
		assertTerm(third, 3, 7, &end)
		if third.PeriodID == g.PeriodID || third.UsedUSD != "0.000000000000" || !third.PeriodStartsAt.Equal(base.Add(2*24*time.Hour)) {
			t.Fatalf("idle advance = %+v", third)
		}
		var rows int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM group_usage_periods WHERE group_id=$1`, g.ID).Scan(&rows); err != nil || rows != 2 {
			t.Fatalf("idle cycles should not create rows: %d %v", rows, err)
		}
		if _, err := edit(third, ptr(2)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("count below current must fail: %v", err)
		}
		shortened, err := edit(third, ptr(3))
		if err != nil {
			t.Fatal(err)
		}
		end = base.Add(3 * 24 * time.Hour)
		assertTerm(shortened, 3, 3, &end)
		omitted, err := edit(shortened, nil)
		if err != nil || omitted.PeriodID != shortened.PeriodID {
			t.Fatalf("omitted count edit: %+v %v", omitted, err)
		}
		assertTerm(omitted, 3, 3, &end)
		now = end
		if final := get(g); final.PeriodID != third.PeriodID || final.CurrentPeriodNumber != 3 {
			t.Fatalf("final cycle renewed: %+v", final)
		}
	})

	t.Run("expired extensions use original timeline and schedule resets number", func(t *testing.T) {
		now = base
		g := create(nil)
		now = base.Add(3*24*time.Hour + time.Hour)
		stillExpired, err := edit(g, ptr(3))
		if err != nil || stillExpired.PeriodID != g.PeriodID {
			t.Fatalf("short extension issued allowance: %+v %v", stillExpired, err)
		}
		end := base.Add(3 * 24 * time.Hour)
		assertTerm(stillExpired, 1, 3, &end)
		resumed, err := edit(stillExpired, ptr(5))
		if err != nil || resumed.PeriodID == g.PeriodID || !resumed.PeriodStartsAt.Equal(base.Add(3*24*time.Hour)) {
			t.Fatalf("extended timeline = %+v %v", resumed, err)
		}
		end = base.Add(5 * 24 * time.Hour)
		assertTerm(resumed, 4, 5, &end)
		now = base.Add(8*24*time.Hour + time.Hour)
		unlimited, err := edit(resumed, ptr(0))
		if err != nil {
			t.Fatal(err)
		}
		assertTerm(unlimited, 9, 0, nil)
		finite, err := edit(unlimited, ptr(9))
		if err != nil {
			t.Fatal(err)
		}
		end = base.Add(9 * 24 * time.Hour)
		assertTerm(finite, 9, 9, &end)
		past := base.Add(-30 * 24 * time.Hour)
		reopened, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "10",
			Period: "week", StartsAt: &past, PeriodCount: ptr(1)})
		if err != nil || reopened.PeriodID == finite.PeriodID || reopened.PeriodStartsAt.After(now) || !reopened.PeriodEndsAt.After(now) {
			t.Fatalf("past schedule reset = %+v %v", reopened, err)
		}
		assertTerm(reopened, 1, 1, &reopened.PeriodEndsAt)
		future := now.Add(48 * time.Hour)
		reopened, err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "10",
			Period: "week", StartsAt: &future, PeriodCount: ptr(2)})
		if err != nil || !reopened.PeriodStartsAt.Equal(future) {
			t.Fatalf("future schedule reset = %+v %v", reopened, err)
		}
		end = future.Add(14 * 24 * time.Hour)
		assertTerm(reopened, 1, 2, &end)
	})

	t.Run("omitted legacy fingerprints and explicit count replay", func(t *testing.T) {
		now = base
		params := PutGroupParams{BillingWriteParams: write(), Name: "Legacy operation", LimitUSD: "10", Period: "day"}
		original, err := s.PutGroup(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE group_operations SET response=response-'period_count'-'current_period_number'-'expires_at' WHERE operation_id=$1`, params.OperationID); err != nil {
			t.Fatal(err)
		}
		now = base.Add(2 * 24 * time.Hour)
		if _, err := edit(original, ptr(5)); err != nil {
			t.Fatal(err)
		}
		replayed, err := s.PutGroup(ctx, params)
		if err != nil || replayed.PeriodID != original.PeriodID {
			t.Fatalf("legacy replay = %+v %v", replayed, err)
		}
		assertTerm(replayed, 1, 1, &original.PeriodEndsAt)
		params.PeriodCount = ptr(1)
		if _, err := s.PutGroup(ctx, params); !errors.Is(err, ErrConflict) {
			t.Fatalf("explicit count must change fingerprint: %v", err)
		}
		params.BillingWriteParams, params.PeriodCount = write(), ptr(0)
		unlimited, err := s.PutGroup(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		replayed, err = s.PutGroup(ctx, params)
		if err != nil || replayed.PeriodID != unlimited.PeriodID {
			t.Fatalf("explicit replay = %+v %v", replayed, err)
		}
		assertTerm(replayed, 1, 0, nil)
		params.PeriodCount = ptr(1)
		if _, err := s.PutGroup(ctx, params); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed count replay must conflict: %v", err)
		}
	})

	t.Run("long custom terms validate final dates without duration overflow", func(t *testing.T) {
		now = base
		params := PutGroupParams{BillingWriteParams: write(), Name: "Long term", LimitUSD: "10", Period: "custom", CustomDays: 106751, PeriodCount: ptr(20)}
		g, err := s.PutGroup(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		end := base.AddDate(0, 0, 20*106751)
		assertTerm(g, 1, 20, &end)
		params.BillingWriteParams, params.PeriodCount = write(), ptr(99)
		if _, err := s.PutGroup(ctx, params); !errors.Is(err, ErrInvalid) {
			t.Fatalf("out of range custom term: %v", err)
		}
		var operations int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM group_operations WHERE operation_id=$1`, params.OperationID).Scan(&operations); err != nil || operations != 0 {
			t.Fatalf("invalid date did not roll back operation: %d %v", operations, err)
		}
		boundary := time.Date(9999, 12, 30, 0, 0, 0, 0, time.UTC)
		params = PutGroupParams{BillingWriteParams: write(), Name: "Boundary", LimitUSD: "10", Period: "day", StartsAt: &boundary, PeriodCount: ptr(1)}
		if _, err := s.PutGroup(ctx, params); err != nil {
			t.Fatalf("valid date boundary: %v", err)
		}
		params.BillingWriteParams, params.PeriodCount = write(), ptr(2)
		if _, err := s.PutGroup(ctx, params); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid expiry date boundary: %v", err)
		}
		params.BillingWriteParams, params.PeriodCount = write(), ptr(0)
		unlimited, err := s.PutGroup(ctx, params)
		if err != nil {
			t.Fatalf("unlimited boundary period: %v", err)
		}
		now = unlimited.PeriodEndsAt
		final := get(unlimited)
		if final.PeriodID != unlimited.PeriodID || final.CurrentPeriodNumber != 1 || groupHasNextPeriod(&final.GroupSummary) || groupPeriodActive(&final.GroupSummary, now) {
			t.Fatalf("boundary renewal must retain the expired snapshot: %+v", final)
		}
		user, _, key := billingIntegrationPrincipal(t, ctx, s, fmt.Sprintf("group-calendar-boundary-%d", base.UnixNano()))
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: final.ID, UserIDs: []string{user.ID}, Action: "add"}); err != nil {
			t.Fatal(err)
		}
		request := BillingReservationParams{RequestID: fmt.Sprintf("group-calendar-boundary-request-%d", base.UnixNano()),
			UserID: user.ID, APIKeyID: key.ID, Model: "billing-priced-model", InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: now}
		var insufficient *InsufficientFundsError
		if _, err := s.ReserveBilling(ctx, request); !errors.As(err, &insufficient) || insufficient.RetryAfter != 0 {
			t.Fatalf("calendar boundary without personal funds must reject without retry: %v", err)
		}
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: user.ID, USDAmount: "1"}); err != nil {
			t.Fatal(err)
		}
		reservation, err := s.ReserveBilling(ctx, request)
		if err != nil || reservation.GroupPeriodID != nil || reservation.CashLotCutoff == nil {
			t.Fatalf("calendar boundary must fall back to personal funds: %+v %v", reservation, err)
		}
	})
}
