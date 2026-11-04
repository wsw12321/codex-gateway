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
	"sync"
	"testing"
	"time"
)

func TestFundingLayersPostgresIntegration(t *testing.T) {
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
	actor := globalUsageIntegrationUser(t, ctx, s, "layers-actor-"+suffix, UserRoleMember)
	write := func() BillingWriteParams {
		return billingIntegrationWrite(t, actor.ID, "funding layer integration", now)
	}
	account := func(exclusiveTo ...User) string {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		id := hex.EncodeToString(raw)
		mode := "shared"
		if len(exclusiveTo) > 0 {
			mode = "exclusive"
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_accounts(id,provider,status,access_mode) VALUES($1,'codex','available',$2)`, id, mode); err != nil {
			t.Fatal(err)
		}
		for _, user := range exclusiveTo {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_account_users(upstream_account_id,user_id) VALUES($1,$2)`, id, user.ID); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	group := func(name string, accounts ...string) Group {
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: name + " " + suffix, LimitUSD: "100", Period: "week"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: g.ID,
			Provider: UpstreamProviderCodex, Strategy: "selected", AccountIDs: accounts}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	join := func(g Group, priority int, users ...User) {
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, Action: "add", UserIDs: ids}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE group_memberships SET priority=$2 WHERE group_id=$1`, g.ID, priority); err != nil {
			t.Fatal(err)
		}
	}
	credit := func(u User, amount string) {
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: u.ID, USDAmount: amount}); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(u User, d Device, k APIKey, id string) RequestAdmission {
		t.Helper()
		admission, err := s.AdmitRequest(ctx, billingIntegrationAdmission(u, d, k, id, now))
		if err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
		return admission
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
		// Replaying settlement is a no-op that returns the bound result.
		reservation, err := s.SettleBilling(ctx, id, now.Add(3*time.Second))
		if err != nil {
			t.Fatalf("replay settlement %s: %v", id, err)
		}
		return reservation
	}
	groupUsed := func(g Group) string {
		t.Helper()
		got, err := s.GetGroup(ctx, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.UsedUSD
	}
	layers := func(r *BillingReservation) []UpstreamAllocationGroup {
		if r == nil {
			return nil
		}
		return r.AllocationGroups
	}

	t.Run("incident replay falls through empty pools to personal funds", func(t *testing.T) {
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "layers-incident-"+suffix)
		exclusive := account(u)
		reauth := account()
		emptyPool := group("Empty pool")
		reauthPool := group("Reauth pool", reauth)
		join(emptyPool, 100, u)
		join(reauthPool, 100, u)
		credit(u, "100")
		id := billingIntegrationRequestID(suffix, "incident", 1)
		admission := admit(u, d, k, id)
		want := []UpstreamAllocationGroup{
			{GroupID: reauthPool.ID, AccountIDs: []string{reauth}},
			{GroupID: PersonalFundingLayerID, AccountIDs: []string{}, AllAccounts: true},
		}
		if got := layers(admission.Billing); !reflect.DeepEqual(got, want) || admission.Billing.FundingRuleVersion != 4 {
			t.Fatalf("layers = %+v, want %+v", got, want)
		}
		complete(id, exclusive, 3)
		r := settle(id)
		groupBillingAssertSplit(t, r, "0", "3", "0")
		if r.GroupID != nil || r.GroupPeriodID != nil {
			t.Fatalf("personal-served request was attributed to a group: %+v", r)
		}
		var ledgerGroup *string
		if err := s.db.QueryRowContext(ctx, `SELECT group_id::text FROM billing_ledger_entries WHERE request_id=$1`, id).Scan(&ledgerGroup); err != nil || ledgerGroup != nil {
			t.Fatalf("ledger group = %v %v", ledgerGroup, err)
		}
		if groupUsed(emptyPool) != "0.000000000000" || groupUsed(reauthPool) != "0.000000000000" {
			t.Fatal("unused group periods were charged")
		}
	})

	t.Run("serving account selects the first funded group that may use it", func(t *testing.T) {
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "layers-shared-"+suffix)
		shared := account()
		first, second := group("First", shared), group("Second", shared)
		join(first, 1, u)
		join(second, 2, u)
		credit(u, "50")
		id := billingIntegrationRequestID(suffix, "second-pays", 1)
		admission := admit(u, d, k, id)
		if got := layers(admission.Billing); len(got) != 3 || got[0].GroupID != first.ID || got[1].GroupID != second.ID || !got[2].AllAccounts {
			t.Fatalf("priority layers = %+v", got)
		}
		// The first group is drained by other requests before this one settles.
		if _, err := s.db.ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=limit_usd WHERE id=$1`, first.PeriodID); err != nil {
			t.Fatal(err)
		}
		// A later policy edit cannot change the admission snapshot.
		if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: second.ID,
			Provider: UpstreamProviderCodex, Strategy: "selected", AccountIDs: []string{}}); err != nil {
			t.Fatal(err)
		}
		complete(id, shared, 4)
		r := settle(id)
		groupBillingAssertSplit(t, r, "4", "0", "0")
		if r.GroupID == nil || *r.GroupID != second.ID {
			t.Fatalf("paying group = %v, want second", r.GroupID)
		}

		// Missing attribution keeps the pre-rule-4 behavior: the first layer
		// pays what it can and personal sources cover the remainder.
		if _, err := s.db.ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=limit_usd-1 WHERE id=$1`, first.PeriodID); err != nil {
			t.Fatal(err)
		}
		missing := billingIntegrationRequestID(suffix, "unattributed", 1)
		if got := layers(admit(u, d, k, missing).Billing); len(got) != 2 || got[0].GroupID != first.ID || !got[1].AllAccounts {
			t.Fatalf("layers after the pool edit = %+v", got)
		}
		complete(missing, "", 3)
		r = settle(missing)
		if r.GroupID == nil || *r.GroupID != first.ID {
			t.Fatalf("unattributed request paid by %v, want first layer", r.GroupID)
		}
		groupBillingAssertSplit(t, r, "1", "2", "0")

		// A request that never reached an account is free and binds no money.
		failed := billingIntegrationRequestID(suffix, "failed", 1)
		admit(u, d, k, failed)
		complete(failed, "", 0)
		r = settle(failed)
		groupBillingAssertSplit(t, r, "0", "0", "0")
	})

	t.Run("no usable pool and no personal funds rejects before any write", func(t *testing.T) {
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "layers-none-"+suffix)
		join(group("Unusable", account()), 100, u)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO user_upstream_access(user_id,provider,mode) VALUES($1,'codex','selected')`, u.ID); err != nil {
			t.Fatal(err)
		}
		id := billingIntegrationRequestID(suffix, "rejected", 1)
		_, err := s.AdmitRequest(ctx, billingIntegrationAdmission(u, d, k, id, now))
		var noAccount *NoUpstreamAccountError
		if !errors.As(err, &noAccount) {
			t.Fatalf("admission error = %v", err)
		}
		var rows int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_requests WHERE request_id=$1)
			+ (SELECT count(*) FROM quota_reservations WHERE request_id=$1)
			+ (SELECT count(*) FROM billing_reservations WHERE request_id=$1)`, id).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("rejected admission wrote %d rows: %v", rows, err)
		}
	})

	t.Run("rule 3 reservations settle with their admission group", func(t *testing.T) {
		u, d, k := billingIntegrationPrincipal(t, ctx, s, "layers-legacy-"+suffix)
		shared := account()
		g := group("Legacy", shared)
		join(g, 100, u)
		id := billingIntegrationRequestID(suffix, "rule3", 1)
		admit(u, d, k, id)
		if _, err := s.db.ExecContext(ctx, `DELETE FROM billing_reservation_layers WHERE request_id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE billing_reservations SET funding_rule_version=3 WHERE request_id=$1`, id); err != nil {
			t.Fatal(err)
		}
		complete(id, "", 1)
		r := settle(id)
		if r.FundingRuleVersion != 3 || r.GroupID == nil || *r.GroupID != g.ID {
			t.Fatalf("rule 3 settlement = %+v", r)
		}
		groupBillingAssertSplit(t, r, "1", "0", "0")
	})

	t.Run("parallel settlements over overlapping groups do not deadlock", func(t *testing.T) {
		shared := account()
		left, right := group("Left", shared), group("Right", shared)
		type request struct{ id string }
		var requests []request
		for i, order := range [][2]Group{{left, right}, {right, left}} {
			u, d, k := billingIntegrationPrincipal(t, ctx, s, fmt.Sprintf("layers-parallel-%d-%s", i, suffix))
			join(order[0], 1, u)
			join(order[1], 2, u)
			for n := range 4 {
				id := billingIntegrationRequestID(suffix, fmt.Sprintf("parallel-%d", i), n+1)
				admit(u, d, k, id)
				complete(id, shared, 1)
				requests = append(requests, request{id})
			}
		}
		var wg sync.WaitGroup
		errs := make(chan error, len(requests))
		for _, r := range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.SettleRequest(ctx, r.id, now.Add(2*time.Second)); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("parallel settlement: %v", err)
		}
		if groupUsed(left) != "4.000000000000" || groupUsed(right) != "4.000000000000" {
			t.Fatalf("group usage left=%s right=%s", groupUsed(left), groupUsed(right))
		}
	})
}
