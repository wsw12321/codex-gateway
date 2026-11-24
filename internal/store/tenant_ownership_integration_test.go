//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// TestTenantOwnershipPostgresIntegration checks the stage 1 invariant: a
// tenant account never reaches a platform path, and platform behavior is
// otherwise unchanged.
func TestTenantOwnershipPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := strconv.FormatInt(now.UnixNano(), 36)
	actor := usageSummaryUser(t, ctx, s, "tenant-actor-"+suffix)
	member := usageSummaryUser(t, ctx, s, "tenant-member-"+suffix)
	tenant := usageSummaryUser(t, ctx, s, "tenant-owner-"+suffix)

	account := func(owner string) string {
		t.Helper()
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		id := hex.EncodeToString(raw)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_accounts(id,provider,status,owner_user_id)
			VALUES($1,'codex','available',NULLIF($2,'')::uuid)`, id, owner); err != nil {
			t.Fatal(err)
		}
		return id
	}
	platform := account("")
	tenantAccount := account(tenant.ID)
	suspended := account(tenant.ID)
	if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET admin_suspended=true WHERE id=$1`, suspended); err != nil {
		t.Fatal(err)
	}
	unknown := upstreamIntegrationAccountID("tenant-unknown-" + suffix)
	candidates := []string{platform, tenantAccount, suspended, unknown}

	settings, err := s.MarketplaceSettings(ctx)
	if err != nil || settings.Revision < 1 || settings.CommissionRate == "" {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
	// The test database is shared; restore whatever this test found.
	t.Cleanup(func() {
		current, err := s.MarketplaceSettings(context.Background())
		if err != nil {
			t.Errorf("read settings for restore: %v", err)
			return
		}
		if _, err := s.SetMarketplaceSettings(context.Background(), SetMarketplaceSettingsParams{
			Enabled: settings.Enabled, CommissionRate: settings.CommissionRate,
			MaxAccountsPerUser: settings.MaxAccountsPerUser, MaxPlansPerUser: settings.MaxPlansPerUser,
			AllowMemberSuppliers: settings.AllowMemberSuppliers, ExpectedRevision: current.Revision, ActorUserID: actor.ID,
		}); err != nil {
			t.Errorf("restore settings: %v", err)
		}
	})
	setEnabled := func(enabled bool) {
		t.Helper()
		current, err := s.MarketplaceSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next, err := s.SetMarketplaceSettings(ctx, SetMarketplaceSettingsParams{
			Enabled: enabled, CommissionRate: "0.05", MaxAccountsPerUser: 10, MaxPlansPerUser: 5,
			AllowMemberSuppliers: true, ExpectedRevision: current.Revision, ActorUserID: actor.ID, SourceIP: "192.0.2.51",
		})
		if err != nil || next.Enabled != enabled || next.CommissionRate != "0.05" || next.Revision != current.Revision+1 {
			t.Fatalf("set enabled=%v: %+v %v", enabled, next, err)
		}
	}

	assertEligible := func(want ...string) {
		t.Helper()
		slices.Sort(want)
		got, err := s.EligibleUpstreamAccounts(ctx, member.ID, candidates)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("eligible=%v err=%v, want %v", got, err, want)
		}
		limits, err := s.EligibleUpstreamAccountLimits(ctx, member.ID, candidates)
		if err != nil || len(limits) != len(want) {
			t.Fatalf("eligible limits=%v err=%v, want %v", limits, err, want)
		}
		for _, item := range limits {
			if !slices.Contains(want, item.ID) {
				t.Fatalf("eligible limit for %s, want %v", item.ID, want)
			}
		}
	}

	// Disabled marketplace: unknown candidates keep their previous behavior.
	setEnabled(false)
	assertEligible(platform, unknown)
	// The tenant's own accounts are not platform accounts either.
	if got, err := s.EligibleUpstreamAccounts(ctx, tenant.ID, []string{tenantAccount}); err != nil || len(got) != 0 {
		t.Fatalf("tenant eligibility on platform path=%v %v", got, err)
	}
	for i := 0; i < 20; i++ {
		got, err := s.SelectUpstreamAccount(ctx, member.ID, []string{platform, tenantAccount, suspended}, now)
		if err != nil || got != platform {
			t.Fatalf("selected %s err=%v", got, err)
		}
	}
	if _, err := s.SelectUpstreamAccount(ctx, member.ID, []string{tenantAccount, suspended}, now); !errors.Is(err, ErrNoUpstreamAccount) {
		t.Fatalf("tenant-only selection err=%v", err)
	}

	// Enabled marketplace: an account the Gateway has no row for may be a
	// tenant login in progress, so it is not eligible.
	setEnabled(true)
	assertEligible(platform)
	if got, err := s.SelectUpstreamAccount(ctx, member.ID, []string{unknown}, now); !errors.Is(err, ErrNoUpstreamAccount) {
		t.Fatalf("unknown selection=%s err=%v", got, err)
	}

	// Owner listings and management only see platform accounts.
	listed, err := s.ListUpstreamAccounts(ctx)
	if err != nil || findUpstreamAccount(listed, platform) == nil || findUpstreamAccount(listed, tenantAccount) != nil {
		t.Fatalf("listed=%d platform=%v tenant=%v err=%v", len(listed), findUpstreamAccount(listed, platform) != nil, findUpstreamAccount(listed, tenantAccount) != nil, err)
	}
	if _, err := s.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{AccountID: tenantAccount, Weight: 3, ActorUserID: actor.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner weight on tenant account err=%v", err)
	}
	if _, err := s.SetUpstreamAccountConcurrentLimit(ctx, SetUpstreamAccountConcurrentLimitParams{AccountID: tenantAccount, Limit: 3, ActorUserID: actor.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner concurrency on tenant account err=%v", err)
	}
	if _, err := s.SetUpstreamAccountAccess(ctx, SetUpstreamAccountAccessParams{AccountID: tenantAccount, Mode: "exclusive", UserIDs: []string{member.ID}, Reason: "tenant", ActorUserID: actor.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner exclusivity on tenant account err=%v", err)
	}

	// Group pools: selected pools reject tenant accounts, "all" pools skip them.
	write := func() BillingWriteParams {
		return billingIntegrationWrite(t, actor.ID, "tenant ownership integration", now)
	}
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "tenant pool " + suffix, LimitUSD: "10", Period: "week"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: group.ID,
		Provider: UpstreamProviderCodex, Strategy: "selected", AccountIDs: []string{platform, tenantAccount}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("group pool with tenant account err=%v", err)
	}
	if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: group.ID,
		Provider: UpstreamProviderCodex, Strategy: "all", AccountIDs: []string{}}); err != nil {
		t.Fatal(err)
	}
	pool, err := s.ListGroupUpstreamAllocationGroup(ctx, group.ID, UpstreamProviderCodex)
	if err != nil || !slices.Contains(pool.AccountIDs, platform) || slices.Contains(pool.AccountIDs, tenantAccount) {
		t.Fatalf("all pool=%v err=%v", pool.AccountIDs, err)
	}
	var snapshot UpstreamAllocationGroup
	if err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var err error
		snapshot, err = snapshotGroupUpstreamAllocationGroupTx(ctx, tx, group.ID, UpstreamProviderCodex)
		return err
	}); err != nil || !slices.Contains(snapshot.AccountIDs, platform) || slices.Contains(snapshot.AccountIDs, tenantAccount) {
		t.Fatalf("admission snapshot=%v err=%v", snapshot.AccountIDs, err)
	}

	// Per-user scopes reject tenant accounts.
	if _, err := s.SetUserUpstreamAccess(ctx, SetUserUpstreamAccessParams{UserID: member.ID,
		Mode: "selected", AccountIDs: []string{tenantAccount}, Reason: "tenant", ActorUserID: actor.ID}); err == nil {
		t.Fatal("per-user scope accepted a tenant account")
	}

	// Database triggers back the application checks.
	for _, statement := range []string{
		`INSERT INTO group_upstream_accounts(group_id,provider,upstream_account_id) VALUES('` + group.ID + `','codex','` + tenantAccount + `')`,
		`INSERT INTO upstream_account_users(upstream_account_id,user_id) VALUES('` + tenantAccount + `','` + member.ID + `')`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); !errors.Is(mapDBError("trigger", err), ErrInvalid) {
			t.Fatalf("trigger accepted %q: %v", statement, err)
		}
	}
	// Ownership is set once and never moves.
	other := usageSummaryUser(t, ctx, s, "tenant-other-"+suffix)
	if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET owner_user_id=$2 WHERE id=$1`, tenantAccount, other.ID); !errors.Is(mapDBError("owner", err), ErrInvalid) {
		t.Fatalf("owner change err=%v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET owner_user_id=NULL WHERE id=$1`, tenantAccount); !errors.Is(mapDBError("owner", err), ErrInvalid) {
		t.Fatalf("owner removal err=%v", err)
	}
	// A platform account already in a platform pool cannot become a tenant account.
	pooled := account("")
	if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: group.ID,
		Provider: UpstreamProviderCodex, Strategy: "selected", AccountIDs: []string{pooled}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET owner_user_id=$2 WHERE id=$1`, pooled, tenant.ID); !errors.Is(mapDBError("owner", err), ErrInvalid) {
		t.Fatalf("pooled platform account claimed: %v", err)
	}
	// An unconfigured platform row may still be claimed once.
	claimable := account("")
	if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET owner_user_id=$2 WHERE id=$1`, claimable, tenant.ID); err != nil {
		t.Fatalf("claim unconfigured account: %v", err)
	}

	// Settings validation and audit.
	current, err := s.MarketplaceSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rate := range []string{"1.000001", "-0.1", "0.1234567", "abc", ""} {
		if _, err := s.SetMarketplaceSettings(ctx, SetMarketplaceSettingsParams{CommissionRate: rate, ExpectedRevision: current.Revision, ActorUserID: actor.ID}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("rate %q err=%v", rate, err)
		}
	}
	if _, err := s.SetMarketplaceSettings(ctx, SetMarketplaceSettingsParams{CommissionRate: "0", ExpectedRevision: current.Revision - 1, ActorUserID: actor.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision err=%v", err)
	}
	var audits int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events
		WHERE event_type='gateway.marketplace_settings_changed' AND actor_user_id=$1::uuid`, actor.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audits=%d err=%v", audits, err)
	}
}
