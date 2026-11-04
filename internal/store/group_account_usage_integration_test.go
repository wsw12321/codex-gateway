//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestGroupAccountUsagePostgresIntegration(t *testing.T) {
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
	actor := globalUsageIntegrationUser(t, ctx, s, "usage-actor-"+suffix, UserRoleMember)
	member := globalUsageIntegrationUser(t, ctx, s, "usage-member-"+suffix, UserRoleMember)
	outsider := globalUsageIntegrationUser(t, ctx, s, "usage-outsider-"+suffix, UserRoleMember)
	write := func() BillingWriteParams {
		return billingIntegrationWrite(t, actor.ID, "account usage integration", clock)
	}
	account := func(provider, seed, alias string) string {
		t.Helper()
		id := upstreamIntegrationAccountID(seed + "-" + suffix)
		// Direct inserts avoid a provider-wide sync that would mark other
		// tests' accounts unavailable.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO upstream_accounts(id,provider,alias) VALUES($1,$2,$3)`, id, provider, alias); err != nil {
			t.Fatal(err)
		}
		return id
	}
	unlimited, single := 0, 1
	create := func(name string, periods *int) Group {
		t.Helper()
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: name, LimitUSD: "10", Period: "day", PeriodCount: periods})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, UserIDs: []string{member.ID}, Action: "add"}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	pool := func(g Group, provider string, ids ...string) {
		t.Helper()
		strategy := "all"
		if ids != nil {
			strategy = "selected"
		}
		if _, err := s.SetGroupUpstreamAccounts(ctx, SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: g.ID, Provider: provider, Strategy: strategy, AccountIDs: ids}); err != nil {
			t.Fatal(err)
		}
	}
	visible := func(g Group, enabled bool) Group {
		t.Helper()
		result, err := s.SetGroupAccountUsageVisibility(ctx, SetGroupAccountUsageVisibilityParams{BillingWriteParams: write(), GroupID: g.ID, Enabled: enabled})
		if err != nil {
			t.Fatal(err)
		}
		if result.AccountUsageVisible != enabled || result.ID != g.ID {
			t.Fatalf("visibility result=%+v", result.GroupSummary)
		}
		return result
	}
	list := func(user User) []GroupAccountUsageTarget {
		t.Helper()
		values, err := s.ListGroupAccountUsageTargets(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		return values
	}

	codexA := account(UpstreamProviderCodex, "usage-codex-a", "主力")
	codexB := account(UpstreamProviderCodex, "usage-codex-b", "")
	claudeA := account(UpstreamProviderAnthropic, "usage-claude-a", "Claude-A")
	claudeB := account(UpstreamProviderAnthropic, "usage-claude-b", "")
	agy := account(UpstreamProviderAntigravity, "usage-agy", "")

	research := create("Research "+suffix, &unlimited)
	if research.AccountUsageVisible {
		t.Fatal("new groups must not expose account usage")
	}
	// The example from the feature request: Codex and Antigravity use all
	// accounts, Claude is limited to two selected accounts.
	pool(research, UpstreamProviderCodex)
	pool(research, UpstreamProviderAntigravity)
	pool(research, UpstreamProviderAnthropic, claudeB, claudeA)
	if got := list(member); len(got) != 0 {
		t.Fatalf("disabled group exposed accounts: %+v", got)
	}
	research = visible(research, true)
	if got, err := s.GetGroup(ctx, research.ID); err != nil || !got.AccountUsageVisible {
		t.Fatalf("visibility not persisted: %+v %v", got.GroupSummary, err)
	}
	claudes := []GroupAccountUsageTarget{
		{GroupID: research.ID, GroupName: research.Name, Provider: UpstreamProviderAnthropic, AccountID: claudeA, Alias: "Claude-A"},
		{GroupID: research.ID, GroupName: research.Name, Provider: UpstreamProviderAnthropic, AccountID: claudeB},
	}
	if claudeB < claudeA {
		claudes[0], claudes[1] = claudes[1], claudes[0]
	}
	if got := list(member); !reflect.DeepEqual(got, claudes) {
		t.Fatalf("selected Claude accounts=%+v, want %+v", got, claudes)
	}
	if got := list(outsider); len(got) != 0 {
		t.Fatalf("non-member saw accounts: %+v", got)
	}

	// Antigravity has no five-hour or weekly window, even when selected; a
	// selected Codex pool joins and sorts before Claude.
	pool(research, UpstreamProviderAntigravity, agy)
	pool(research, UpstreamProviderCodex, codexA)
	got := list(member)
	if len(got) != 3 || got[0].Provider != UpstreamProviderCodex || got[0].AccountID != codexA || got[0].Alias != "主力" || !reflect.DeepEqual(got[1:], claudes) {
		t.Fatalf("mixed pools=%+v", got)
	}
	// An explicit empty selection exposes nothing for that provider.
	pool(research, UpstreamProviderCodex, []string{}...)
	if got := list(member); !reflect.DeepEqual(got, claudes) {
		t.Fatalf("empty Codex selection=%+v", got)
	}

	// A second, higher-priority group lists first; a shared account appears
	// once per group that grants it.
	engineering := create("Engineering "+suffix, &single)
	pool(engineering, UpstreamProviderCodex, codexB)
	pool(engineering, UpstreamProviderAnthropic, claudeA)
	visible(engineering, true)
	if _, err := s.SetGroupMembershipPriority(ctx, SetGroupMembershipPreferenceParams{BillingWriteParams: billingIntegrationWrite(t, member.ID, "prefer engineering", clock), GroupID: engineering.ID, UserID: member.ID, Priority: 1}); err != nil {
		t.Fatal(err)
	}
	got = list(member)
	if len(got) != 4 || got[0].GroupID != engineering.ID || got[0].AccountID != codexB || got[1].AccountID != claudeA || got[1].GroupID != engineering.ID || got[2].GroupID != research.ID {
		t.Fatalf("priority order=%+v", got)
	}

	// Expired and archived groups stop exposing accounts.
	clock = clock.Add(72 * time.Hour)
	if got := list(member); !reflect.DeepEqual(got, claudes) {
		t.Fatalf("expired group still visible: %+v", got)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE user_groups SET archived_at=$2 WHERE id=$1`, research.ID, clock); err != nil {
		t.Fatal(err)
	}
	if got := list(member); len(got) != 0 {
		t.Fatalf("archived group still visible: %+v", got)
	}
	if _, err := s.SetGroupAccountUsageVisibility(ctx, SetGroupAccountUsageVisibilityParams{BillingWriteParams: write(), GroupID: research.ID, Enabled: false}); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived group write error=%v, want conflict", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE user_groups SET archived_at=NULL WHERE id=$1`, research.ID); err != nil {
		t.Fatal(err)
	}

	// Writes are idempotent; replaying an operation ID with the opposite value
	// is a conflict, and disabling hides the accounts again.
	replay := write()
	first, err := s.SetGroupAccountUsageVisibility(ctx, SetGroupAccountUsageVisibilityParams{BillingWriteParams: replay, GroupID: research.ID, Enabled: false})
	if err != nil || first.AccountUsageVisible {
		t.Fatalf("disable=%+v %v", first.GroupSummary, err)
	}
	again, err := s.SetGroupAccountUsageVisibility(ctx, SetGroupAccountUsageVisibilityParams{BillingWriteParams: replay, GroupID: research.ID, Enabled: false})
	if err != nil || again.AccountUsageVisible || !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("replay=%+v %v", again.GroupSummary, err)
	}
	if _, err := s.SetGroupAccountUsageVisibility(ctx, SetGroupAccountUsageVisibilityParams{BillingWriteParams: replay, GroupID: research.ID, Enabled: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched replay error=%v, want conflict", err)
	}
	if got := list(member); len(got) != 0 {
		t.Fatalf("disabled group still visible: %+v", got)
	}
	var events int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE subject_id=$1 AND event_type IN ('group.account_usage.enable','group.account_usage.disable')`, research.ID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("visibility audit events=%d err=%v", events, err)
	}
}
