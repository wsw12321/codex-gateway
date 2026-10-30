//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

// The pool is captured in the same transaction as billing admission. A later
// policy revocation must not rewrite the capability already handed to CPA.
func TestBillingAdmissionSnapshotsGroupUpstreamPolicyPostgresIntegration(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: os.Getenv("TEST_DATABASE_URL"), MaxOpenConns: 8, MaxIdleConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	billingIntegrationModelScopes(t, ctx, s, "billing-priced-model")
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "permission-snapshot")
	group, err := s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "create permission snapshot group", now),
		Name:               "Permission snapshot", LimitUSD: "100", Period: "month",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "join permission snapshot group", now),
		GroupID:            group.ID, UserIDs: []string{user.ID}, Action: "add",
	}); err != nil {
		t.Fatal(err)
	}
	first := upstreamIntegrationAccountID("permission-snapshot-first")
	second := upstreamIntegrationAccountID("permission-snapshot-second")
	for _, id := range []string{first, second} {
		if err := s.EnsureUpstreamAccount(ctx, id, now); err != nil {
			t.Fatal(err)
		}
	}
	setPolicy := func(id string) {
		t.Helper()
		if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{
			BillingWriteParams: billingIntegrationWrite(t, user.ID, "set permission snapshot pool", now),
			GroupID:            group.ID, Provider: UpstreamProviderCodex, Strategy: "selected", AccountIDs: []string{id},
		}); err != nil {
			t.Fatal(err)
		}
	}
	setPolicy(first)
	requestID := billingIntegrationRequestID("permission-snapshot", "admission", 1)
	params := billingIntegrationAdmission(user, device, key, requestID, now)
	params.UpstreamProvider = UpstreamProviderCodex
	admitted, err := s.AdmitRequest(ctx, params)
	if err != nil || admitted.Billing == nil || len(admitted.Billing.AllocationGroups) != 1 {
		t.Fatalf("admission snapshot = %+v, %v", admitted, err)
	}
	want := []UpstreamAllocationGroup{{GroupID: group.ID, AccountIDs: []string{first}}}
	if !reflect.DeepEqual(admitted.Billing.AllocationGroups, want) {
		t.Fatalf("admission pool = %+v; want %+v", admitted.Billing.AllocationGroups, want)
	}
	setPolicy(second)
	if !reflect.DeepEqual(admitted.Billing.AllocationGroups, want) {
		t.Fatalf("policy revocation changed immutable pool = %+v; want %+v", admitted.Billing.AllocationGroups, want)
	}
	if err := s.ReleaseRequest(ctx, requestID, now.Add(time.Second)); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
