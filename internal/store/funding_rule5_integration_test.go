//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

// TestFundingRule5PostgresIntegration covers stage 3 of the aggregation
// gateway: self-use, platform plan provider scopes, seller plan layers, the
// per-layer eligibility checks and rule 5 settlement.
func TestFundingRule5PostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 20, MaxIdleConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	billingIntegrationModelScopes(t, ctx, s, "billing-priced-model")
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	suffix := fmt.Sprint(now.UnixNano())
	codex := s.WithUpstreamProvider(UpstreamProviderCodex)

	var previous bool
	if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM marketplace_settings WHERE singleton`).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), `UPDATE marketplace_settings SET enabled=$1 WHERE singleton`, previous)
	})
	marketplace := func(enabled bool) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE marketplace_settings SET enabled=$1 WHERE singleton`, enabled); err != nil {
			t.Fatal(err)
		}
	}
	actor := globalUsageIntegrationUser(t, ctx, s, "rule5-actor-"+suffix, UserRoleMember)
	write := func() BillingWriteParams {
		return billingIntegrationWrite(t, actor.ID, "funding rule 5 integration", now)
	}
	account := func(owner *User) string {
		t.Helper()
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		id := hex.EncodeToString(raw)
		var ownerID any
		if owner != nil {
			ownerID = owner.ID
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_accounts(id,provider,status,access_mode,owner_user_id)
			VALUES($1,'codex','available','shared',$2)`, id, ownerID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	credit := func(u User, amount string) {
		t.Helper()
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: u.ID, USDAmount: amount}); err != nil {
			t.Fatal(err)
		}
	}
	balance := func(u User) string {
		t.Helper()
		var value string
		if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, u.ID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	admit := func(u User, d Device, k APIKey, id string) (RequestAdmission, error) {
		t.Helper()
		return s.AdmitRequest(ctx, billingIntegrationAdmission(u, d, k, id, now))
	}
	mustAdmit := func(u User, d Device, k APIKey, id string) []UpstreamAllocationGroup {
		t.Helper()
		admission, err := admit(u, d, k, id)
		if err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
		return admission.Billing.AllocationGroups
	}
	complete := func(id, accountID string, dollars int64) {
		t.Helper()
		if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: id, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second),
			InputTokens: dollars * 1_000_000, ActualModel: "billing-priced-model", UpstreamAccountID: accountID,
		}); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}
	settle := func(id string) BillingReservation {
		t.Helper()
		if err := s.SettleRequest(ctx, id, now.Add(2*time.Second)); err != nil {
			t.Fatalf("settle %s: %v", id, err)
		}
		reservation, err := s.SettleBilling(ctx, id, now.Add(3*time.Second))
		if err != nil {
			t.Fatalf("replay settlement %s: %v", id, err)
		}
		return reservation
	}
	amounts := func(r BillingReservation) [3]string {
		value := func(p *string) string {
			if p == nil {
				return "<nil>"
			}
			return *p
		}
		return [3]string{value(r.ActualCostUSD), value(r.ChargedUSD), value(r.UncoveredUSD)}
	}
	layer := func(id string, accounts ...string) UpstreamAllocationGroup {
		return UpstreamAllocationGroup{GroupID: id, AccountIDs: accounts}
	}
	personalLayer := UpstreamAllocationGroup{GroupID: PersonalFundingLayerID, AccountIDs: []string{}, AllAccounts: true}
	ids := func(rows []UpstreamAccountEligibility) []string {
		out := []string{}
		for _, row := range rows {
			out = append(out, row.ID)
		}
		return out
	}
	platformPlan := func(name, allowance string) BillingPlan {
		t.Helper()
		plan, err := s.CreateBillingPlan(ctx, CreateBillingPlanParams{BillingWriteParams: write(), Name: name + " " + suffix,
			Tier: BillingTierDay, ModelIDs: []string{"billing-priced-model"}, PriceUSD: "1", AllowanceUSD: allowance, MinPeriodCount: 1, Active: true})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	sellerPlan := func(seller User, name, allowance string) BillingPlan {
		t.Helper()
		plan := platformPlan(name, allowance)
		// Stage 4 publishes seller plans; until then the seller is set here.
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE billing_plans DISABLE TRIGGER billing_plans_seller_immutable`); err != nil {
			t.Fatal(err)
		}
		_, err := s.db.ExecContext(ctx, `UPDATE billing_plans SET seller_user_id=$2 WHERE id=$1`, plan.ID, seller.ID)
		if _, enableErr := s.db.ExecContext(ctx, `ALTER TABLE billing_plans ENABLE TRIGGER billing_plans_seller_immutable`); enableErr != nil {
			t.Fatal(enableErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	buy := func(u User, plan BillingPlan) BillingSubscriptionState {
		t.Helper()
		credit(u, "1")
		result, err := s.PurchaseBillingPlan(ctx, PurchaseBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, u.ID, "buy plan", now),
			UserID: u.ID, PlanID: plan.ID, PlanVersion: plan.Version, PeriodCount: 1})
		if err != nil {
			t.Fatalf("purchase %s: %v", plan.Name, err)
		}
		return result.Subscription
	}
	scope := func(plan BillingPlan, seller string, mode string, accounts ...string) PlanProviderScope {
		t.Helper()
		scopes, err := s.PlanProviderScopes(ctx, seller)
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.SetPlanProviderScope(ctx, SetPlanProviderScopeParams{PlanID: plan.ID, SellerUserID: seller, Mode: mode,
			AccountIDs: accounts, ExpectedRevision: scopes[plan.ID].Revision, ActorUserID: actor.ID, At: now})
		if err != nil {
			t.Fatalf("set scope of %s: %v", plan.Name, err)
		}
		return result
	}

	t.Run("marketplace off keeps rule 4 and ignores own accounts", func(t *testing.T) {
		marketplace(false)
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "rule5-off-"+suffix)
		account(&u)
		if _, err := admit(u, d, k, "rule5-off-unfunded-"+suffix); !errors.As(err, new(*InsufficientFundsError)) {
			t.Fatalf("unfunded admission with own accounts while off = %v", err)
		}
		credit(u, "5")
		id := "rule5-off-" + suffix
		if got := mustAdmit(u, d, k, id); !reflect.DeepEqual(got, []UpstreamAllocationGroup{personalLayer}) {
			t.Fatalf("layers = %+v", got)
		}
		var rule int
		if err := s.db.QueryRowContext(ctx, `SELECT funding_rule_version FROM billing_reservations WHERE request_id=$1`, id).Scan(&rule); err != nil || rule != 4 {
			t.Fatalf("rule = %d, %v", rule, err)
		}
	})

	marketplace(true)

	t.Run("self-use alone admits and settles at zero", func(t *testing.T) {
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "rule5-self-"+suffix)
		own := account(&u)
		stranger := globalUsageIntegrationUser(t, ctx, s, "rule5-stranger-"+suffix, UserRoleMember)
		account(&stranger)
		id := "rule5-self-" + suffix
		got := mustAdmit(u, d, k, id)
		if !reflect.DeepEqual(got, []UpstreamAllocationGroup{{GroupID: SelfFundingLayerID, AccountIDs: []string{own}, Tenant: true}}) {
			t.Fatalf("layers = %+v", got)
		}
		complete(id, own, 3)
		settled := settle(id)
		if settled.FundingRuleVersion != 5 || !settled.SelfUse || amounts(settled) != [3]string{"3.000000000000", "0.000000000000", "0.000000000000"} {
			t.Fatalf("self-use settlement = rule %d self %v %v", settled.FundingRuleVersion, settled.SelfUse, amounts(settled))
		}
		state, err := s.GetBillingState(ctx, u.ID, 10, 0)
		if err != nil || len(state.Ledger) == 0 || !state.Ledger[0].SelfUse {
			t.Fatalf("ledger listing does not mark self-use: %v", err)
		}

		// An own OAuth account that serves requests is checked by the quota
		// guard like a platform account, and a held one leaves the self layer.
		claim, err := s.ClaimUpstreamQuotaCheck(ctx, own, now)
		if err != nil || claim.Provider != UpstreamProviderCodex || !claim.Claimed {
			t.Fatalf("quota guard claim for an own account = %+v, %v", claim, err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_quota_guard_states(account_id,status,source)
			VALUES($1,'limited','manual') ON CONFLICT (account_id) DO UPDATE SET status='limited', source='manual'`, own); err != nil {
			t.Fatal(err)
		}
		if held, err := codex.LayerEligibleAccounts(ctx, u.ID, []UpstreamAllocationGroup{layer(SelfFundingLayerID, own)}); err != nil || len(held[0].Accounts) != 0 {
			t.Fatalf("held own account eligible: %+v, %v", held, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_quota_guard_states SET status='available' WHERE account_id=$1`, own); err != nil {
			t.Fatal(err)
		}

		// Missing attribution never counts as self-use: the cost is left
		// uncovered instead.
		unattributed := "rule5-self-unattributed-" + suffix
		mustAdmit(u, d, k, unattributed)
		complete(unattributed, "", 2)
		if settled := settle(unattributed); settled.SelfUse || amounts(settled) != [3]string{"2.000000000000", "0.000000000000", "2.000000000000"} {
			t.Fatalf("unattributed self-only settlement = %v %v", settled.SelfUse, amounts(settled))
		}

		// Self-use off: the user's own accounts form no layer.
		if _, err := s.SetSelfUse(ctx, SetSelfUseParams{UserID: u.ID, Enabled: false, At: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := admit(u, d, k, "rule5-self-off-"+suffix); !errors.As(err, new(*InsufficientFundsError)) {
			t.Fatalf("admission with self-use off = %v", err)
		}
		eligible, err := codex.LayerEligibleAccounts(ctx, u.ID, []UpstreamAllocationGroup{layer(SelfFundingLayerID, own)})
		if err != nil || len(eligible[0].Accounts) != 0 {
			t.Fatalf("self layer eligibility with self-use off = %+v, %v", eligible, err)
		}
		if _, err := s.SetSelfUse(ctx, SetSelfUseParams{UserID: u.ID, Enabled: true, At: now}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("self-use comes first and personal funds pay for platform accounts", func(t *testing.T) {
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "rule5-mixed-"+suffix)
		own := account(&u)
		platform := account(nil)
		credit(u, "10")
		got := mustAdmit(u, d, k, "rule5-mixed-platform-"+suffix)
		if len(got) != 2 || got[0].GroupID != SelfFundingLayerID || !got[1].AllAccounts {
			t.Fatalf("layers = %+v", got)
		}
		complete("rule5-mixed-platform-"+suffix, platform, 4)
		if settled := settle("rule5-mixed-platform-" + suffix); settled.SelfUse || amounts(settled) != [3]string{"4.000000000000", "4.000000000000", "0.000000000000"} {
			t.Fatalf("platform settlement = %v", amounts(settled))
		}
		mustAdmit(u, d, k, "rule5-mixed-own-"+suffix)
		complete("rule5-mixed-own-"+suffix, own, 4)
		if settled := settle("rule5-mixed-own-" + suffix); !settled.SelfUse {
			t.Fatalf("own account settlement = %v", amounts(settled))
		}
		if got := balance(u); got != "6.000000000000" {
			t.Fatalf("balance = %s", got)
		}

		// The personal layer admits platform accounts only, the self layer
		// only the user's own.
		eligible, err := codex.LayerEligibleAccounts(ctx, u.ID, []UpstreamAllocationGroup{
			layer(SelfFundingLayerID, own, platform), layer(PersonalFundingLayerID, own, platform)})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids(eligible[0].Accounts), []string{own}) || !reflect.DeepEqual(ids(eligible[1].Accounts), []string{platform}) {
			t.Fatalf("eligibility = %+v", eligible)
		}
		if picked, err := codex.SelectLayerUpstreamAccount(ctx, u.ID, layer(SelfFundingLayerID, own, platform), now); err != nil || picked != own {
			t.Fatalf("self layer selection = %q, %v", picked, err)
		}
		if picked, err := codex.SelectLayerUpstreamAccount(ctx, u.ID, layer(PersonalFundingLayerID, own), now); !errors.Is(err, ErrNoUpstreamAccount) {
			t.Fatalf("personal layer selected an own account: %q, %v", picked, err)
		}
		// An Owner suspension takes effect on the next check.
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET admin_suspended=true WHERE id=$1`, own); err != nil {
			t.Fatal(err)
		}
		if eligible, err := codex.LayerEligibleAccounts(ctx, u.ID, []UpstreamAllocationGroup{layer(SelfFundingLayerID, own)}); err != nil || len(eligible[0].Accounts) != 0 {
			t.Fatalf("suspended own account eligible: %+v, %v", eligible, err)
		}
	})

	t.Run("platform plan scope applies from the next request", func(t *testing.T) {
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "rule5-scope-"+suffix)
		x, y, other := account(nil), account(nil), account(nil)
		plan := platformPlan("Scoped", "5")
		subscription := buy(u, plan)
		credit(u, "10")
		// Unrestricted: the subscription is a personal source.
		if got := mustAdmit(u, d, k, "rule5-scope-all-"+suffix); !reflect.DeepEqual(got, []UpstreamAllocationGroup{personalLayer}) {
			t.Fatalf("unrestricted layers = %+v", got)
		}
		saved := scope(plan, "", PlanProviderScopeSelected, x)
		if saved.Mode != PlanProviderScopeSelected || len(saved.Accounts) != 1 || !saved.Accounts[0].Usable {
			t.Fatalf("saved scope = %+v", saved)
		}
		first := "rule5-scope-x-" + suffix
		got := mustAdmit(u, d, k, first)
		if !reflect.DeepEqual(got, []UpstreamAllocationGroup{layer(subscription.ID, x), personalLayer}) {
			t.Fatalf("first layers = %+v", got)
		}
		scope(plan, "", PlanProviderScopeSelected, y)
		second := "rule5-scope-y-" + suffix
		if got := mustAdmit(u, d, k, second); !reflect.DeepEqual(got, []UpstreamAllocationGroup{layer(subscription.ID, y), personalLayer}) {
			t.Fatalf("second layers = %+v", got)
		}
		// The layer check follows the current scope, even for the first
		// request: x is no longer the plan's, so it is only a personal account.
		eligible, err := codex.LayerEligibleAccounts(ctx, u.ID, []UpstreamAllocationGroup{layer(subscription.ID, x, y)})
		if err != nil || !reflect.DeepEqual(ids(eligible[0].Accounts), []string{y}) {
			t.Fatalf("current scope eligibility = %+v, %v", eligible, err)
		}
		// The first request still settles by its own admission snapshot: x
		// served it, so the subscription pays, then personal funds.
		complete(first, x, 7)
		if settled := settle(first); amounts(settled) != [3]string{"7.000000000000", "7.000000000000", "0.000000000000"} {
			t.Fatalf("first settlement = %v", amounts(settled))
		}
		var subscriptionCharged, cashCharged string
		if err := s.db.QueryRowContext(ctx, `SELECT
				COALESCE(sum(amount_usd) FILTER (WHERE source_type='subscription'),0)::text,
				COALESCE(sum(amount_usd) FILTER (WHERE source_type='cash'),0)::text
			FROM billing_charge_allocations WHERE request_id=$1`, first).Scan(&subscriptionCharged, &cashCharged); err != nil {
			t.Fatal(err)
		}
		if subscriptionCharged != "5.000000000000" || cashCharged != "2.000000000000" {
			t.Fatalf("allocations subscription=%s cash=%s", subscriptionCharged, cashCharged)
		}
		// An account outside the scope never draws on the subscription.
		complete(second, other, 1)
		if settled := settle(second); amounts(settled) != [3]string{"1.000000000000", "1.000000000000", "0.000000000000"} {
			t.Fatalf("second settlement = %v", amounts(settled))
		}
		// A stale revision is refused; a member account cannot be listed.
		if _, err := s.SetPlanProviderScope(ctx, SetPlanProviderScopeParams{PlanID: plan.ID, Mode: PlanProviderScopeAll,
			ExpectedRevision: 1, ActorUserID: actor.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale scope revision = %v", err)
		}
		member := globalUsageIntegrationUser(t, ctx, s, "rule5-scope-member-"+suffix, UserRoleMember)
		scopes, _ := s.PlanProviderScopes(ctx, "")
		if _, err := s.SetPlanProviderScope(ctx, SetPlanProviderScopeParams{PlanID: plan.ID, Mode: PlanProviderScopeSelected,
			AccountIDs: []string{account(&member)}, ExpectedRevision: scopes[plan.ID].Revision, ActorUserID: actor.ID}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("member account in platform scope = %v", err)
		}
		// With the marketplace off the scope no longer restricts.
		marketplace(false)
		got = mustAdmit(u, d, k, "rule5-scope-off-"+suffix)
		marketplace(true)
		if !reflect.DeepEqual(got, []UpstreamAllocationGroup{personalLayer}) {
			t.Fatalf("layers with marketplace off = %+v", got)
		}
	})

	t.Run("seller plan pays only for seller accounts and never touches cash", func(t *testing.T) {
		seller := globalUsageIntegrationUser(t, ctx, s, "rule5-seller-"+suffix, UserRoleMember)
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "rule5-buyer-"+suffix)
		first := account(&seller)
		platform := account(nil)
		plan := sellerPlan(seller, "Seller", "3")
		subscription := buy(u, plan)
		credit(u, "10")
		id := "rule5-seller-" + suffix
		got := mustAdmit(u, d, k, id)
		if !reflect.DeepEqual(got, []UpstreamAllocationGroup{{GroupID: subscription.ID, AccountIDs: []string{first}, Tenant: true}, personalLayer}) {
			t.Fatalf("seller layers = %+v", got)
		}
		// Unrestricted seller scope: an account the seller adds joins on the
		// next request.
		second := account(&seller)
		want := []string{first, second}
		if first > second {
			want = []string{second, first}
		}
		next := mustAdmit(u, d, k, "rule5-seller-next-"+suffix)
		if !reflect.DeepEqual(next[0].AccountIDs, want) {
			t.Fatalf("seller pool after a new account = %+v", next[0])
		}
		eligible, err := codex.LayerEligibleAccounts(ctx, u.ID, []UpstreamAllocationGroup{
			layer(subscription.ID, first, platform), layer(PersonalFundingLayerID, first, platform)})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids(eligible[0].Accounts), []string{first}) || !reflect.DeepEqual(ids(eligible[1].Accounts), []string{platform}) {
			t.Fatalf("seller eligibility = %+v", eligible)
		}
		// A selected scope narrows the seller's accounts; a released account
		// drops out of the pool at once.
		scope(plan, seller.ID, PlanProviderScopeSelected, first, second)
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=now() WHERE id=$1`, second); err != nil {
			t.Fatal(err)
		}
		if got := mustAdmit(u, d, k, "rule5-seller-released-"+suffix); !reflect.DeepEqual(got[0].AccountIDs, []string{first}) {
			t.Fatalf("pool after release = %+v", got[0])
		}
		scopes, err := s.PlanProviderScopes(ctx, seller.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, listed := range scopes[plan.ID].Accounts {
			if listed.ID == second && listed.Usable {
				t.Fatal("released account still usable in the scope listing")
			}
		}
		// The subscription covers 3; the rest is the seller's, never cash.
		complete(id, first, 5)
		if settled := settle(id); amounts(settled) != [3]string{"5.000000000000", "3.000000000000", "2.000000000000"} {
			t.Fatalf("seller settlement = %v", amounts(settled))
		}
		if got := balance(u); got != "10.000000000000" {
			t.Fatalf("buyer cash = %s", got)
		}
		buyerScopes, err := s.SubscriptionProviderScopes(ctx, u.ID)
		if err != nil || !buyerScopes[subscription.ID].Restricted || !buyerScopes[subscription.ID].Seller ||
			buyerScopes[subscription.ID].AccountCounts[UpstreamProviderCodex] != 1 {
			t.Fatalf("buyer scope view = %+v, %v", buyerScopes, err)
		}
		// Marketplace off: the seller subscription pays for nothing.
		marketplace(false)
		got = mustAdmit(u, d, k, "rule5-seller-off-"+suffix)
		closed, err := codex.LayerEligibleAccounts(ctx, u.ID, []UpstreamAllocationGroup{layer(subscription.ID, first)})
		marketplace(true)
		if !reflect.DeepEqual(got, []UpstreamAllocationGroup{personalLayer}) {
			t.Fatalf("layers with marketplace off = %+v", got)
		}
		if err != nil || closed[0].Kind != LayerEligibilityClosed || len(closed[0].Accounts) != 0 {
			t.Fatalf("seller layer with marketplace off = %+v, %v", closed, err)
		}
	})

	t.Run("a paid subscription without accounts is reported", func(t *testing.T) {
		seller := globalUsageIntegrationUser(t, ctx, s, "rule5-empty-seller-"+suffix, UserRoleMember)
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "rule5-empty-buyer-"+suffix)
		own := account(&u)
		sold := account(&seller)
		plan := sellerPlan(seller, "Empty", "3")
		subscription := buy(u, plan)
		credit(u, "5")
		admission, err := admit(u, d, k, "rule5-empty-full-"+suffix)
		if err != nil {
			t.Fatal(err)
		}
		if len(admission.Billing.EmptySubscriptionLayers) != 0 ||
			!reflect.DeepEqual(admission.Billing.AllocationLayerKinds, []string{FundingLayerSelf, FundingLayerSubscription, FundingLayerPersonal}) {
			t.Fatalf("served layers: kinds %v empty %+v", admission.Billing.AllocationLayerKinds, admission.Billing.EmptySubscriptionLayers)
		}
		// The seller's only account is released: the buyer still has
		// allowance, but the plan has nothing to serve it.
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=now() WHERE id=$1`, sold); err != nil {
			t.Fatal(err)
		}
		admission, err = admit(u, d, k, "rule5-empty-released-"+suffix)
		if err != nil {
			t.Fatal(err)
		}
		want := []EmptySubscriptionLayer{{SubscriptionID: subscription.ID, PlanID: plan.ID, SellerUserID: seller.ID}}
		if !reflect.DeepEqual(admission.Billing.EmptySubscriptionLayers, want) ||
			!reflect.DeepEqual(admission.Billing.AllocationLayerKinds, []string{FundingLayerSelf, FundingLayerPersonal}) {
			t.Fatalf("empty layers %+v kinds %v", admission.Billing.EmptySubscriptionLayers, admission.Billing.AllocationLayerKinds)
		}
		// With nothing else to fall back on, the refusal names it too.
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=now() WHERE id=$1`, own); err != nil {
			t.Fatal(err)
		}
		credit(u, "-5")
		_, err = admit(u, d, k, "rule5-empty-refused-"+suffix)
		var refused *NoUpstreamAccountError
		if !errors.As(err, &refused) || !reflect.DeepEqual(refused.EmptySubscriptionLayers, want) {
			t.Fatalf("refusal = %v (%+v)", err, refused)
		}
	})

	t.Run("rule 4 reservations in flight settle unchanged", func(t *testing.T) {
		marketplace(false)
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "rule5-inflight-"+suffix)
		credit(u, "5")
		platform := account(nil)
		id := "rule5-inflight-" + suffix
		mustAdmit(u, d, k, id)
		marketplace(true)
		account(&u)
		complete(id, platform, 2)
		settled := settle(id)
		if settled.FundingRuleVersion != 4 || amounts(settled) != [3]string{"2.000000000000", "2.000000000000", "0.000000000000"} {
			t.Fatalf("in-flight rule 4 = %d %v", settled.FundingRuleVersion, amounts(settled))
		}
	})
}
