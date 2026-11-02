//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestUserUpstreamAccessBatchPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	owner := globalUsageIntegrationUser(t, ctx, s, "batch-access-owner", UserRoleOwner)
	first := usageSummaryUser(t, ctx, s, "batch-access-first")
	second := usageSummaryUser(t, ctx, s, "batch-access-second")
	pending := usageSummaryUser(t, ctx, s, "batch-access-pending")
	agy := s.WithUpstreamProvider(UpstreamProviderAntigravity)
	var codexIDs, agyIDs []string
	for i := range 3 {
		codexIDs = append(codexIDs, upstreamIntegrationAccountID(fmt.Sprintf("batch-codex-%d", i)))
		agyIDs = append(agyIDs, upstreamIntegrationAccountID(fmt.Sprintf("batch-agy-%d", i)))
	}
	sort.Strings(codexIDs)
	sort.Strings(agyIDs)
	for i := range 3 {
		if err := s.EnsureUpstreamAccount(ctx, codexIDs[i], time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := agy.EnsureUpstreamAccount(ctx, agyIDs[i], time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	setSingle := func(repository *Store, userID, mode string, ids ...string) {
		t.Helper()
		if _, err := repository.SetUserUpstreamAccess(ctx, SetUserUpstreamAccessParams{
			UserID: userID, Mode: mode, AccountIDs: ids, Reason: "batch fixture", ActorUserID: owner.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	setSingle(s, second.ID, "selected", codexIDs[1])
	setSingle(s, pending.ID, "selected")
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, pending.ID); err != nil {
		t.Fatal(err)
	}
	batch := func(action string, userIDs, accountIDs, providers []string) UserUpstreamAccessBatchResult {
		t.Helper()
		result, err := s.SetUserUpstreamAccessBatch(ctx, SetUserUpstreamAccessBatchParams{
			UserIDs: userIDs, Action: action, AccountIDs: accountIDs, Providers: providers, Reason: "batch " + action, ActorUserID: owner.ID,
		})
		if err != nil {
			t.Fatalf("batch %s: %v", action, err)
		}
		if len(result.Access) != result.TargetCount {
			t.Fatalf("batch %s access rows = %d; targets %d", action, len(result.Access), result.TargetCount)
		}
		return result
	}
	get := func(repository *Store, userID, mode string, ids ...string) {
		t.Helper()
		got, err := repository.GetUserUpstreamAccess(ctx, userID)
		if ids == nil {
			ids = []string{}
		}
		if err != nil || got.Mode != mode || !reflect.DeepEqual(got.AccountIDs, ids) {
			t.Fatalf("%s access = %+v, %v; want %s %v", repository.upstreamProviderName(), got, err, mode, ids)
		}
	}
	audits := func(userID string) int {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE subject_id=$1 AND event_type='user.upstream_access_changed'`, userID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	users := []string{first.ID, second.ID}

	// Disabling converts all to every other registered account of that
	// provider; a selected list only loses the disabled accounts.
	result := batch(UserUpstreamAccessBatchDisable, users, []string{codexIDs[0], agyIDs[0]}, nil)
	if result.TargetCount != 4 || result.ChangedCount != 3 || result.ConvertedCount != 3 {
		t.Fatalf("disable result = %+v", result)
	}
	get(s, first.ID, "selected", codexIDs[1], codexIDs[2])
	get(agy, first.ID, "selected", agyIDs[1], agyIDs[2])
	get(s, second.ID, "selected", codexIDs[1])
	get(agy, second.ID, "selected", agyIDs[1], agyIDs[2])
	assertUserUpstreamCandidates(t, ctx, s, first.ID, codexIDs, codexIDs[1:])
	if got := audits(first.ID); got != 2 {
		t.Fatalf("first audits = %d", got)
	}
	if got := audits(second.ID); got != 2 {
		t.Fatalf("second audits = %d (fixture plus one change)", got)
	}
	var action, previousMode, mode, reason string
	if err := s.db.QueryRowContext(ctx, `SELECT metadata->>'batch_action',metadata->>'previous_mode',metadata->>'mode',metadata->>'reason'
		FROM audit_events WHERE subject_id=$1 AND event_type='user.upstream_access_changed' AND metadata->>'provider'='antigravity'`, first.ID).
		Scan(&action, &previousMode, &mode, &reason); err != nil || action != "disable" || previousMode != "all" || mode != "selected" || reason != "batch disable" {
		t.Fatalf("batch audit = %q %q %q %q, %v", action, previousMode, mode, reason, err)
	}

	// Enabling extends selected lists only.
	result = batch(UserUpstreamAccessBatchEnable, users, []string{codexIDs[0]}, nil)
	if result.TargetCount != 2 || result.ChangedCount != 2 || result.ConvertedCount != 0 {
		t.Fatalf("enable result = %+v", result)
	}
	get(s, first.ID, "selected", codexIDs...)
	get(s, second.ID, "selected", codexIDs[0], codexIDs[1])

	// Disabling every account leaves an explicit empty deny list.
	batch(UserUpstreamAccessBatchDisable, []string{first.ID}, codexIDs, nil)
	get(s, first.ID, "selected")
	assertUserUpstreamCandidates(t, ctx, s, first.ID, codexIDs, nil)

	rules, err := s.ListUserUpstreamAccessRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]UserUpstreamAccess{}
	for _, rule := range rules {
		if rule.UserID == pending.ID {
			t.Fatalf("pending user rule listed: %+v", rule)
		}
		listed[rule.UserID+"/"+rule.Provider] = rule
	}
	if len(listed) != 4 || listed[first.ID+"/codex"].Mode != "selected" || len(listed[first.ID+"/codex"].AccountIDs) != 0 ||
		!reflect.DeepEqual(listed[second.ID+"/codex"].AccountIDs, []string{codexIDs[0], codexIDs[1]}) {
		t.Fatalf("listed rules = %+v", rules)
	}

	// Failures roll back the whole batch.
	firstAudits := audits(first.ID)
	for _, test := range []struct {
		name   string
		params SetUserUpstreamAccessBatchParams
		want   error
	}{
		{"pending user", SetUserUpstreamAccessBatchParams{UserIDs: []string{first.ID, pending.ID}, Action: UserUpstreamAccessBatchEnable, AccountIDs: []string{codexIDs[0]}}, ErrConflict},
		{"missing user", SetUserUpstreamAccessBatchParams{UserIDs: []string{first.ID, "00000000-0000-0000-0000-000000000001"}, Action: UserUpstreamAccessBatchEnable, AccountIDs: []string{codexIDs[0]}}, ErrConflict},
		{"unknown account", SetUserUpstreamAccessBatchParams{UserIDs: []string{first.ID}, Action: UserUpstreamAccessBatchEnable, AccountIDs: []string{codexIDs[0], "ffffffffffffffff"}}, ErrInvalid},
		{"audit failure", SetUserUpstreamAccessBatchParams{UserIDs: users, Action: UserUpstreamAccessBatchAll, Providers: []string{UpstreamProviderCodex}, SourceIP: "invalid-ip"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.params.Reason, test.params.ActorUserID = "rejected batch", owner.ID
			if _, err := s.SetUserUpstreamAccessBatch(ctx, test.params); err == nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("batch error = %v; want %v", err, test.want)
			}
			get(s, first.ID, "selected")
			get(s, second.ID, "selected", codexIDs[0], codexIDs[1])
			if got := audits(first.ID); got != firstAudits {
				t.Fatalf("rejected batch audited: %d", got)
			}
		})
	}

	// Restoring all only touches selected scopes and is idempotent.
	result = batch(UserUpstreamAccessBatchAll, users, nil, []string{UpstreamProviderCodex, UpstreamProviderAntigravity})
	if result.TargetCount != 4 || result.ChangedCount != 4 || result.ConvertedCount != 0 {
		t.Fatalf("all result = %+v", result)
	}
	for _, userID := range users {
		get(s, userID, "all")
		get(agy, userID, "all")
	}
	assertUserUpstreamCandidates(t, ctx, s, first.ID, codexIDs, codexIDs)
	before := audits(first.ID) + audits(second.ID)
	if result = batch(UserUpstreamAccessBatchAll, users, nil, []string{UpstreamProviderCodex}); result.ChangedCount != 0 {
		t.Fatalf("repeated all changed scopes: %+v", result)
	}
	if result = batch(UserUpstreamAccessBatchEnable, users, []string{codexIDs[2]}, nil); result.ChangedCount != 0 {
		t.Fatalf("enable for all changed scopes: %+v", result)
	}
	if after := audits(first.ID) + audits(second.ID); after != before {
		t.Fatalf("unchanged batch audited: %d -> %d", before, after)
	}
}

func TestUserUpstreamAccessBatchConcurrentEditsPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	// The Owner sorts first, so batch user locks also cover the audit actor.
	owner, err := s.CreateUser(ctx, CreateUserParams{ID: "00000000-0000-0000-0000-000000000001", Username: "batch-lock-owner", Role: UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateUser(ctx, CreateUserParams{ID: "00000000-0000-0000-0000-000000000002", Username: "batch-lock-member", Role: UserRoleMember})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"123456789abcdef0", "23456789abcdef01", "3456789abcdef012"}
	for _, id := range ids {
		if err := s.EnsureUpstreamAccount(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	users := []string{owner.ID, member.ID}
	operations := []func() error{
		func() error {
			_, err := s.SetUserUpstreamAccessBatch(ctx, SetUserUpstreamAccessBatchParams{
				UserIDs: users, Action: UserUpstreamAccessBatchDisable, AccountIDs: ids[:1], Reason: "concurrent disable", ActorUserID: owner.ID,
			})
			return err
		},
		func() error {
			_, err := s.SetUserUpstreamAccessBatch(ctx, SetUserUpstreamAccessBatchParams{
				UserIDs: users, Action: UserUpstreamAccessBatchEnable, AccountIDs: ids[:2], Reason: "concurrent enable", ActorUserID: owner.ID,
			})
			return err
		},
		func() error {
			_, err := s.SetUpstreamAccountAccess(ctx, SetUpstreamAccountAccessParams{
				AccountID: ids[2], Mode: "exclusive", UserIDs: users, Reason: "concurrent exclusive", ActorUserID: owner.ID,
			})
			return err
		},
		func() error {
			_, err := s.SetUserUpstreamAccess(ctx, SetUserUpstreamAccessParams{
				UserID: member.ID, Mode: "selected", AccountIDs: ids[1:], Reason: "concurrent single", ActorUserID: owner.ID,
			})
			return err
		},
	}
	const rounds = 6
	errs := make(chan error, rounds*len(operations))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range rounds {
		for _, operation := range operations {
			wg.Go(func() {
				<-start
				errs <- operation()
			})
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent access edit: %v", err)
		}
	}
	for _, userID := range users {
		got, err := s.GetUserUpstreamAccess(ctx, userID)
		if err != nil || got.Mode == "" {
			t.Fatalf("final access = %+v, %v", got, err)
		}
	}
}
