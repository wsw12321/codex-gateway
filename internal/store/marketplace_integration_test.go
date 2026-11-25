//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestMarketplacePostgresIntegration covers stage 4 of the aggregation
// gateway: seller plans and their review, the marketplace listing and the
// split of every sale between seller and platform.
func TestMarketplacePostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 30, MaxIdleConns: 8})
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

	// marketplace_settings is a shared singleton: restore it afterwards.
	var enabled bool
	var rate string
	var maxPlans int
	if err := s.db.QueryRowContext(ctx, `SELECT enabled, commission_rate::text, max_plans_per_user FROM marketplace_settings WHERE singleton`).Scan(&enabled, &rate, &maxPlans); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), `UPDATE marketplace_settings SET enabled=$1, commission_rate=$2::numeric, max_plans_per_user=$3 WHERE singleton`, enabled, rate, maxPlans)
	})
	settings := func(enabled bool, rate string, maxPlans int) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE marketplace_settings SET enabled=$1, commission_rate=$2::numeric, max_plans_per_user=$3 WHERE singleton`, enabled, rate, maxPlans); err != nil {
			t.Fatal(err)
		}
	}
	settings(true, "0.1", 20)

	reviewer := globalUsageIntegrationUser(t, ctx, s, "market-reviewer-"+suffix, UserRoleMember)
	member := func(name string) User {
		return globalUsageIntegrationUser(t, ctx, s, "market-"+name+"-"+suffix, UserRoleMember)
	}
	credit := func(u User, amount string) {
		t.Helper()
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, reviewer.ID, "credit", now), UserID: u.ID, USDAmount: amount}); err != nil {
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
	account := func(owner User, kind string) string {
		t.Helper()
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		id := hex.EncodeToString(raw)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_accounts(id,provider,status,access_mode,owner_user_id,credential_kind)
			VALUES($1,'codex','available','shared',$2,$3)`, id, owner.ID, kind); err != nil {
			t.Fatal(err)
		}
		return id
	}
	create := func(seller User, name, price string) SellerPlan {
		t.Helper()
		plan, err := s.CreateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "publish", now),
			Name: name + " " + suffix, Description: "seller plan " + name, PriceUSD: price, Tier: BillingTierDay,
			AllowanceUSD: "10", ModelIDs: []string{"billing-priced-model"}, MinPeriodCount: 1, Active: true})
		if err != nil {
			t.Fatalf("create seller plan: %v", err)
		}
		return plan
	}
	review := func(plan SellerPlan, approve bool, note string) SellerPlan {
		t.Helper()
		result, err := s.ReviewSellerPlan(ctx, ReviewSellerPlanParams{PlanReviewWriteParams: PlanReviewWriteParams{
			PlanID: plan.ID, Version: plan.Version, ActorUserID: reviewer.ID, At: now}, Approve: approve, Note: note})
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		return result
	}
	submit := func(seller User, plan SellerPlan) SellerPlan {
		t.Helper()
		result, err := s.SubmitSellerPlan(ctx, PlanReviewWriteParams{PlanID: plan.ID, Version: plan.Version, ActorUserID: seller.ID, At: now})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return result
	}
	onSale := func(seller User, name, price string) SellerPlan {
		t.Helper()
		account(seller, "oauth")
		return review(submit(seller, create(seller, name, price)), true, "")
	}
	purchase := func(buyer User, plan SellerPlan, count int, acknowledge bool) (BillingPlanTransaction, error) {
		return s.PurchaseBillingPlan(ctx, PurchaseBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, buyer.ID, "buy", now),
			UserID: buyer.ID, PlanID: plan.ID, PlanVersion: plan.Version, PeriodCount: count, AcknowledgeThirdParty: acknowledge})
	}
	inMarket := func(plan SellerPlan) bool {
		t.Helper()
		plans, err := s.MarketPlans(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range plans {
			if item.ID == plan.ID {
				return true
			}
		}
		return false
	}
	saleCount := func(plan SellerPlan) int {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM marketplace_sales WHERE plan_id=$1`, plan.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	t.Run("review lifecycle decides what is on sale", func(t *testing.T) {
		seller := member("lifecycle")
		account(seller, "oauth")
		plan := create(seller, "Lifecycle", "2")
		if plan.ReviewStatus != PlanReviewDraft || plan.OnSale || inMarket(plan) {
			t.Fatalf("new plan = %+v", plan)
		}
		if _, err := s.ReviewSellerPlan(ctx, ReviewSellerPlanParams{PlanReviewWriteParams: PlanReviewWriteParams{
			PlanID: plan.ID, Version: plan.Version, ActorUserID: reviewer.ID}, Approve: true}); !errors.Is(err, ErrPlanReviewState) {
			t.Fatalf("approving a draft = %v", err)
		}
		plan = submit(seller, plan)
		pending, err := s.PendingSellerPlans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range pending {
			found = found || item.ID == plan.ID
		}
		if plan.ReviewStatus != PlanReviewPending || !found || inMarket(plan) {
			t.Fatalf("submitted plan = %+v in queue %v", plan, found)
		}
		if _, err := s.ReviewSellerPlan(ctx, ReviewSellerPlanParams{PlanReviewWriteParams: PlanReviewWriteParams{
			PlanID: plan.ID, Version: plan.Version, ActorUserID: reviewer.ID}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("rejection without a note = %v", err)
		}
		plan = review(plan, false, "价格说明不清楚")
		if plan.ReviewStatus != PlanReviewRejected || plan.ReviewNote != "价格说明不清楚" || inMarket(plan) {
			t.Fatalf("rejected plan = %+v", plan)
		}
		plan = review(submit(seller, plan), true, "")
		if plan.ReviewStatus != PlanReviewApproved || !plan.OnSale || !inMarket(plan) {
			t.Fatalf("approved plan = %+v", plan)
		}
		// Delisting and relisting is not a material change.
		delisted, err := s.SetSellerPlanListing(ctx, SetSellerPlanListingParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "delist", now),
			PlanID: plan.ID, Version: plan.Version, Active: false})
		if err != nil || delisted.ReviewStatus != PlanReviewApproved || delisted.OnSale || inMarket(plan) {
			t.Fatalf("delisted = %+v %v", delisted, err)
		}
		plan, err = s.SetSellerPlanListing(ctx, SetSellerPlanListingParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "relist", now),
			PlanID: delisted.ID, Version: delisted.Version, Active: true})
		if err != nil || !plan.OnSale {
			t.Fatalf("relisted = %+v %v", plan, err)
		}
		// A provider scope change keeps the approval and the version.
		own := account(seller, "oauth")
		scopes, err := s.PlanProviderScopes(ctx, seller.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetPlanProviderScope(ctx, SetPlanProviderScopeParams{PlanID: plan.ID, SellerUserID: seller.ID, Mode: PlanProviderScopeSelected,
			AccountIDs: []string{own}, ExpectedRevision: scopes[plan.ID].Revision, ActorUserID: seller.ID, At: now}); err != nil {
			t.Fatal(err)
		}
		scoped, err := s.SellerPlan(ctx, seller.ID, plan.ID)
		if err != nil || scoped.ReviewStatus != PlanReviewApproved || scoped.Version != plan.Version || !scoped.OnSale {
			t.Fatalf("scope change touched review = %+v %v", scoped, err)
		}
		// A price change sends the plan back to review and off sale.
		edited, err := s.UpdateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "reprice", now),
			PlanID: plan.ID, Version: plan.Version, Name: plan.Name, Description: plan.Description, PriceUSD: "3", Tier: plan.Tier,
			PeriodDays: plan.PeriodDays, AllowanceUSD: plan.AllowanceUSD, ModelIDs: plan.ModelIDs, MinPeriodCount: plan.MinPeriodCount, Active: true})
		if err != nil || edited.ReviewStatus != PlanReviewPending || edited.Version != plan.Version+1 || edited.OnSale || inMarket(plan) {
			t.Fatalf("material edit = %+v %v", edited, err)
		}
		// A description change counts too, for a plan already approved.
		approved := review(edited, true, "")
		described, err := s.UpdateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "describe", now),
			PlanID: approved.ID, Version: approved.Version, Name: approved.Name, Description: "new words", PriceUSD: approved.PriceUSD,
			Tier: approved.Tier, PeriodDays: approved.PeriodDays, AllowanceUSD: approved.AllowanceUSD, ModelIDs: approved.ModelIDs,
			MinPeriodCount: approved.MinPeriodCount, Active: true})
		if err != nil || described.ReviewStatus != PlanReviewPending {
			t.Fatalf("description edit = %+v %v", described, err)
		}
		// Owner suspension takes it off sale and blocks relisting.
		approved = review(described, true, "")
		suspended, err := s.SetSellerPlanSuspended(ctx, SetSellerPlanSuspendedParams{PlanReviewWriteParams: PlanReviewWriteParams{
			PlanID: approved.ID, ActorUserID: reviewer.ID, At: now}, Suspended: true, Reason: "违规"})
		if err != nil || !suspended.AdminSuspended || suspended.OnSale || inMarket(plan) {
			t.Fatalf("suspended = %+v %v", suspended, err)
		}
		delisted, err = s.SetSellerPlanListing(ctx, SetSellerPlanListingParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "delist suspended", now),
			PlanID: suspended.ID, Version: suspended.Version, Active: false})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetSellerPlanListing(ctx, SetSellerPlanListingParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "relist suspended", now),
			PlanID: delisted.ID, Version: delisted.Version, Active: true}); !errors.Is(err, ErrSellerPlanSuspended) {
			t.Fatalf("relisting a suspended plan = %v", err)
		}
	})

	t.Run("the Owner's plan routes only see platform plans", func(t *testing.T) {
		seller := member("isolation")
		plan := create(seller, "Isolation", "1")
		if _, err := s.UpdateBillingPlan(ctx, UpdateBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, reviewer.ID, "owner edit", now),
			PlanID: plan.ID, Version: plan.Version, Name: "taken", PriceUSD: "1", Tier: BillingTierDay, AllowanceUSD: "1",
			ModelIDs: plan.ModelIDs, MinPeriodCount: 1}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Owner edit of a seller plan = %v", err)
		}
		if _, err := s.DeleteBillingPlan(ctx, DeleteBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, reviewer.ID, "owner delete", now),
			PlanID: plan.ID, Version: plan.Version}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Owner delete of a seller plan = %v", err)
		}
		other := member("isolation-other")
		if _, err := s.SellerPlan(ctx, other.ID, plan.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another seller read the plan: %v", err)
		}
		if _, err := s.UpdateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, other.ID, "steal", now),
			PlanID: plan.ID, Version: plan.Version, Name: "x", PriceUSD: "1", Tier: BillingTierDay, AllowanceUSD: "1",
			ModelIDs: plan.ModelIDs, MinPeriodCount: 1}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another seller edited the plan: %v", err)
		}
		plans, err := s.ListBillingPlans(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range plans {
			if item.ID == plan.ID {
				t.Fatal("platform plan list includes a seller plan")
			}
		}
		if _, err := s.DeleteBillingPlan(ctx, DeleteBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "seller delete", now),
			PlanID: plan.ID, Version: plan.Version, SellerUserID: seller.ID}); err != nil {
			t.Fatalf("seller delete = %v", err)
		}
	})

	t.Run("plan limit counts the seller's plans", func(t *testing.T) {
		seller := member("limit")
		settings(true, "0.1", 1)
		defer settings(true, "0.1", 20)
		create(seller, "Only", "1")
		if _, err := s.CreateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "second", now),
			Name: "Second " + suffix, PriceUSD: "1", Tier: BillingTierDay, AllowanceUSD: "1", ModelIDs: []string{"billing-priced-model"}}); !errors.Is(err, ErrSellerPlanLimit) {
			t.Fatalf("second plan over the limit = %v", err)
		}
		settings(false, "0.1", 20)
		if _, err := s.CreateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: billingIntegrationWrite(t, seller.ID, "disabled", now),
			Name: "Off " + suffix, PriceUSD: "1", Tier: BillingTierDay, AllowanceUSD: "1", ModelIDs: []string{"billing-priced-model"}}); !errors.Is(err, ErrMarketplaceDisabled) {
			t.Fatalf("plan while disabled = %v", err)
		}
	})

	t.Run("a sale splits exactly and replays once", func(t *testing.T) {
		seller, buyer := member("split-seller"), member("split-buyer")
		plan := onSale(seller, "Split", "3.333333333333")
		credit(buyer, "20")
		params := PurchaseBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, buyer.ID, "buy", now),
			UserID: buyer.ID, PlanID: plan.ID, PlanVersion: plan.Version, PeriodCount: 3}
		first, err := s.PurchaseBillingPlan(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := s.PurchaseBillingPlan(ctx, params)
		if err != nil || replayed.Ledger.ID != first.Ledger.ID || saleCount(plan) != 1 {
			t.Fatalf("replay = %+v %v, sales %d", replayed, err, saleCount(plan))
		}
		// gross 9.999999999999, commission floor(0.9999999999999) = 0.999999999999
		if first.TotalUSD != "9.999999999999" || balance(buyer) != "10.000000000001" || balance(seller) != "9.000000000000" {
			t.Fatalf("amounts: total %s buyer %s seller %s", first.TotalUSD, balance(buyer), balance(seller))
		}
		sales, err := s.SellerSales(ctx, seller.ID, 50, 0)
		if err != nil || len(sales.Sales) != 1 {
			t.Fatalf("seller sales = %+v %v", sales, err)
		}
		sale := sales.Sales[0]
		if sale.BuyerNumber != 1 || sale.BuyerUserID != "" || sale.BuyerName != "" || sale.CommissionUSD != "0.999999999999" ||
			sale.NetUSD != "9.000000000000" || sale.CommissionRate != "0.1" || sale.Kind != "purchase" ||
			sales.Summary.NetUSD != "9.000000000000" || sales.Summary.Buyers != 1 {
			t.Fatalf("seller sale = %+v summary %+v", sale, sales.Summary)
		}
		all, err := s.AllMarketplaceSales(ctx, 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range all.Sales {
			found = found || item.PlanID == plan.ID && item.BuyerUserID == buyer.ID && item.SellerUserID == seller.ID
		}
		if !found {
			t.Fatal("Owner sales view misses the buyer and seller")
		}
		var actor *string
		var entryType, cashDelta string
		if err := s.db.QueryRowContext(ctx, `SELECT entry_type, actor_user_id::text, cash_delta_usd::text FROM billing_ledger_entries
			WHERE user_id=$1 AND entry_type='plan_sale_income'`, seller.ID).Scan(&entryType, &actor, &cashDelta); err != nil || actor != nil || cashDelta != "9.000000000000" {
			t.Fatalf("seller income entry = %s %v %s %v", entryType, actor, cashDelta, err)
		}
		var lot string
		if err := s.db.QueryRowContext(ctx, `SELECT remaining_usd::text FROM billing_cash_credit_lots WHERE user_id=$1`, seller.ID).Scan(&lot); err != nil || lot != "9.000000000000" {
			t.Fatalf("seller lot = %s %v", lot, err)
		}
		// The seller can spend the income like any cash.
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, reviewer.ID, "debit", now), UserID: seller.ID, USDAmount: "-9"}); err != nil {
			t.Fatalf("spend seller income: %v", err)
		}
		// A renewal pays the seller again, at the rate in force then, and keeps
		// the buyer number.
		settings(true, "0.25", 20)
		defer settings(true, "0.1", 20)
		renewed, err := s.RenewBillingSubscription(ctx, RenewBillingSubscriptionParams{BillingWriteParams: billingIntegrationWrite(t, buyer.ID, "renew", now),
			UserID: buyer.ID, SubscriptionID: first.Subscription.ID, PlanVersion: plan.Version,
			SubscriptionConfigVersion: first.Subscription.ConfigVersion, PeriodCount: 1})
		if err != nil {
			t.Fatalf("renew: %v", err)
		}
		sales, err = s.SellerSales(ctx, seller.ID, 50, 0)
		if err != nil || len(sales.Sales) != 2 || sales.Sales[0].Kind != "renewal" || sales.Sales[0].BuyerNumber != 1 ||
			sales.Sales[0].CommissionRate != "0.25" || sales.Sales[0].CommissionUSD != "0.833333333333" || sales.Sales[0].NetUSD != "2.500000000000" {
			t.Fatalf("renewal sale = %+v %v (renewed %+v)", sales, err, renewed.TotalUSD)
		}
		if balance(seller) != "2.500000000000" {
			t.Fatalf("seller balance after renewal = %s", balance(seller))
		}
		// A second buyer gets the next number.
		other := member("split-other")
		credit(other, "10")
		if _, err := purchase(other, plan, 1, false); err != nil {
			t.Fatal(err)
		}
		sales, _ = s.SellerSales(ctx, seller.ID, 1, 0)
		if sales.Sales[0].BuyerNumber != 2 || sales.Total != 3 || sales.Summary.Buyers != 2 {
			t.Fatalf("second buyer = %+v", sales)
		}
	})

	t.Run("sales refuse self purchase, closed plans and missing consent", func(t *testing.T) {
		seller, buyer := member("gate-seller"), member("gate-buyer")
		plan := onSale(seller, "Gate", "1")
		credit(seller, "5")
		credit(buyer, "50")
		if _, err := purchase(seller, plan, 1, false); !errors.Is(err, ErrSelfPurchase) {
			t.Fatalf("self purchase = %v", err)
		}
		account(seller, "supplier")
		if _, err := purchase(buyer, plan, 1, false); !errors.Is(err, ErrThirdPartyAcknowledgement) {
			t.Fatalf("third-party purchase without consent = %v", err)
		}
		if _, err := purchase(buyer, plan, 1, true); err != nil {
			t.Fatalf("third-party purchase with consent = %v", err)
		}
		settings(false, "0.1", 20)
		_, err := purchase(buyer, plan, 1, true)
		settings(true, "0.1", 20)
		if !errors.Is(err, ErrPlanNotOnSale) {
			t.Fatalf("purchase while disabled = %v", err)
		}
		if err := s.DisableUser(ctx, seller.ID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := purchase(buyer, plan, 1, true); !errors.Is(err, ErrPlanNotOnSale) || inMarket(plan) {
			t.Fatalf("purchase from a disabled seller = %v", err)
		}
		// Not enough cash rolls the whole sale back.
		rich, poor := member("gate-rich"), member("gate-poor")
		expensive := onSale(rich, "Expensive", "100")
		credit(poor, "1")
		before := balance(rich)
		if _, err := purchase(poor, expensive, 1, false); !errors.As(err, new(*InsufficientFundsError)) {
			t.Fatalf("purchase beyond the balance = %v", err)
		}
		if balance(rich) != before || balance(poor) != "1.000000000000" || saleCount(expensive) != 0 {
			t.Fatal("a failed purchase left money or a sale behind")
		}
	})

	t.Run("concurrent purchases neither deadlock nor double charge", func(t *testing.T) {
		a, b := member("mutual-a"), member("mutual-b")
		planA, planB := onSale(a, "Mutual A", "1"), onSale(b, "Mutual B", "1")
		credit(a, "100")
		credit(b, "100")
		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := 0; i < 20; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := purchase(a, planB, 1, false)
				errs <- err
			}()
			go func() {
				defer wg.Done()
				_, err := purchase(b, planA, 1, false)
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("mutual purchase: %v", err)
			}
		}
		// Each paid 20 and received 20 x 0.9.
		if balance(a) != "98.000000000000" || balance(b) != "98.000000000000" {
			t.Fatalf("balances after mutual purchases: %s %s", balance(a), balance(b))
		}
		// The same operation sent twice at once charges once.
		buyer := member("duplicate")
		credit(buyer, "10")
		params := PurchaseBillingPlanParams{BillingWriteParams: billingIntegrationWrite(t, buyer.ID, "duplicate", now),
			UserID: buyer.ID, PlanID: planA.ID, PlanVersion: planA.Version, PeriodCount: 1}
		var dup sync.WaitGroup
		results := make(chan error, 8)
		for i := 0; i < 8; i++ {
			dup.Add(1)
			go func() {
				defer dup.Done()
				_, err := s.PurchaseBillingPlan(ctx, params)
				results <- err
			}()
		}
		dup.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatalf("duplicate purchase: %v", err)
			}
		}
		if balance(buyer) != "9.000000000000" {
			t.Fatalf("duplicate operation charged %s", balance(buyer))
		}
	})

	t.Run("plan usage aggregates by account without buyers", func(t *testing.T) {
		seller, buyer := member("usage-seller"), member("usage-buyer")
		plan := onSale(seller, "Usage", "1")
		credit(buyer, "5")
		bought, err := purchase(buyer, plan, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		usage, err := s.SellerPlanUsage(ctx, seller.ID, plan.ID)
		if err != nil || usage.Subscribers != 1 || usage.ConsumedUSD != "0.000000000000" || len(usage.Accounts) != 0 {
			t.Fatalf("usage before requests = %+v %v (subscription %s)", usage, err, bought.Subscription.ID)
		}
		if _, err := s.SellerPlanUsage(ctx, buyer.ID, plan.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("buyer read seller usage: %v", err)
		}
	})
}
