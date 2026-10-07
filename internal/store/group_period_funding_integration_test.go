//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestGroupExpiryFundingPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "group-expiry-funding")
	write := func() BillingWriteParams { return billingIntegrationWrite(t, user.ID, "group expiry funding", now) }
	g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Single cycle", LimitUSD: "10", Period: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, Action: "add", UserIDs: []string{user.ID}}); err != nil {
		t.Fatal(err)
	}
	// Accept before expiry but finish afterwards. Settlement must continue to
	// use the admission-time period even though new requests cannot use it.
	lateID := "group-expiry-late"
	admitted, err := s.AdmitRequest(ctx, billingIntegrationAdmission(user, device, key, lateID, now))
	if err != nil || admitted.Billing == nil || admitted.Billing.GroupPeriodID == nil || *admitted.Billing.GroupPeriodID != g.PeriodID {
		t.Fatalf("before expiry admission = %+v, %v", admitted, err)
	}
	now = g.PeriodEndsAt
	rejectedID := "group-expiry-rejected"
	_, err = s.AdmitRequest(ctx, billingIntegrationAdmission(user, device, key, rejectedID, now))
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) || insufficient.RetryAfter != 0 {
		t.Fatalf("expired only funding must reject without renewal hint: %v", err)
	}
	var artifacts int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_reservations WHERE request_id=$1)
		+(SELECT count(*) FROM quota_reservations WHERE request_id=$1)+(SELECT count(*) FROM usage_requests WHERE request_id=$1)`, rejectedID).Scan(&artifacts); err != nil || artifacts != 0 {
		t.Fatalf("rejected request artifacts=%d: %v", artifacts, err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: lateID, State: "completed", HTTPStatus: 200,
		CompletedAt: now.Add(time.Second), InputTokens: 2000000, ActualModel: "billing-priced-model"}); err != nil {
		t.Fatal(err)
	}
	settled, err := s.SettleBilling(ctx, lateID, now)
	if err != nil {
		t.Fatal(err)
	}
	groupBillingAssertSplit(t, settled, "2", "0", "0")
	replayed, err := s.SettleBilling(ctx, lateID, now.Add(time.Hour))
	if err != nil || replayed.SettledAt == nil || !replayed.SettledAt.Equal(*settled.SettledAt) {
		t.Fatalf("late settlement replay = %+v, %v", replayed, err)
	}
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: user.ID, USDAmount: "3"}); err != nil {
		t.Fatal(err)
	}
	personalID := "group-expiry-personal"
	personal, err := s.AdmitRequest(ctx, billingIntegrationAdmission(user, device, key, personalID, now))
	if err != nil || personal.Billing == nil || personal.Billing.GroupPeriodID != nil || personal.Billing.CashLotCutoff == nil {
		t.Fatalf("expired group must fall back to cash: %+v, %v", personal, err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: personalID, State: "completed", HTTPStatus: 200,
		CompletedAt: now.Add(time.Second), InputTokens: 1000000, ActualModel: "billing-priced-model"}); err != nil {
		t.Fatal(err)
	}
	settled, err = s.SettleBilling(ctx, personalID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	groupBillingAssertSplit(t, settled, "0", "1", "0")
	state, err := s.GetBillingState(ctx, user.ID, 10, 0)
	if err != nil || state.Group == nil || state.Group.ID != g.ID || state.Group.PeriodID != g.PeriodID || state.Group.PeriodCount != 1 ||
		state.Group.CurrentPeriodNumber != 1 || state.Group.ExpiresAt == nil || !state.Group.ExpiresAt.Equal(g.PeriodEndsAt) ||
		state.GroupMemberUsedUSD == nil || *state.GroupMemberUsedUSD != "2.000000000000" || state.BalanceUSD != "2.000000000000" {
		t.Fatalf("expired group membership, snapshot and usage = %+v, %v", state, err)
	}
	// Extending retains the original timeline and resumes group funding in
	// cycle 2; charges accepted during expiry remain bound to personal funds.
	count := 2
	extended, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), GroupID: g.ID, Name: g.Name, LimitUSD: "10", Period: "day", PeriodCount: &count})
	if err != nil || extended.CurrentPeriodNumber != 2 || extended.PeriodID == g.PeriodID || extended.UsedUSD != "0.000000000000" {
		t.Fatalf("extension = %+v, %v", extended, err)
	}
	resumedID := "group-expiry-resumed"
	resumed, err := s.AdmitRequest(ctx, billingIntegrationAdmission(user, device, key, resumedID, now))
	if err != nil || resumed.Billing == nil || resumed.Billing.GroupPeriodID == nil || *resumed.Billing.GroupPeriodID != extended.PeriodID {
		t.Fatalf("extension must resume group funding: %+v, %v", resumed, err)
	}
	if err := s.ReleaseRequest(ctx, resumedID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleBilling(ctx, personalID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetGroup(ctx, g.ID)
	if err != nil || current.MemberCount != 1 || current.UsedUSD != "0.000000000000" || current.Members[0].UsedUSD != "0.000000000000" {
		t.Fatalf("replay after extension changed usage or membership: %+v, %v", current, err)
	}
}

func TestGroupPeriodRetryAfterPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	base := time.Now().UTC().Truncate(time.Microsecond)
	now := base
	s.now = func() time.Time { return now }
	for i, tc := range []struct {
		name        string
		count       int
		startsAfter time.Duration
		requestAt   time.Duration
		limit       string
		memberLimit *string
		want        time.Duration
	}{
		{name: "future single cycle", count: 1, startsAfter: time.Hour, limit: "1", want: time.Hour},
		{name: "exhausted single cycle", count: 1, limit: "1"},
		{name: "exhausted with renewal", count: 2, limit: "1", want: 24 * time.Hour},
		{name: "final cycle", count: 2, requestAt: 24 * time.Hour, limit: "1"},
		{name: "expired single", count: 1, requestAt: 24 * time.Hour, limit: "1"},
		{name: "expired idle multi cycle", count: 4, requestAt: 8 * 24 * time.Hour, limit: "1"},
		{name: "unlimited", count: 0, requestAt: 8 * 24 * time.Hour, limit: "1", want: 24 * time.Hour},
		{name: "zero group limit", count: 2, limit: "0"},
		{name: "zero member limit", count: 2, limit: "1", memberLimit: new("0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now = base
			user, device, key := billingIntegrationPrincipal(t, ctx, s, fmt.Sprintf("group-retry-%d", i))
			write := func() BillingWriteParams { return billingIntegrationWrite(t, user.ID, "group renewal retry", now) }
			start := base.Add(tc.startsAfter)
			g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: tc.name, LimitUSD: tc.limit, Period: "day",
				StartsAt: &start, PeriodCount: &tc.count, MemberLimitUSD: tc.memberLimit})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, Action: "add", UserIDs: []string{user.ID}}); err != nil {
				t.Fatal(err)
			}
			now = base.Add(tc.requestAt)
			g, err = s.GetGroup(ctx, g.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.startsAfter == 0 {
				if _, err := s.db.ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=limit_usd WHERE id=$1`, g.PeriodID); err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.AdmitRequest(ctx, billingIntegrationAdmission(user, device, key, fmt.Sprintf("retry-%d", i), now))
			var insufficient *InsufficientFundsError
			if !errors.As(err, &insufficient) || insufficient.RetryAfter != tc.want {
				t.Fatalf("retry error=%v, detail=%+v, want=%v", err, insufficient, tc.want)
			}
		})
	}
}
