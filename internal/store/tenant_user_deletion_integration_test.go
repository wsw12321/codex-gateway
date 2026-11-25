//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTenantUserDeletionPostgresIntegration covers stage 5 of the
// aggregation gateway: permanently deleting a member who once held tenant
// resources. Live resources block with a named reason, released accounts lose
// their owner without becoming platform accounts, and a seller's deleted
// plans go with the seller once nothing names them.
func TestTenantUserDeletionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	billingIntegrationModelScopes(t, ctx, s, "billing-priced-model")
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := now.AddDate(0, 0, -120)
	suffix := fmt.Sprint(now.UnixNano())
	owner := globalUsageIntegrationUser(t, ctx, s, "tenant-delete-owner-"+suffix, UserRoleOwner)
	if _, err := s.db.ExecContext(ctx, `UPDATE marketplace_settings SET enabled=true, commission_rate=0.1 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	member := func(name string) User {
		return globalUsageIntegrationUser(t, ctx, s, "tenant-delete-"+name+"-"+suffix, UserRoleMember)
	}
	deletion := func(ids ...string) DeleteInformationUsersParams {
		return DeleteInformationUsersParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "delete tenant", now), UserIDs: ids}
	}
	account := func(holder User, released bool) string {
		t.Helper()
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		id := hex.EncodeToString(raw)
		var removed any
		if released {
			removed = old
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_accounts(id,provider,status,access_mode,owner_user_id,credential_kind,credential_removed_at)
			VALUES($1,'codex','available','shared',$2,'oauth',$3)`, id, holder.ID, removed); err != nil {
			t.Fatal(err)
		}
		return id
	}
	reasons := func(err error, userID string) []string {
		t.Helper()
		var blocked *UserDeletionBlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("deletion error = %v, want blockers", err)
		}
		for _, blocker := range blocked.Blockers {
			if blocker.UserID == userID {
				result := append([]string(nil), blocker.Reasons...)
				sort.Strings(result)
				return result
			}
		}
		t.Fatalf("no blocker for %s in %+v", userID, blocked.Blockers)
		return nil
	}
	listed := func(u User) bool {
		t.Helper()
		users, err := s.ListDeletableInformationUsers(ctx, u.Username, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(users) == 1 && users[0].ID == u.ID
	}
	cleanup := func() {
		t.Helper()
		cutoff, err := InformationCutoff(now, 90)
		if err != nil {
			t.Fatal(err)
		}
		write := billingIntegrationWrite(t, owner.ID, "cleanup", now)
		job, err := s.CreateInformationCleanupJob(ctx, InformationCleanupParams{OperationID: write.OperationID, ActorUserID: owner.ID, RetentionDays: 90, Cutoff: cutoff})
		if err != nil {
			t.Fatal(err)
		}
		for batch := 0; batch < 100 && job.Status != "completed"; batch++ {
			progress, err := s.RunInformationCleanupBatch(ctx, 50)
			if err != nil || progress == nil {
				t.Fatalf("cleanup batch: %+v %v", progress, err)
			}
			job = *progress
		}
		if job.Status != "completed" {
			t.Fatal("cleanup did not finish")
		}
	}

	t.Run("live tenant resources block with named reasons", func(t *testing.T) {
		seller := member("live")
		live := account(seller, false)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO egress_servers(id,name,server,port,cipher,password_ciphertext,revision,created_at,updated_at,owner_user_id)
			VALUES($1,'member node','203.0.113.7',8388,'aes-256-gcm',$2,1,$3,$3,$4)`, uuid.NewString(), make([]byte, 40), now, seller.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "publish", now),
			Name: "Live " + suffix, PriceUSD: "1", Tier: BillingTierDay, AllowanceUSD: "1",
			ModelIDs: []string{"billing-priced-model"}, MinPeriodCount: 1, Active: true}); err != nil {
			t.Fatal(err)
		}
		if listed(seller) {
			t.Fatal("a member with tenant resources is a deletion candidate")
		}
		got := reasons(func() error { _, err := s.DeleteInformationUsers(ctx, deletion(seller.ID)); return err }(), seller.ID)
		for _, want := range []string{"providers", "egress_servers", "seller_plans"} {
			found := false
			for _, reason := range got {
				found = found || reason == want
			}
			if !found {
				t.Errorf("reasons %v miss %s", got, want)
			}
		}
		for _, reason := range got {
			if reason == "management_history" {
				t.Errorf("a seller's own plan counts as management history: %v", got)
			}
		}
		// The blocked listing gives the same reasons, so the Owner sees why
		// the member is missing from the candidates.
		found := false
		for offset := 0; !found; offset += 100 {
			page, err := s.ListBlockedInformationUsers(ctx, 100, offset)
			if err != nil {
				t.Fatal(err)
			}
			for _, user := range page {
				if user.ID == seller.ID {
					found = true
					listed := append([]string(nil), user.Reasons...)
					sort.Strings(listed)
					if strings.Join(listed, ",") != strings.Join(got, ",") {
						t.Errorf("blocked listing reasons %v, deletion %v", listed, got)
					}
				}
			}
			if len(page) < 100 {
				break
			}
		}
		if !found {
			t.Error("blocked listing misses the member")
		}
		if _, err := s.ListBlockedInformationUsers(ctx, 0, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid page = %v", err)
		}
		var still string
		if err := s.db.QueryRowContext(ctx, `SELECT owner_user_id::text FROM upstream_accounts WHERE id=$1`, live).Scan(&still); err != nil || still != seller.ID {
			t.Fatalf("refused deletion changed the live account: %q %v", still, err)
		}
	})

	t.Run("released accounts lose their owner and belong to no scope", func(t *testing.T) {
		former, claimer := member("former"), member("claimer")
		released := account(former, true)
		if !listed(former) {
			t.Fatal("a member with only released accounts is not a deletion candidate")
		}
		result, err := s.DeleteInformationUsers(ctx, deletion(former.ID))
		if err != nil || result.DeletedCount != 1 {
			t.Fatalf("delete: %+v %v", result, err)
		}
		var ownerID sql.NullString
		var deletedAt sql.NullTime
		if err := s.db.QueryRowContext(ctx, `SELECT owner_user_id::text, owner_deleted_at FROM upstream_accounts WHERE id=$1`, released).Scan(&ownerID, &deletedAt); err != nil {
			t.Fatal(err)
		}
		if ownerID.Valid || !deletedAt.Valid {
			t.Fatalf("released account owner=%v deleted_at=%v", ownerID, deletedAt)
		}
		platform, err := s.WithUpstreamProvider(UpstreamProviderCodex).ListUpstreamAccounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range platform {
			if item.ID == released {
				t.Fatal("an account of a deleted member appears among platform accounts")
			}
		}
		ownership, err := s.UpstreamAccountOwnership(ctx, released)
		if err != nil || !ownership.OwnerDeleted || ownership.InScope("") || ownership.InScope(claimer.ID) {
			t.Fatalf("ownership = %+v %v", ownership, err)
		}
		var audited bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_events WHERE event_type='information.users.deleted'
			AND metadata->'released_account_ids' ? $1)`, released).Scan(&audited); err != nil || !audited {
			t.Fatalf("deletion audit misses the released account: %v", err)
		}
		// The trigger accepts the owner_deleted change only for released accounts.
		live := account(claimer, false)
		err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('gateway.upstream_owner_change','owner_deleted',true)`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE upstream_accounts SET owner_user_id=NULL WHERE id=$1`, live)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "released") {
			t.Fatalf("owner_deleted change of a live account: %v", err)
		}
		// The next login of the same upstream account claims it.
		err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
			return claimRegistrationAccountTx(ctx, tx, EgressRegistration{TenantUserID: claimer.ID, Provider: UpstreamProviderCodex, CreatedAt: now}, released)
		})
		if err != nil {
			t.Fatalf("claim of an orphaned account: %v", err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT owner_user_id::text, owner_deleted_at FROM upstream_accounts WHERE id=$1 AND credential_removed_at IS NULL`, released).Scan(&ownerID, &deletedAt); err != nil {
			t.Fatal(err)
		}
		if ownerID.String != claimer.ID || deletedAt.Valid {
			t.Fatalf("claimed account owner=%v deleted_at=%v", ownerID, deletedAt)
		}
	})

	t.Run("a seller's deleted plans go once nothing names them", func(t *testing.T) {
		seller := member("retired")
		plan, err := s.CreateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "publish", old),
			Name: "Retired " + suffix, PriceUSD: "1", Tier: BillingTierDay, AllowanceUSD: "1",
			ModelIDs: []string{"billing-priced-model"}, MinPeriodCount: 1, Active: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteBillingPlan(ctx, DeleteBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "retire", old),
			PlanID: plan.ID, Version: plan.Version, SellerUserID: seller.ID}); err != nil {
			t.Fatal(err)
		}
		if listed(seller) {
			t.Fatal("uncleaned plan history must block deletion")
		}
		cleanup()
		if !listed(seller) {
			_, err := s.DeleteInformationUsers(ctx, deletion(seller.ID))
			t.Fatalf("seller with a deleted, unreferenced plan is not a candidate: %v", err)
		}
		if _, err := s.DeleteInformationUsers(ctx, deletion(seller.ID)); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_plans WHERE id=$1)+(SELECT count(*) FROM billing_plan_provider_scopes WHERE plan_id=$1)`, plan.ID).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("deleted seller plan survived: %d %v", remaining, err)
		}
	})

	t.Run("old sales go with their ledger entries and free the seller", func(t *testing.T) {
		seller, buyer := member("old-seller"), member("old-buyer")
		account(seller, true)
		plan, err := s.CreateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "publish", old),
			Name: "Old " + suffix, PriceUSD: "2", Tier: BillingTierDay, AllowanceUSD: "1",
			ModelIDs: []string{"billing-priced-model"}, MinPeriodCount: 1, Active: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan, err = s.SubmitSellerPlan(ctx, PlanReviewWriteParams{PlanID: plan.ID, Version: plan.Version, ActorUserID: seller.ID, At: old}); err != nil {
			t.Fatal(err)
		}
		if plan, err = s.ReviewSellerPlan(ctx, ReviewSellerPlanParams{PlanReviewWriteParams: PlanReviewWriteParams{
			PlanID: plan.ID, Version: plan.Version, ActorUserID: owner.ID, At: old}, Approve: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "credit", old), UserID: buyer.ID, USDAmount: "2"}); err != nil {
			t.Fatal(err)
		}
		// The seller's account must hold a credential while the plan is sold.
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=NULL WHERE owner_user_id=$1`, seller.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PurchaseBillingPlan(ctx, PurchaseBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, buyer.ID, "buy", old),
			UserID: buyer.ID, PlanID: plan.ID, PlanVersion: plan.Version, PeriodCount: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=$2 WHERE owner_user_id=$1`, seller.ID, old); err != nil {
			t.Fatal(err)
		}
		sales := func() int {
			t.Helper()
			var count int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM marketplace_sales WHERE plan_id=$1`, plan.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			return count
		}
		if sales() != 1 {
			t.Fatal("sale not recorded")
		}
		// The buyer's subscription is still enabled and the seller's income
		// is unspent: both ledger entries, and so the sale, stay.
		cleanup()
		if sales() != 1 {
			t.Fatal("a sale went while its ledger entries are retained")
		}
		if _, err := s.DeleteBillingPlan(ctx, DeleteBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "retire", old),
			PlanID: plan.ID, Version: plan.Version, SellerUserID: seller.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "spend income", old), UserID: seller.ID, USDAmount: "-1.8"}); err != nil {
			t.Fatal(err)
		}
		if err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM billing_accounts WHERE user_id=$1 FOR UPDATE`, buyer.ID); err != nil {
				return err
			}
			_, err := rollBillingSubscriptionsTx(ctx, tx, buyer.ID, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var enabled bool
		if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM billing_subscriptions WHERE user_id=$1`, buyer.ID).Scan(&enabled); err != nil || enabled {
			t.Fatalf("expired subscription still enabled: %v", err)
		}
		cleanup()
		if sales() != 0 {
			t.Fatal("a sale outlived both of its ledger entries")
		}
		var reported int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sum((report->'delete_counts'->>'marketplace_sales')::bigint),0) FROM information_cleanup_jobs`).Scan(&reported); err != nil || reported < 1 {
			t.Fatalf("cleanup report misses sales: %d %v", reported, err)
		}
		_, err = s.DeleteInformationUsers(ctx, deletion(seller.ID, buyer.ID))
		var blocked *UserDeletionBlockedError
		if err != nil && !errors.As(err, &blocked) {
			t.Fatal(err)
		}
		if blocked != nil {
			for _, blocker := range blocked.Blockers {
				for _, reason := range blocker.Reasons {
					if reason == "marketplace_sales" || blocker.UserID == seller.ID {
						t.Fatalf("cleaned sale still blocks: %+v", blocked.Blockers)
					}
				}
			}
			if _, err := s.DeleteInformationUsers(ctx, deletion(seller.ID)); err != nil {
				t.Fatalf("seller deletion after cleanup: %v", err)
			}
		}
		if _, err := s.GetUser(ctx, seller.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("seller survived: %v", err)
		}
	})

	t.Run("sales and buyers' subscriptions block both sides", func(t *testing.T) {
		seller, buyer := member("seller"), member("buyer")
		account(seller, false)
		plan, err := s.CreateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "publish", now),
			Name: "Sold " + suffix, PriceUSD: "2", Tier: BillingTierDay, AllowanceUSD: "1",
			ModelIDs: []string{"billing-priced-model"}, MinPeriodCount: 1, Active: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan, err = s.SubmitSellerPlan(ctx, PlanReviewWriteParams{PlanID: plan.ID, Version: plan.Version, ActorUserID: seller.ID, At: now}); err != nil {
			t.Fatal(err)
		}
		if plan, err = s.ReviewSellerPlan(ctx, ReviewSellerPlanParams{PlanReviewWriteParams: PlanReviewWriteParams{
			PlanID: plan.ID, Version: plan.Version, ActorUserID: owner.ID, At: now}, Approve: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "credit", now), UserID: buyer.ID, USDAmount: "5"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PurchaseBillingPlan(ctx, PurchaseBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, buyer.ID, "buy", now),
			UserID: buyer.ID, PlanID: plan.ID, PlanVersion: plan.Version, PeriodCount: 1}); err != nil {
			t.Fatal(err)
		}
		_, err = s.DeleteInformationUsers(ctx, deletion(seller.ID, buyer.ID))
		sellerReasons, buyerReasons := strings.Join(reasons(err, seller.ID), ","), strings.Join(reasons(err, buyer.ID), ",")
		for _, want := range []string{"plan_subscriptions", "marketplace_sales", "seller_plans"} {
			if !strings.Contains(sellerReasons, want) {
				t.Errorf("seller reasons %s miss %s", sellerReasons, want)
			}
		}
		for _, want := range []string{"subscriptions", "marketplace_sales"} {
			if !strings.Contains(buyerReasons, want) {
				t.Errorf("buyer reasons %s miss %s", buyerReasons, want)
			}
		}
		if strings.Contains(buyerReasons, "plan_subscriptions") || strings.Contains(buyerReasons, "seller_plans") {
			t.Errorf("buyer carries seller reasons: %s", buyerReasons)
		}
	})
}
