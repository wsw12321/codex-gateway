//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestUpstreamAllocationPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	at := time.Now().UTC().Truncate(time.Microsecond)
	accountA := upstreamIntegrationAccountID("allocation-a-" + suffix)
	accountB := upstreamIntegrationAccountID("allocation-b-" + suffix)
	unknown := upstreamIntegrationAccountID("allocation-new-" + suffix)
	actor := usageSummaryUser(t, ctx, repository, "allocation-actor-"+suffix)
	otherUser := usageSummaryUser(t, ctx, repository, "allocation-other-"+suffix)
	write := func(id string, weight int) UpstreamAccount {
		t.Helper()
		account, err := repository.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{
			AccountID: id, Weight: weight, ActorUserID: actor.ID, At: at,
		})
		if err != nil {
			t.Fatalf("set weight %s=%d: %v", id, weight, err)
		}
		if account.AllocationWeight != weight || account.ID != id {
			t.Fatalf("confirmed allocation = %+v", account)
		}
		return account
	}
	selection := func(ids []string, want string) {
		t.Helper()
		id, err := repository.SelectUpstreamAccount(ctx, ids, at)
		if err != nil || id != want {
			t.Fatalf("SelectUpstreamAccount(%v) = %q, %v; want %q", ids, id, err, want)
		}
	}
	selection([]string{unknown}, unknown)
	var unknownCount int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM upstream_accounts WHERE id = $1`, unknown).Scan(&unknownCount); err != nil || unknownCount != 0 {
		t.Fatalf("selection created unknown account: count=%d err=%v", unknownCount, err)
	}

	snapshot := []UpstreamAccountSnapshot{
		{ID: accountA, MaskedEmail: "a***@allocation.example", Plan: "plus", Status: UpstreamAccountStatusAvailable},
		{ID: accountB, MaskedEmail: "b***@allocation.example", Plan: "pro", Status: UpstreamAccountStatusAvailable},
	}
	if err := repository.SyncUpstreamAccounts(ctx, snapshot, at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var weight int
	if err := repository.db.QueryRowContext(ctx, `SELECT allocation_weight FROM upstream_accounts WHERE id = $1`, accountA).Scan(&weight); err != nil || weight != 1 {
		t.Fatalf("new account default = %d, %v", weight, err)
	}
	write(accountA, 5)
	write(accountB, 20)
	if err := repository.SyncUpstreamAccounts(ctx, snapshot, at.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureUpstreamAccount(ctx, accountA, at); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT allocation_weight FROM upstream_accounts WHERE id = $1`, accountA).Scan(&weight); err != nil || weight != 5 {
		t.Fatalf("sync/ensure overwrote configured weight: %d, %v", weight, err)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE upstream_accounts SET allocation_weight = -1 WHERE id = $1`, accountA); err == nil {
		t.Fatal("PostgreSQL accepted negative allocation weight")
	}
	if _, err := repository.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{
		AccountID: accountA, Weight: 99, ActorUserID: actor.ID, SourceIP: "invalid-ip", At: at,
	}); err == nil {
		t.Fatal("invalid audit source IP succeeded")
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT allocation_weight FROM upstream_accounts WHERE id = $1`, accountA).Scan(&weight); err != nil || weight != 5 {
		t.Fatalf("failed audit did not roll back weight: %d, %v", weight, err)
	}
	var audits int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events
		WHERE actor_user_id = $1 AND event_type = 'upstream_account.allocation_weight_changed'
		AND subject_id IN ($2, $3) AND metadata ? 'previous_weight' AND metadata ? 'weight'`,
		actor.ID, accountA, accountB).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("committed allocation audits = %d, %v; want 2", audits, err)
	}
	if _, err := repository.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{
		AccountID: unknown, Weight: 1, ActorUserID: actor.ID, At: at,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown weight edit error = %v", err)
	}

	// A settled usage_charge is the source of truth. Time is [at-24h, at),
	// request timestamps take precedence over settlement/ledger creation time,
	// and old entries without a request timestamp use creation time.
	for index, fixture := range []struct {
		accountID string
		userID    string
		model     string
		cost      string
		requested any
		created   time.Time
		entryType string
	}{
		{accountA, actor.ID, "allocation-model-a", "1", at.Add(-24 * time.Hour), at.Add(-time.Hour), "usage_charge"},
		{accountA, otherUser.ID, "allocation-model-b", "2", at.Add(-time.Hour), at.Add(-time.Minute), "usage_charge"},
		{accountA, actor.ID, "allocation-model-a", "1", nil, at.Add(-2 * time.Hour), "usage_charge"},
		{accountA, actor.ID, "allocation-model-a", "100", at.Add(-24*time.Hour - time.Microsecond), at.Add(-time.Minute), "usage_charge"},
		{accountA, actor.ID, "allocation-model-a", "100", at, at, "usage_charge"},
		{accountA, actor.ID, "allocation-model-a", "100", nil, at.Add(-24*time.Hour - time.Microsecond), "usage_charge"},
		{accountA, actor.ID, "allocation-model-a", "100", at.Add(-time.Hour), at.Add(-time.Minute), "adjustment"},
		{accountB, actor.ID, "allocation-model-a", "6", at.Add(-time.Hour), at.Add(-time.Minute), "usage_charge"},
	} {
		if _, err := repository.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries
			(user_id, entry_type, amount_usd, cash_delta_usd, request_id, upstream_account_id,
			 model, actual_cost_usd, charged_usd, uncovered_usd, usage_requested_at, created_at)
			VALUES ($1,$2,$3::numeric,0,$4,$5,$6,$3::numeric,0,$3::numeric,$7,$8)`,
			fixture.userID, fixture.entryType, fixture.cost, fmt.Sprintf("allocation-ledger-%s-%d", suffix, index),
			fixture.accountID, fixture.model, fixture.requested, fixture.created); err != nil {
			t.Fatalf("ledger fixture %d: %v", index, err)
		}
	}
	device := usageSummaryDevice(t, ctx, repository, actor.ID, "allocation-"+suffix)
	key := usageSummaryKey(t, ctx, repository, actor.ID, device.ID, suffix, at.Add(-time.Hour))
	for _, state := range []string{"in_progress", "completed"} {
		requestID := "allocation-unsettled-" + state + "-" + suffix
		if _, err := repository.BeginUsageRequest(ctx, BeginUsageRequestParams{
			RequestID: requestID, UserID: actor.ID, DeviceID: device.ID, APIKeyID: key.ID,
			Model: "allocation-model-a", Endpoint: "responses", RequestedAt: at.Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.AttributeUsageRequest(ctx, requestID, accountB); err != nil {
			t.Fatal(err)
		}
		if state == "completed" {
			if _, err := repository.db.ExecContext(ctx, `UPDATE usage_requests SET state = 'completed',
				http_status = 200, completed_at = $2, input_tokens = 10000000 WHERE request_id = $1`,
				requestID, at.Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	selection([]string{accountA, accountB}, accountB) // target 2:8, settled 4:6
	selection([]string{accountA}, accountA)
	selection([]string{accountA, unknown}, unknown)
	allocations, err := repository.ListUpstreamAccountAllocations(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	// Other integration cases can leave available accounts with later snapshot
	// timestamps. Reference targets use the whole persisted available pool;
	// actual selection above deliberately uses only the supplied candidates.
	var totalTargetWeight int64
	for _, allocation := range allocations {
		if !equalAllocationDecimal(allocation.TargetShare, "0") {
			totalTargetWeight += int64(allocation.AllocationWeight)
		}
	}
	found := 0
	for _, allocation := range allocations {
		switch allocation.AccountID {
		case accountA:
			found++
			if !equalAllocationDecimal(allocation.CostUSD, "4") {
				t.Fatalf("account A rolling allocation = %+v", allocation)
			}
		case accountB:
			found++
			if !equalAllocationDecimal(allocation.CostUSD, "6") {
				t.Fatalf("account B rolling allocation = %+v", allocation)
			}
		}
		if allocation.AccountID == accountA || allocation.AccountID == accountB {
			got, ok := new(big.Rat).SetString(allocation.TargetShare)
			want := new(big.Rat).SetFrac64(int64(allocation.AllocationWeight), totalTargetWeight)
			if !ok || new(big.Rat).Abs(new(big.Rat).Sub(got, want)).Cmp(new(big.Rat).SetFrac64(1, 10000000000000000)) > 0 {
				t.Fatalf("reference target = %+v; total available weight %d", allocation, totalTargetWeight)
			}
		}
	}
	if found != 2 {
		t.Fatalf("rolling statistics returned %d fixture accounts, want 2", found)
	}
	write(accountB, 0)
	selection([]string{accountA, accountB}, accountA)
	if _, err := repository.SelectUpstreamAccount(ctx, []string{accountB}, at); !errors.Is(err, ErrNoUpstreamAccount) {
		t.Fatalf("single zero-weight candidate error = %v", err)
	}
	write(accountA, 0)
	if _, err := repository.SelectUpstreamAccount(ctx, []string{accountA, accountB}, at); !errors.Is(err, ErrNoUpstreamAccount) {
		t.Fatalf("all zero-weight candidates error = %v", err)
	}
	selection([]string{accountA, accountB, unknown}, unknown)
	allocations, err = repository.ListUpstreamAccountAllocations(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, allocation := range allocations {
		if allocation.AccountID == accountA || allocation.AccountID == accountB {
			if !equalAllocationDecimal(allocation.TargetShare, "0") || equalAllocationDecimal(allocation.CostShare, "0") {
				t.Fatalf("drained account lost actual share or retained target: %+v", allocation)
			}
		}
	}
}
