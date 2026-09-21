//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestUpstreamAccountAccessPostgresIntegration(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: os.Getenv("TEST_DATABASE_URL"), MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	owner := usageSummaryUser(t, ctx, s, "access-owner-"+suffix)
	member := usageSummaryUser(t, ctx, s, "access-member-"+suffix)
	shared := upstreamIntegrationAccountID("access-shared-" + suffix)
	exclusive := upstreamIntegrationAccountID("access-exclusive-" + suffix)
	for _, id := range []string{shared, exclusive} {
		if err := s.EnsureUpstreamAccount(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	params := SetUpstreamAccountAccessParams{AccountID: exclusive, Mode: "exclusive", UserIDs: []string{member.ID}, Reason: "Dedicated account", ActorUserID: owner.ID}
	if _, err := s.SetUpstreamAccountAccess(ctx, params); err != nil {
		t.Fatal(err)
	}
	assertEligible := func(user string, want int) {
		t.Helper()
		got, err := s.EligibleUpstreamAccounts(ctx, user, []string{shared, exclusive})
		if err != nil || len(got) != want {
			t.Fatalf("eligibility %s=%v %v", user, got, err)
		}
	}
	assertEligible(owner.ID, 1)
	assertEligible(member.ID, 2)
	if got, err := s.SelectUpstreamAccount(ctx, owner.ID, []string{shared, exclusive}, time.Now()); err != nil || got != shared {
		t.Fatalf("owner bypassed exclusivity: %s %v", got, err)
	}
	if _, err := s.SelectUpstreamAccount(ctx, owner.ID, []string{exclusive}, time.Now()); !errors.Is(err, ErrNoUpstreamAccount) {
		t.Fatalf("denied selection=%v", err)
	}
	if _, err := s.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{AccountID: exclusive, Weight: 0, ActorUserID: owner.ID}); err != nil {
		t.Fatal(err)
	}
	assertEligible(member.ID, 2)
	if _, err := s.SelectUpstreamAccount(ctx, member.ID, []string{exclusive}, time.Now()); !errors.Is(err, ErrNoUpstreamAccount) {
		t.Fatalf("zero weight selected=%v", err)
	}
	if err := s.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: exclusive, MaskedEmail: "e***@example.com", Plan: "pro", Status: UpstreamAccountStatusAvailable}}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertEligible(owner.ID, 1)
	assertEligible(member.ID, 2)
	for _, bad := range []SetUpstreamAccountAccessParams{
		{AccountID: exclusive, Mode: "exclusive", Reason: "Empty", ActorUserID: owner.ID},
		{AccountID: exclusive, Mode: "exclusive", UserIDs: []string{"00000000-0000-0000-0000-000000000001"}, Reason: "Missing", ActorUserID: owner.ID},
		{AccountID: exclusive, Mode: "shared", Reason: "Audit fails", ActorUserID: owner.ID, SourceIP: "invalid-ip"},
	} {
		if _, err := s.SetUpstreamAccountAccess(ctx, bad); err == nil {
			t.Fatal("invalid write committed")
		}
		assertEligible(owner.ID, 1)
	}
	var audits int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE subject_id=$1 AND event_type='upstream_account.access_changed'`, exclusive).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count=%d %v", audits, err)
	}
	params.Mode = "shared"
	params.UserIDs = nil
	if _, err := s.SetUpstreamAccountAccess(ctx, params); err != nil {
		t.Fatal(err)
	}
	assertEligible(owner.ID, 2)
}
