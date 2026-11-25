//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTenantProviderLifecyclePostgresIntegration covers stage 2: logins claim
// their account for the tenant that started them, conflicting logins are
// refused, deletion releases an account for the next login, only the Owner
// transfers, and tenant egress nodes stay with their owner.
func TestTenantProviderLifecyclePostgresIntegration(t *testing.T) {
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
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	owner := usageSummaryUser(t, ctx, s, "lifecycle-owner-"+suffix)
	alice := usageSummaryUser(t, ctx, s, "lifecycle-alice-"+suffix)
	bob := usageSummaryUser(t, ctx, s, "lifecycle-bob-"+suffix)
	member := usageSummaryUser(t, ctx, s, "lifecycle-member-"+suffix)
	ownerActor := EgressActor{UserID: owner.ID, SourceIP: "192.0.2.10"}
	aliceActor := EgressActor{UserID: alice.ID}
	bobActor := EgressActor{UserID: bob.ID}
	platformStore := s
	aliceStore := s.WithUpstreamOwner(alice.ID)
	bobStore := s.WithUpstreamOwner(bob.ID)

	settings, err := s.MarketplaceSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		current, err := s.MarketplaceSettings(context.Background())
		if err != nil {
			t.Errorf("read settings for restore: %v", err)
			return
		}
		if _, err := s.SetMarketplaceSettings(context.Background(), SetMarketplaceSettingsParams{
			Enabled: settings.Enabled, CommissionRate: settings.CommissionRate,
			MaxAccountsPerUser: settings.MaxAccountsPerUser, MaxPlansPerUser: settings.MaxPlansPerUser,
			AllowMemberSuppliers: settings.AllowMemberSuppliers, MaxEgressServersPerUser: settings.MaxEgressServersPerUser,
			ExpectedRevision: current.Revision, ActorUserID: owner.ID,
		}); err != nil {
			t.Errorf("restore settings: %v", err)
		}
	})
	setMarketplace := func(enabled bool) {
		t.Helper()
		current, err := s.MarketplaceSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next, err := s.SetMarketplaceSettings(ctx, SetMarketplaceSettingsParams{
			Enabled: enabled, CommissionRate: "0", MaxAccountsPerUser: 2, MaxPlansPerUser: 1,
			MaxEgressServersPerUser: 1, ExpectedRevision: current.Revision, ActorUserID: owner.ID,
		})
		if err != nil || next.Enabled != enabled || next.MaxEgressServersPerUser != 1 {
			t.Fatalf("set marketplace %v: %+v %v", enabled, next, err)
		}
	}
	setMarketplace(true)

	accountID := func(name string) string { return upstreamIntegrationAccountID(name + "-" + suffix) }
	login := func(scope *Store, actor EgressActor, node *string, id string) error {
		t.Helper()
		registration, err := scope.CreateEgressRegistration(ctx, UpstreamProviderCodex, node, actor)
		if err != nil {
			return err
		}
		if registration.TenantUserID != scope.UpstreamOwner() {
			t.Fatalf("registration tenant=%q, want %q", registration.TenantUserID, scope.UpstreamOwner())
		}
		if _, err := s.PrepareEgressRegistration(ctx, registration.ID, UpstreamProviderCodex, id); err != nil {
			return err
		}
		_, err = s.CommitEgressRegistration(ctx, registration.ID, UpstreamProviderCodex, id)
		return err
	}
	ownership := func(id string) UpstreamAccountOwnership {
		t.Helper()
		value, err := s.UpstreamAccountOwnership(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	// A tenant login creates the account for the tenant.
	shared := accountID("shared")
	if err := login(aliceStore, aliceActor, nil, shared); err != nil {
		t.Fatalf("alice login: %v", err)
	}
	if got := ownership(shared); !got.Exists || got.OwnerUserID != alice.ID || got.CredentialRemoved {
		t.Fatalf("shared ownership=%+v", got)
	}
	// Re-authorizing her own account is fine; anyone else is refused.
	if err := login(aliceStore, aliceActor, nil, shared); err != nil {
		t.Fatalf("alice re-login: %v", err)
	}
	if err := login(bobStore, bobActor, nil, shared); !errors.Is(err, ErrAccountOwnedElsewhere) {
		t.Fatalf("bob login of alice's account err=%v", err)
	}
	if err := login(platformStore, ownerActor, nil, shared); !errors.Is(err, ErrAccountOwnedElsewhere) {
		t.Fatalf("owner login of alice's account err=%v", err)
	}
	platformAccount := accountID("platform")
	if err := login(platformStore, ownerActor, nil, platformAccount); err != nil {
		t.Fatalf("owner login: %v", err)
	}
	if got := ownership(platformAccount); got.OwnerUserID != "" {
		t.Fatalf("platform ownership=%+v", got)
	}
	if err := login(aliceStore, aliceActor, nil, platformAccount); !errors.Is(err, ErrAccountOwnedElsewhere) {
		t.Fatalf("alice login of a platform account err=%v", err)
	}

	// The account limit counts accounts with a credential.
	second := accountID("second")
	if err := login(aliceStore, aliceActor, nil, second); err != nil {
		t.Fatalf("alice second login: %v", err)
	}
	if err := login(aliceStore, aliceActor, nil, accountID("third")); !errors.Is(err, ErrProviderLimitReached) {
		t.Fatalf("alice over limit err=%v", err)
	}

	// Owner-scoped management.
	if _, err := aliceStore.SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{AccountID: shared, Alias: "mine", ActorUserID: alice.ID}); err != nil {
		t.Fatalf("alice alias: %v", err)
	}
	if _, err := bobStore.SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{AccountID: shared, Alias: "theirs", ActorUserID: bob.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob alias of alice's account err=%v", err)
	}
	if _, err := platformStore.SetUpstreamAccountConcurrentLimit(ctx, SetUpstreamAccountConcurrentLimitParams{AccountID: shared, Limit: 2, ActorUserID: owner.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner limit on alice's account err=%v", err)
	}
	if _, err := aliceStore.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{AccountID: second, Weight: 3, ActorUserID: alice.ID}); err != nil {
		t.Fatalf("alice weight: %v", err)
	}
	now := time.Now().UTC()
	summaryFilter := UpstreamAccountSummaryFilter{From: now.Add(-time.Hour), Until: now.Add(time.Minute)}
	summaryIDs := func(scope *Store) []string {
		t.Helper()
		rows, err := scope.SummarizeUpstreamAccounts(ctx, summaryFilter)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, row := range rows {
			if row.AccountID != nil {
				ids = append(ids, *row.AccountID)
			}
		}
		return ids
	}
	if ids := summaryIDs(aliceStore); !slices.Contains(ids, shared) || !slices.Contains(ids, second) || slices.Contains(ids, platformAccount) {
		t.Fatalf("alice summary=%v", ids)
	}
	if ids := summaryIDs(platformStore); slices.Contains(ids, shared) || !slices.Contains(ids, platformAccount) {
		t.Fatalf("platform summary=%v", ids)
	}

	// Deleting releases the account: it leaves every path, and Bob's later
	// login of the same upstream account claims it.
	if err := bobStore.ReleaseUpstreamAccount(ctx, shared, bobActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob released alice's account err=%v", err)
	}
	if err := aliceStore.ReleaseUpstreamAccount(ctx, shared, aliceActor); err != nil {
		t.Fatalf("alice release: %v", err)
	}
	if got := ownership(shared); !got.CredentialRemoved || got.InScope(alice.ID) {
		t.Fatalf("released ownership=%+v", got)
	}
	if ids := summaryIDs(aliceStore); slices.Contains(ids, shared) {
		t.Fatalf("released account still listed for alice: %v", ids)
	}
	if err := login(bobStore, bobActor, nil, shared); err != nil {
		t.Fatalf("bob claims a released account: %v", err)
	}
	if got := ownership(shared); got.OwnerUserID != bob.ID || got.CredentialRemoved {
		t.Fatalf("claimed ownership=%+v", got)
	}
	var claims int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE event_type='upstream_account.owner_changed'
		AND subject_id=$1 AND metadata->>'reason'='claim'`, shared).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claim audits=%d err=%v", claims, err)
	}
	// A released platform account is not eligible for platform funding.
	if err := platformStore.ReleaseUpstreamAccount(ctx, platformAccount, ownerActor); err != nil {
		t.Fatalf("owner release: %v", err)
	}
	if eligible, err := s.EligibleUpstreamAccounts(ctx, member.ID, []string{platformAccount}); err != nil || len(eligible) != 0 {
		t.Fatalf("released platform account eligible=%v err=%v", eligible, err)
	}
	if err := login(platformStore, ownerActor, nil, platformAccount); err != nil {
		t.Fatalf("owner re-login of a released account: %v", err)
	}
	if eligible, err := s.EligibleUpstreamAccounts(ctx, member.ID, []string{platformAccount}); err != nil || len(eligible) != 1 {
		t.Fatalf("restored platform account eligible=%v err=%v", eligible, err)
	}

	// Synchronization does not create rows while the marketplace is on.
	unseen := accountID("unseen")
	snapshot := []UpstreamAccountSnapshot{{ID: unseen, MaskedEmail: "u***@example.com", Status: UpstreamAccountStatusAvailable}}
	if err := s.SyncUpstreamAccounts(ctx, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ownership(unseen).Exists {
		t.Fatal("synchronization created an account row while the marketplace is enabled")
	}
	// A login that started before a synchronization created a platform row
	// (marketplace briefly off) still claims that row.
	registration, err := bobStore.CreateEgressRegistration(ctx, UpstreamProviderCodex, nil, bobActor)
	if err != nil {
		t.Fatal(err)
	}
	setMarketplace(false)
	if err := s.SyncUpstreamAccounts(ctx, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := ownership(unseen); !got.Exists || got.OwnerUserID != "" {
		t.Fatalf("synchronized row=%+v", got)
	}
	setMarketplace(true)
	if _, err := s.PrepareEgressRegistration(ctx, registration.ID, UpstreamProviderCodex, unseen); err != nil {
		t.Fatalf("claim synchronized row: %v", err)
	}
	if got := ownership(unseen); got.OwnerUserID != bob.ID {
		t.Fatalf("race-claimed ownership=%+v", got)
	}

	// Owner transfers.
	pooled := accountID("pooled")
	if err := login(platformStore, ownerActor, nil, pooled); err != nil {
		t.Fatal(err)
	}
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "lifecycle", now), Name: "lifecycle " + suffix, LimitUSD: "1", Period: "week"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "lifecycle", now),
		GroupID: group.ID, Provider: UpstreamProviderCodex, Strategy: "selected", AccountIDs: []string{pooled}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransferUpstreamAccountOwner(ctx, TransferUpstreamAccountParams{AccountID: pooled, NewOwnerUserID: alice.ID, Actor: ownerActor}); !errors.Is(err, ErrAccountInPlatformUse) {
		t.Fatalf("transfer of a pooled account err=%v", err)
	}
	if _, err := s.TransferUpstreamAccountOwner(ctx, TransferUpstreamAccountParams{AccountID: second, NewOwnerUserID: bob.ID, ExpectedOwnerUserID: bob.ID, Actor: ownerActor}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale transfer err=%v", err)
	}
	if _, err := s.TransferUpstreamAccountOwner(ctx, TransferUpstreamAccountParams{AccountID: second, NewOwnerUserID: uuid.NewString(), ExpectedOwnerUserID: alice.ID, Actor: ownerActor}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transfer to nobody err=%v", err)
	}

	// Tenant egress nodes.
	password := make([]byte, 40)
	if _, err := rand.Read(password); err != nil {
		t.Fatal(err)
	}
	node := func(server string) EgressServer {
		return EgressServer{ID: uuid.NewString(), Name: "node " + server, Protocol: "ss", Server: server, Port: 8388, Cipher: "aes-256-gcm", PasswordCiphertext: password}
	}
	for _, server := range []string{"proxy.example.com", "10.0.0.1", "172.17.0.1", "127.0.0.1", "100.64.0.1"} {
		if _, err := aliceStore.SaveEgressServer(ctx, node(server), true, aliceActor); !errors.Is(err, ErrInvalid) {
			t.Fatalf("tenant node %s err=%v", server, err)
		}
	}
	aliceNode, err := aliceStore.SaveEgressServer(ctx, node("8.8.8.8"), true, aliceActor)
	if err != nil || aliceNode.OwnerUserID != alice.ID {
		t.Fatalf("alice node=%+v err=%v", aliceNode, err)
	}
	if _, err := aliceStore.SaveEgressServer(ctx, node("8.8.4.4"), true, aliceActor); !errors.Is(err, ErrProviderLimitReached) {
		t.Fatalf("alice second node err=%v", err)
	}
	if _, err := bobStore.EgressServer(ctx, aliceNode.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob read alice's node err=%v", err)
	}
	if _, err := platformStore.EgressServer(ctx, aliceNode.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("platform read alice's node err=%v", err)
	}
	if err := bobStore.DeleteEgressServer(ctx, aliceNode.ID, bobActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deleted alice's node err=%v", err)
	}
	updated := aliceNode
	updated.Name = "renamed"
	updated.PasswordCiphertext = nil
	if _, err := bobStore.SaveEgressServer(ctx, updated, false, bobActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob edited alice's node err=%v", err)
	}
	if _, err := aliceStore.SetEgressBinding(ctx, second, aliceNode.ID, aliceActor); err != nil {
		t.Fatalf("alice binds her node: %v", err)
	}
	if _, err := platformStore.SetEgressBinding(ctx, second, "", ownerActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner binding of a tenant account err=%v", err)
	}
	if _, err := bobStore.SetEgressBinding(ctx, unseen, aliceNode.ID, bobActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob bound alice's node err=%v", err)
	}
	if _, err := bobStore.CreateEgressRegistration(ctx, UpstreamProviderCodex, &aliceNode.ID, bobActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob login through alice's node err=%v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_account_egress(account_id,node_id,revision) VALUES($1,$2,1)
		ON CONFLICT(account_id) DO UPDATE SET node_id=EXCLUDED.node_id`, platformAccount, aliceNode.ID); !errors.Is(mapDBError("bind", err), ErrInvalid) {
		t.Fatalf("trigger accepted a foreign node binding: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE egress_servers SET owner_user_id=$2 WHERE id=$1`, aliceNode.ID, bob.ID); !errors.Is(mapDBError("node", err), ErrInvalid) {
		t.Fatalf("node owner changed: %v", err)
	}
	// Transferring Alice's bound account to Bob drops her node.
	binding, err := s.TransferUpstreamAccountOwner(ctx, TransferUpstreamAccountParams{AccountID: second, NewOwnerUserID: bob.ID, ExpectedOwnerUserID: alice.ID, Actor: ownerActor})
	if err != nil || binding == nil || binding.NodeID != "" || binding.AccountID != second {
		t.Fatalf("transfer binding=%+v err=%v", binding, err)
	}
	if got := ownership(second); got.OwnerUserID != bob.ID {
		t.Fatalf("transferred ownership=%+v", got)
	}
	if err := aliceStore.DeleteEgressServer(ctx, aliceNode.ID, aliceActor); err != nil {
		t.Fatalf("alice deletes her unbound node: %v", err)
	}
	// Transfer back to the platform.
	if _, err := s.TransferUpstreamAccountOwner(ctx, TransferUpstreamAccountParams{AccountID: second, ExpectedOwnerUserID: bob.ID, Actor: ownerActor}); err != nil {
		t.Fatalf("transfer to platform: %v", err)
	}
	if got := ownership(second); got.OwnerUserID != "" {
		t.Fatalf("platform transfer ownership=%+v", got)
	}

	// Suspension applies to tenant accounts only.
	suspended, err := s.SetUpstreamAccountSuspended(ctx, SetUpstreamAccountSuspendedParams{AccountID: shared, Suspended: true, Actor: ownerActor})
	if err != nil || !suspended.AdminSuspended || suspended.OwnerUsername != bob.Username {
		t.Fatalf("suspend=%+v err=%v", suspended, err)
	}
	if _, err := s.SetUpstreamAccountSuspended(ctx, SetUpstreamAccountSuspendedParams{AccountID: platformAccount, Suspended: true, Actor: ownerActor}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("suspend platform account err=%v", err)
	}
	listed, err := s.ListTenantUpstreamAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listedIDs []string
	for _, item := range listed {
		listedIDs = append(listedIDs, item.ID)
	}
	if !slices.Contains(listedIDs, shared) || !slices.Contains(listedIDs, unseen) || slices.Contains(listedIDs, platformAccount) || slices.Contains(listedIDs, second) {
		t.Fatalf("tenant accounts=%v", listedIDs)
	}

	// Tenant suppliers count towards the limit and are released on deletion.
	supplier := ModelSupplier{ID: accountID("supplier"), BaseURL: "https://api.example.com/v1", Ciphertext: password, Models: []string{"gpt-5.4"}}
	if err := bobStore.CreateModelSupplier(ctx, supplier, "", bobActor); !errors.Is(err, ErrProviderLimitReached) {
		t.Fatalf("bob supplier over limit err=%v", err)
	}
	if err := aliceStore.CreateModelSupplier(ctx, supplier, "", aliceActor); err != nil {
		t.Fatalf("alice supplier: %v", err)
	}
	if values, err := aliceStore.ModelSuppliers(ctx); err != nil || len(values) != 1 || values[0].ID != supplier.ID {
		t.Fatalf("alice suppliers=%+v err=%v", values, err)
	}
	if values, err := platformStore.ModelSuppliers(ctx); err != nil || slices.ContainsFunc(values, func(v ModelSupplier) bool { return v.ID == supplier.ID }) {
		t.Fatalf("platform sees tenant supplier: %+v err=%v", values, err)
	}
	if err := aliceStore.SetModelSupplierEnabled(ctx, supplier.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := bobStore.DeleteModelSupplier(ctx, supplier.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deleted alice's supplier err=%v", err)
	}
	if err := aliceStore.DeleteModelSupplier(ctx, supplier.ID); err != nil {
		t.Fatal(err)
	}
	if got := ownership(supplier.ID); !got.CredentialRemoved || got.OwnerUserID != alice.ID {
		t.Fatalf("deleted supplier ownership=%+v", got)
	}
}
