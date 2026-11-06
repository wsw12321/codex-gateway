//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGroupUsageBreakdownPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return clock }
	suffix := fmt.Sprint(clock.UnixNano())
	actor := globalUsageIntegrationUser(t, ctx, s, "breakdown-actor-"+suffix, UserRoleMember)
	alice := globalUsageIntegrationUser(t, ctx, s, "breakdown-alice-"+suffix, UserRoleMember)
	bob := globalUsageIntegrationUser(t, ctx, s, "breakdown-bob-"+suffix, UserRoleMember)
	carol := globalUsageIntegrationUser(t, ctx, s, "breakdown-carol-"+suffix, UserRoleMember)
	dave := globalUsageIntegrationUser(t, ctx, s, "breakdown-dave-"+suffix, UserRoleMember)
	write := func() BillingWriteParams { return billingIntegrationWrite(t, actor.ID, "group usage breakdown", clock) }
	account := func(provider, seed, alias string) string {
		t.Helper()
		id := upstreamIntegrationAccountID(seed + "-" + suffix)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_accounts(id,provider,alias) VALUES($1,$2,$3)`, id, provider, alias); err != nil {
			t.Fatal(err)
		}
		return id
	}
	members := func(g Group, action string, users ...User) {
		t.Helper()
		ids := make([]string, 0, len(users))
		for _, user := range users {
			ids = append(ids, user.ID)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, Action: action, UserIDs: ids}); err != nil {
			t.Fatal(err)
		}
	}

	unlimited := 0
	memberLimit := "40"
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Breakdown " + suffix, LimitUSD: "100",
		MemberLimitUSD: &memberLimit, MemberLimitSet: true, Period: "week", PeriodCount: &unlimited})
	if err != nil {
		t.Fatal(err)
	}
	members(group, "add", alice, bob, carol)
	members(group, "remove", carol)

	poolCodex := account(UpstreamProviderCodex, "breakdown-pool-codex", "Codex 主力")
	poolClaude := account(UpstreamProviderAnthropic, "breakdown-pool-claude", "")
	paidOnly := account(UpstreamProviderCodex, "breakdown-paid-only", "")
	unrelated := account(UpstreamProviderCodex, "breakdown-unrelated", "")
	for provider, ids := range map[string][]string{UpstreamProviderCodex: {poolCodex}, UpstreamProviderAnthropic: {poolClaude}} {
		if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: group.ID,
			Provider: provider, Strategy: "selected", AccountIDs: ids}); err != nil {
			t.Fatal(err)
		}
	}

	period := group.PeriodID
	for user, used := range map[string]string{alice.ID: "30", bob.ID: "12", carol.ID: "5"} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO group_member_usage(period_id,user_id,used_usd) VALUES($1,$2,$3::numeric)`, period, user, used); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=47 WHERE id=$1`, period); err != nil {
		t.Fatal(err)
	}
	sequence := 0
	ledger := func(user User, accountID, cost, groupCharged string, at time.Time) {
		t.Helper()
		sequence++
		var groupID, periodID any
		if groupCharged != "0" {
			groupID, periodID = group.ID, period
		}
		costValue, _ := new(big.Rat).SetString(cost)
		chargedValue, _ := new(big.Rat).SetString(groupCharged)
		personal := new(big.Rat).Sub(costValue, chargedValue).FloatString(12)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries
				(user_id,entry_type,amount_usd,cash_delta_usd,request_id,model,input_tokens,cached_input_tokens,output_tokens,
				 actual_cost_usd,charged_usd,group_charged_usd,personal_charged_usd,uncovered_usd,reason,created_at,
				 usage_requested_at,upstream_account_id,group_id,group_period_id)
			VALUES($1,'usage_charge',$2::numeric,0,$3,'breakdown-model',1,0,1,$2::numeric,$2::numeric,$4::numeric,$5::numeric,0,
				'group usage breakdown fixture',$6,$6,$7,$8,$9)`,
			user.ID, cost, fmt.Sprintf("group-breakdown-%s-%02d", suffix, sequence), groupCharged, personal, at, accountID, groupID, periodID); err != nil {
			t.Fatalf("insert ledger %d: %v", sequence, err)
		}
	}
	inside := clock.Add(time.Minute)
	ledger(alice, poolCodex, "30", "25", inside)
	ledger(bob, poolCodex, "12", "12", inside)
	ledger(dave, poolCodex, "8", "0", inside)
	ledger(dave, poolCodex, "99", "0", group.PeriodStartsAt.Add(-time.Hour)) // before the period
	ledger(carol, poolClaude, "5", "5", inside)
	ledger(dave, poolClaude, "2.5", "0", inside)
	ledger(alice, paidOnly, "1", "1", inside)
	ledger(dave, unrelated, "9", "0", inside)

	got, err := s.GroupUsageBreakdown(ctx, group.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupID != group.ID || got.Period.ID != period || !got.CurrentPeriod || !got.DataComplete ||
		got.LimitUSD != "100.000000000000" || got.MemberLimitUSD == nil || *got.MemberLimitUSD != "40.000000000000" ||
		got.UsedUSD != "47.000000000000" || got.RemainingUSD != "53.000000000000" {
		t.Fatalf("summary = %+v", got)
	}
	wantMembers := []GroupUsageMember{
		{UserID: alice.ID, Username: alice.Username, DisplayName: alice.DisplayName, CurrentMember: true, GroupUsedUSD: "30.000000000000"},
		{UserID: bob.ID, Username: bob.Username, DisplayName: bob.DisplayName, CurrentMember: true, GroupUsedUSD: "12.000000000000"},
		{UserID: carol.ID, Username: carol.Username, DisplayName: carol.DisplayName, CurrentMember: false, GroupUsedUSD: "5.000000000000"},
	}
	if len(got.Members) != len(wantMembers) {
		t.Fatalf("members = %+v", got.Members)
	}
	for index, want := range wantMembers {
		if got.Members[index] != want {
			t.Fatalf("member %d = %+v, want %+v", index, got.Members[index], want)
		}
	}
	accounts := map[string]GroupUsageAccount{}
	for _, value := range got.Accounts {
		accounts[value.AccountID] = value
	}
	if len(got.Accounts) != 3 {
		t.Fatalf("accounts = %+v", got.Accounts)
	}
	if _, ok := accounts[unrelated]; ok {
		t.Fatal("an account outside the pool that the group never paid for must not appear")
	}
	check := func(id string, inPool bool, total, member, other, charged string, users map[string]string) {
		t.Helper()
		value := accounts[id]
		if value.InPool != inPool || value.TotalCostUSD != total || value.MemberCostUSD != member || value.NonMemberCostUSD != other || value.GroupChargedUSD != charged {
			t.Fatalf("account %s = %+v", id, value)
		}
		if len(value.Members) != len(users) {
			t.Fatalf("account %s members = %+v", id, value.Members)
		}
		for _, user := range value.Members {
			if users[user.UserID] != user.CostUSD {
				t.Fatalf("account %s member %s cost %s, want %s", id, user.UserID, user.CostUSD, users[user.UserID])
			}
		}
	}
	check(poolCodex, true, "50.000000000000", "42.000000000000", "8.000000000000", "37.000000000000",
		map[string]string{alice.ID: "30.000000000000", bob.ID: "12.000000000000"})
	check(poolClaude, true, "7.500000000000", "5.000000000000", "2.500000000000", "5.000000000000",
		map[string]string{carol.ID: "5.000000000000"})
	check(paidOnly, false, "1.000000000000", "1.000000000000", "0.000000000000", "1.000000000000",
		map[string]string{alice.ID: "1.000000000000"})
	if accounts[poolCodex].Alias != "Codex 主力" || accounts[poolClaude].Provider != UpstreamProviderAnthropic {
		t.Fatalf("account metadata = %+v %+v", accounts[poolCodex], accounts[poolClaude])
	}
	if len(got.Periods) != 1 || got.Periods[0].ID != period {
		t.Fatalf("periods = %+v", got.Periods)
	}

	// A new period starts empty; the earlier one stays selectable by ID.
	clock = group.PeriodEndsAt.Add(time.Hour)
	next, err := s.GroupUsageBreakdown(ctx, group.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Period.ID == period || !next.CurrentPeriod || next.UsedUSD != "0.000000000000" || len(next.Periods) != 2 || next.Periods[0].ID != next.Period.ID {
		t.Fatalf("rolled period = %+v", next)
	}
	for _, member := range next.Members {
		if member.UserID == carol.ID || member.GroupUsedUSD != "0.000000000000" {
			t.Fatalf("a new period lists current members with no usage only: %+v", next.Members)
		}
	}
	earlier, err := s.GroupUsageBreakdown(ctx, group.ID, period)
	if err != nil {
		t.Fatal(err)
	}
	if earlier.CurrentPeriod || earlier.UsedUSD != "47.000000000000" || earlier.LimitUSD != "100.000000000000" || len(earlier.Members) != 3 || len(earlier.Accounts) != 3 {
		t.Fatalf("earlier period = %+v", earlier)
	}

	other, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Other " + suffix, LimitUSD: "1", Period: "day"})
	if err != nil {
		t.Fatal(err)
	}

	// The Owner's pool view lists selected Codex and Claude accounts, Codex
	// first, regardless of the member visibility switch.
	agy := account(UpstreamProviderAntigravity, "breakdown-agy", "")
	if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: group.ID,
		Provider: UpstreamProviderAntigravity, Strategy: "selected", AccountIDs: []string{agy}}); err != nil {
		t.Fatal(err)
	}
	pool, err := s.ListGroupPoolAccounts(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 2 || pool[0] != (GroupPoolAccount{Provider: UpstreamProviderCodex, AccountID: poolCodex, Alias: "Codex 主力"}) ||
		pool[1].Provider != UpstreamProviderAnthropic || pool[1].AccountID != poolClaude {
		t.Fatalf("pool accounts = %+v", pool)
	}
	if empty, err := s.ListGroupPoolAccounts(ctx, other.ID); err != nil || len(empty) != 0 {
		t.Fatalf("all-accounts pools = %+v %v", empty, err)
	}
	if _, err := s.ListGroupPoolAccounts(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group err = %v", err)
	}
	for name, test := range map[string]struct {
		group, period string
		want          error
	}{
		"foreign period": {group.ID, other.PeriodID, ErrNotFound},
		"unknown period": {group.ID, uuid.NewString(), ErrNotFound},
		"invalid period": {group.ID, "not-a-uuid", ErrInvalid},
		"unknown group":  {uuid.NewString(), "", ErrNotFound},
	} {
		if _, err := s.GroupUsageBreakdown(ctx, test.group, test.period); !errors.Is(err, test.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, test.want)
		}
	}
}
