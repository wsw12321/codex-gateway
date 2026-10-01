//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGroupMemberUsageSurvivesHistoryCleanupPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	old := now.Add(-100 * 24 * time.Hour)
	actor := globalUsageIntegrationUser(t, ctx, s, "group-cleanup-actor", UserRoleMember)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "group-cleanup-member")
	cap := "10"
	group, err := s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: billingIntegrationWrite(t, actor.ID, "long group cycle", old),
		Name:               "Cleanup invariant", LimitUSD: "100", Period: "custom", CustomDays: 200,
		StartsAt: &old, MemberLimitUSD: &cap, MemberLimitSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{
		BillingWriteParams: billingIntegrationWrite(t, actor.ID, "join group", old),
		GroupID:            group.ID, UserIDs: []string{user.ID}, Action: "add",
	}); err != nil {
		t.Fatal(err)
	}
	requestID := "group-cleanup-historical-payment"
	billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, requestID,
		old.Add(time.Hour), 5_000_000, "1", "billing-priced-model")
	if _, err := s.SettleBilling(ctx, requestID, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Truncate(24 * time.Hour).Add(-30 * 24 * time.Hour)
	job, err := s.CreateInformationCleanupJob(ctx, InformationCleanupParams{
		OperationID: billingIntegrationWrite(t, actor.ID, "cleanup", now).OperationID,
		ActorUserID: actor.ID, RetentionDays: 30, Cutoff: cutoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30 && job.Status != "completed"; i++ {
		value, err := s.RunInformationCleanupBatch(ctx, 100)
		if err != nil || value == nil {
			t.Fatalf("cleanup batch: %+v %v", value, err)
		}
		job = *value
	}
	if job.Status != "completed" {
		t.Fatalf("cleanup did not complete: %+v", job)
	}
	var artifacts int
	if err := s.DB().QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM billing_reservations WHERE request_id=$1)+
		(SELECT count(*) FROM billing_charge_allocations WHERE request_id=$1)+
		(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1)+
		(SELECT count(*) FROM usage_requests WHERE request_id=$1)`, requestID).Scan(&artifacts); err != nil || artifacts != 0 {
		t.Fatalf("history survived cleanup: %d %v", artifacts, err)
	}
	assertUsage := func(want string) {
		t.Helper()
		g, err := s.GetGroup(ctx, group.ID)
		if err != nil || g.PeriodID != group.PeriodID || len(g.Members) != 1 ||
			!equalAllocationDecimal(g.UsedUSD, want) || !equalAllocationDecimal(g.Members[0].UsedUSD, want) {
			t.Fatalf("cleanup or rejoin changed counters: %+v %v", g, err)
		}
	}
	assertUsage("5")
	for _, action := range []string{"remove", "add"} {
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{
			BillingWriteParams: billingIntegrationWrite(t, actor.ID, "rejoin", now),
			GroupID:            group.ID, UserIDs: []string{user.ID}, Action: action,
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertUsage("5")
	newRequest := "group-cleanup-cap-still-enforced"
	billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, newRequest, now,
		10_000_000, "1", "billing-priced-model")
	settled, err := s.SettleBilling(ctx, newRequest, now.Add(time.Second))
	if err != nil || settled.GroupChargedUSD == nil || settled.PersonalChargedUSD == nil || settled.UncoveredUSD == nil ||
		!equalAllocationDecimal(*settled.GroupChargedUSD, "5") || !equalAllocationDecimal(*settled.PersonalChargedUSD, "0") ||
		!equalAllocationDecimal(*settled.UncoveredUSD, "5") {
		t.Fatalf("cleaned history restored group allowance: %+v %v", settled, err)
	}
	assertUsage("10")
	_, err = s.ReserveBilling(ctx, BillingReservationParams{
		RequestID: "group-cleanup-all-funds-exhausted", UserID: user.ID, APIKeyID: key.ID,
		Model: "billing-priced-model", InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0",
		OutputUSDPerMillion: "0", Now: now.Add(2 * time.Second),
	})
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("exhausted member accepted after cleanup: %v", err)
	}
}
