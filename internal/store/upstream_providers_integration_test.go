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

func providerIntegrationStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return repository, ctx
}

func TestUpstreamProvidersIsolateManagementAndSelection(t *testing.T) {
	repository, ctx := providerIntegrationStore(t)
	agy := repository.WithUpstreamProvider(UpstreamProviderAntigravity)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	at := time.Now().UTC().Truncate(time.Microsecond)
	actor := usageSummaryUser(t, ctx, repository, "provider-actor-"+suffix)
	other := usageSummaryUser(t, ctx, repository, "provider-other-"+suffix)
	codexID := upstreamIntegrationAccountID("provider-codex-" + suffix)
	agyID := upstreamIntegrationAccountID("provider-agy-" + suffix)
	codexSnapshot := UpstreamAccountSnapshot{ID: codexID, MaskedEmail: "c***@example.com", Plan: "plus", Status: "available"}
	agySnapshot := UpstreamAccountSnapshot{ID: agyID, DisplayName: "primary", Status: "available"}
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{codexSnapshot}, at); err != nil {
		t.Fatal(err)
	}
	if err := agy.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{agySnapshot}, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		store    *Store
		ownID    string
		otherID  string
		snapshot UpstreamAccountSnapshot
	}{
		{"codex", repository, codexID, agyID, codexSnapshot},
		{"antigravity", agy, agyID, codexID, agySnapshot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts, err := tc.store.ListUpstreamAccounts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			own := findUpstreamAccount(accounts, tc.ownID)
			if own == nil || own.Status != "available" || own.DisplayName != tc.snapshot.DisplayName || findUpstreamAccount(accounts, tc.otherID) != nil {
				t.Fatalf("provider account isolation: own=%+v other=%+v", own, findUpstreamAccount(accounts, tc.otherID))
			}
			if err := tc.store.EnsureUpstreamAccount(ctx, tc.otherID, at); !errors.Is(err, ErrConflict) {
				t.Fatalf("cross-provider placeholder error = %v", err)
			}
			collision := tc.snapshot
			collision.ID = tc.otherID
			if err := tc.store.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{collision}, at.Add(time.Minute)); !errors.Is(err, ErrConflict) {
				t.Fatalf("cross-provider snapshot error = %v", err)
			}
			if _, err := tc.store.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{
				AccountID: tc.otherID, Weight: 0, ActorUserID: actor.ID,
			}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-provider weight edit = %v", err)
			}
			if _, err := tc.store.SetUpstreamAccountConcurrentLimit(ctx, SetUpstreamAccountConcurrentLimitParams{
				AccountID: tc.otherID, Limit: 99, ActorUserID: actor.ID,
			}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-provider concurrent limit edit = %v", err)
			}
			if _, err := tc.store.SetUpstreamAccountAccess(ctx, SetUpstreamAccountAccessParams{
				AccountID: tc.otherID, Mode: "exclusive", UserIDs: []string{other.ID}, Reason: "isolation", ActorUserID: actor.ID,
			}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-provider access edit = %v", err)
			}
			if _, err := tc.store.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{
				AccountID: tc.ownID, Weight: 3, ActorUserID: actor.ID,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tc.store.SetUpstreamAccountConcurrentLimit(ctx, SetUpstreamAccountConcurrentLimitParams{
				AccountID: tc.ownID, Limit: 4, ActorUserID: actor.ID,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tc.store.SetUpstreamAccountAccess(ctx, SetUpstreamAccountAccessParams{
				AccountID: tc.ownID, Mode: "exclusive", UserIDs: []string{actor.ID}, Reason: "owned account", ActorUserID: actor.ID,
			}); err != nil {
				t.Fatal(err)
			}
			if err := tc.store.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{tc.snapshot}, at.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			accounts, err = tc.store.ListUpstreamAccounts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			own = findUpstreamAccount(accounts, tc.ownID)
			if own == nil || own.AllocationWeight != 3 || own.ConcurrentLimit != 4 {
				t.Fatalf("synchronization overwrote local preferences: %+v", own)
			}
			access, err := tc.store.ListUpstreamAccountAccess(ctx)
			if err != nil {
				t.Fatal(err)
			}
			foundAccess := false
			for _, item := range access {
				if item.AccountID == tc.otherID {
					t.Fatal("access listing leaked another provider")
				}
				if item.AccountID == tc.ownID {
					foundAccess = item.Mode == "exclusive" && len(item.UserIDs) == 1 && item.UserIDs[0] == actor.ID
				}
			}
			if !foundAccess {
				t.Fatal("own access preference missing")
			}
			ids := []string{tc.ownID, tc.otherID}
			eligible, err := tc.store.EligibleUpstreamAccounts(ctx, actor.ID, ids)
			if err != nil || len(eligible) != 1 || eligible[0] != tc.ownID {
				t.Fatalf("provider eligibility = %v, %v", eligible, err)
			}
			limits, err := tc.store.EligibleUpstreamAccountLimits(ctx, actor.ID, ids)
			if err != nil || len(limits) != 1 || limits[0].ID != tc.ownID || limits[0].ConcurrentLimit != 4 {
				t.Fatalf("provider limits = %v, %v", limits, err)
			}
			eligible, err = tc.store.EligibleUpstreamAccounts(ctx, other.ID, ids)
			if err != nil || len(eligible) != 0 {
				t.Fatalf("provider exclusive eligibility = %v, %v", eligible, err)
			}
			selected, err := tc.store.SelectUpstreamAccount(ctx, actor.ID, ids, at)
			if err != nil || selected != tc.ownID {
				t.Fatalf("provider allocation selected %q, %v", selected, err)
			}
			if _, err := tc.store.SelectUpstreamAccount(ctx, actor.ID, []string{tc.otherID}, at); !errors.Is(err, ErrNoUpstreamAccount) {
				t.Fatalf("foreign-provider-only selection = %v", err)
			}
		})
	}
	if err := agy.SyncUpstreamAccounts(ctx, nil, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var codexStatus, agyStatus string
	if err := repository.db.QueryRowContext(ctx, `SELECT (SELECT status FROM upstream_accounts WHERE id=$1), (SELECT status FROM upstream_accounts WHERE id=$2)`, codexID, agyID).Scan(&codexStatus, &agyStatus); err != nil {
		t.Fatal(err)
	}
	if codexStatus != "available" || agyStatus != "unavailable" {
		t.Fatalf("empty AGY snapshot affected Codex: codex=%s agy=%s", codexStatus, agyStatus)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE upstream_accounts SET provider='unsafe' WHERE id=$1`, agyID); err == nil {
		t.Fatal("database accepted unknown provider")
	}
}

func TestUpstreamProvidersIsolateUsageAndCostSummaries(t *testing.T) {
	repository, ctx := providerIntegrationStore(t)
	agy := repository.WithUpstreamProvider(UpstreamProviderAntigravity)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	at := time.Now().UTC().Truncate(time.Microsecond)
	actor := usageSummaryUser(t, ctx, repository, "provider-usage-"+suffix)
	device := usageSummaryDevice(t, ctx, repository, actor.ID, "provider-usage-"+suffix)
	key := usageSummaryKey(t, ctx, repository, actor.ID, device.ID, suffix, at.Add(-time.Hour))
	codexID := upstreamIntegrationAccountID("provider-usage-codex-" + suffix)
	agyID := upstreamIntegrationAccountID("provider-usage-agy-" + suffix)
	oldAt := at.AddDate(0, -4, 0)
	for _, fixture := range []struct {
		store *Store
		id    string
		cost  string
	}{
		{repository, codexID, "2"},
		{agy, agyID, "7"},
	} {
		for _, requestedAt := range []time.Time{at.Add(-time.Minute), oldAt} {
			requestID := fixture.id + "-" + strconv.FormatInt(requestedAt.UnixNano(), 36)
			if _, err := repository.BeginUsageRequest(ctx, BeginUsageRequestParams{
				RequestID: requestID, UserID: actor.ID, DeviceID: device.ID, APIKeyID: key.ID,
				Model: "provider-fixture", Endpoint: "responses", RequestedAt: requestedAt,
			}); err != nil {
				t.Fatal(err)
			}
			completed, err := fixture.store.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
				RequestID: requestID, State: "completed", HTTPStatus: 200, CompletedAt: requestedAt.Add(time.Second),
				InputTokens: 20, OutputTokens: 5, UpstreamAccountID: fixture.id,
			})
			if err != nil || completed.UpstreamAccountID == nil || *completed.UpstreamAccountID != fixture.id {
				t.Fatalf("scoped usage completion = %+v, %v", completed, err)
			}
			// The shared ledger must accept known IDs irrespective of the store
			// used to finish or replay usage, without changing provider ownership.
			if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
				RequestID: requestID, State: "completed", HTTPStatus: 200, CompletedAt: requestedAt.Add(time.Second),
				InputTokens: 20, OutputTokens: 5, UpstreamAccountID: fixture.id,
			}); err != nil {
				t.Fatalf("shared completion replay: %v", err)
			}
			if _, err := repository.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries
				(user_id,entry_type,amount_usd,cash_delta_usd,request_id,upstream_account_id,
				 model,actual_cost_usd,charged_usd,group_charged_usd,personal_charged_usd,uncovered_usd,usage_requested_at,created_at)
				VALUES ($1,'usage_charge',$2::numeric,0,$3,$4,'provider-fixture',$2::numeric,0,0,0,$2::numeric,$5,$5)`,
				actor.ID, fixture.cost, requestID, fixture.id, requestedAt); err != nil {
				t.Fatal(err)
			}
			if err := repository.AttributeUsageRequest(ctx, requestID, fixture.id); err != nil {
				t.Fatalf("shared settled attribution: %v", err)
			}
		}
	}
	if err := repository.AggregateUsageMonth(ctx, oldAt, "UTC"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		store   *Store
		ownID   string
		otherID string
		cost    string
		allCost string
	}{
		{repository, codexID, agyID, "2", "4"},
		{agy, agyID, codexID, "7", "14"},
	} {
		for _, all := range []bool{false, true} {
			summaries, err := tc.store.SummarizeUpstreamAccounts(ctx, UpstreamAccountSummaryFilter{
				All: all, From: at.Add(-time.Hour), Until: at,
				LiveFrom: time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatal(err)
			}
			own := findUpstreamAccountSummary(summaries, &tc.ownID)
			wantRequests, wantCost := int64(1), tc.cost
			if all {
				wantRequests, wantCost = 2, tc.allCost
			}
			if own == nil || own.RequestCount != wantRequests || own.InputTokens != 20*wantRequests || !equalAllocationDecimal(own.EquivalentCostUSD, wantCost) || findUpstreamAccountSummary(summaries, &tc.otherID) != nil {
				t.Fatalf("%s all=%t summary = %+v", tc.store.upstreamProviderName(), all, own)
			}
			if tc.store == agy && findUpstreamAccountSummary(summaries, nil) != nil {
				t.Fatal("AGY summary includes global unattributed history")
			}
		}
		allocations, err := tc.store.ListUpstreamAccountAllocations(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		var expectedShare string
		if err := repository.db.QueryRowContext(ctx, `SELECT ($1::numeric / sum(l.actual_cost_usd))::text FROM billing_ledger_entries l JOIN upstream_accounts a ON a.id=l.upstream_account_id WHERE a.provider=$2 AND l.entry_type='usage_charge' AND COALESCE(l.usage_requested_at,l.created_at)>=$3 AND COALESCE(l.usage_requested_at,l.created_at)<$4`, tc.cost, tc.store.upstreamProviderName(), at.Add(-24*time.Hour), at).Scan(&expectedShare); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, allocation := range allocations {
			if allocation.AccountID == tc.otherID {
				t.Fatal("allocation statistics leaked another provider")
			}
			if allocation.AccountID == tc.ownID {
				found = true
				if !equalAllocationDecimal(allocation.CostUSD, tc.cost) || !equalAllocationDecimal(allocation.CostShare, expectedShare) {
					t.Fatalf("allocation costs include another provider: %+v want share %s", allocation, expectedShare)
				}
			}
		}
		if !found {
			t.Fatal("own allocation statistics missing")
		}
	}
}
