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

func assertUserUpstreamCandidates(t *testing.T, ctx context.Context, s *Store, userID string, candidates, want []string) {
	t.Helper()
	want = append([]string{}, want...)
	sort.Strings(want)
	got, err := s.EligibleUpstreamAccounts(ctx, userID, candidates)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s eligible = %v, %v; want %v", s.upstreamProviderName(), got, err, want)
	}
	limits, err := s.EligibleUpstreamAccountLimits(ctx, userID, candidates)
	if err != nil {
		t.Fatal(err)
	}
	limitIDs := make([]string, 0, len(limits))
	for _, limit := range limits {
		limitIDs = append(limitIDs, limit.ID)
		if limit.ConcurrentLimit != 1 {
			t.Fatalf("unexpected concurrent limit: %+v", limit)
		}
	}
	if !reflect.DeepEqual(limitIDs, want) {
		t.Fatalf("eligible limits = %v; want %v", limitIDs, want)
	}
	selected, err := s.SelectUpstreamAccount(ctx, userID, candidates, time.Now())
	if len(want) == 0 {
		if !errors.Is(err, ErrNoUpstreamAccount) || selected != "" {
			t.Fatalf("denied allocation = %q, %v", selected, err)
		}
		return
	}
	if err != nil || sort.SearchStrings(want, selected) == len(want) || want[sort.SearchStrings(want, selected)] != selected {
		t.Fatalf("allocation = %q, %v; want one of %v", selected, err, want)
	}
}

func TestUserUpstreamAccessPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	owner := globalUsageIntegrationUser(t, ctx, s, "user-access-owner", UserRoleOwner)
	member := usageSummaryUser(t, ctx, s, "user-access-member")
	other := usageSummaryUser(t, ctx, s, "user-access-other")
	at := time.Now().UTC()
	for _, provider := range []string{UpstreamProviderCodex, UpstreamProviderAntigravity} {
		t.Run(provider, func(t *testing.T) {
			repository := s.WithUpstreamProvider(provider)
			first := upstreamIntegrationAccountID(provider + "-first")
			second := upstreamIntegrationAccountID(provider + "-second")
			exclusive := upstreamIntegrationAccountID(provider + "-exclusive")
			unknown := upstreamIntegrationAccountID(provider + "-unknown")
			for _, id := range []string{first, second, exclusive} {
				// Unavailable registered accounts may still be selected by an Owner.
				if err := repository.EnsureUpstreamAccount(ctx, id, at); err != nil {
					t.Fatal(err)
				}
			}
			get := func(userID, mode string, ids ...string) {
				t.Helper()
				want := append([]string{}, ids...)
				sort.Strings(want)
				got, err := repository.GetUserUpstreamAccess(ctx, userID)
				if err != nil || got.UserID != userID || got.Provider != provider || got.Mode != mode || !reflect.DeepEqual(got.AccountIDs, want) {
					t.Fatalf("get access = %+v, %v; want %s %v", got, err, mode, want)
				}
			}
			set := func(userID, mode string, ids ...string) {
				t.Helper()
				got, err := repository.SetUserUpstreamAccess(ctx, SetUserUpstreamAccessParams{
					UserID: userID, Mode: mode, AccountIDs: ids, Reason: "test scope", ActorUserID: owner.ID, At: at,
				})
				if err != nil || got.Mode != mode || got.Provider != provider || got.AccountIDs == nil {
					t.Fatalf("set access = %+v, %v", got, err)
				}
				get(userID, mode, ids...)
			}
			candidates := []string{first, second, exclusive, unknown}
			get(member.ID, "all")
			var ruleCount int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM user_upstream_access WHERE user_id=$1 AND provider=$2`, member.ID, provider).Scan(&ruleCount); err != nil || ruleCount != 0 {
				t.Fatalf("default access backfilled a rule: %d, %v", ruleCount, err)
			}
			assertUserUpstreamCandidates(t, ctx, repository, member.ID, candidates, candidates)
			set(member.ID, "selected", second, first)
			assertUserUpstreamCandidates(t, ctx, repository, member.ID, candidates, []string{first, second})
			assertUserUpstreamCandidates(t, ctx, repository, other.ID, candidates, candidates)
			// The other provider remains at its previous configuration.
			otherProvider := UpstreamProviderAntigravity
			if provider == UpstreamProviderAntigravity {
				otherProvider = UpstreamProviderCodex
			}
			if otherAccess, err := s.WithUpstreamProvider(otherProvider).GetUserUpstreamAccess(ctx, member.ID); err != nil || otherAccess.Mode != "all" {
				t.Fatalf("provider scope leaked: %+v, %v", otherAccess, err)
			}
			// Adding an account leaves selected scopes unchanged; all includes it.
			if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: unknown, DisplayName: "new", Status: "available"}}, at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			get(member.ID, "selected", first, second)
			assertUserUpstreamCandidates(t, ctx, repository, member.ID, candidates, []string{first, second})
			assertUserUpstreamCandidates(t, ctx, repository, other.ID, candidates, candidates)
			// An empty snapshot marks accounts unavailable without removing scope.
			if err := repository.SyncUpstreamAccounts(ctx, nil, at.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			get(member.ID, "selected", first, second)
			set(member.ID, "selected")
			assertUserUpstreamCandidates(t, ctx, repository, member.ID, candidates, nil)
			set(member.ID, "all")
			assertUserUpstreamCandidates(t, ctx, repository, member.ID, candidates, candidates)
			// Scope and account-exclusive grants are an intersection in both directions.
			if _, err := repository.SetUpstreamAccountAccess(ctx, SetUpstreamAccountAccessParams{
				AccountID: exclusive, Mode: "exclusive", UserIDs: []string{other.ID}, Reason: "exclusive grant", ActorUserID: owner.ID,
			}); err != nil {
				t.Fatal(err)
			}
			set(member.ID, "selected", first, exclusive)
			assertUserUpstreamCandidates(t, ctx, repository, member.ID, candidates, []string{first})
			set(other.ID, "selected", first)
			assertUserUpstreamCandidates(t, ctx, repository, other.ID, candidates, []string{first})
			// Owners issuing ordinary requests receive no account-scope exemption.
			set(owner.ID, "selected")
			assertUserUpstreamCandidates(t, ctx, repository, owner.ID, candidates, nil)
			set(owner.ID, "all")
			assertUserUpstreamCandidates(t, ctx, repository, owner.ID, candidates, []string{first, second, unknown})
			// Zero-weight bindings survive only while the user still authorizes them.
			if _, err := repository.SetUpstreamAccountAllocationWeight(ctx, SetUpstreamAccountAllocationWeightParams{
				AccountID: first, Weight: 0, ActorUserID: owner.ID,
			}); err != nil {
				t.Fatal(err)
			}
			if eligible, err := repository.EligibleUpstreamAccounts(ctx, member.ID, []string{first}); err != nil || len(eligible) != 1 {
				t.Fatalf("authorized zero-weight binding denied: %v, %v", eligible, err)
			}
			if limits, err := repository.EligibleUpstreamAccountLimits(ctx, member.ID, []string{first}); err != nil || len(limits) != 1 {
				t.Fatalf("authorized zero-weight limits denied: %v, %v", limits, err)
			}
			if _, err := repository.SelectUpstreamAccount(ctx, member.ID, []string{first}, at); !errors.Is(err, ErrNoUpstreamAccount) {
				t.Fatalf("zero weight newly allocated: %v", err)
			}
			set(member.ID, "selected", second)
			assertUserUpstreamCandidates(t, ctx, repository, member.ID, []string{first}, nil)
			// Restore all so the subsequent provider test can assert isolation.
			set(member.ID, "all")
		})
	}
}

func TestUserUpstreamAccessValidationRollbackAndDeletionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	owner := globalUsageIntegrationUser(t, ctx, s, "access-validation-owner", UserRoleOwner)
	member := usageSummaryUser(t, ctx, s, "access-validation-member")
	pending := usageSummaryUser(t, ctx, s, "access-validation-pending")
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, pending.ID); err != nil {
		t.Fatal(err)
	}
	account := upstreamIntegrationAccountID("user-scope-validation")
	agyAccount := upstreamIntegrationAccountID("user-scope-validation-agy")
	if err := s.EnsureUpstreamAccount(ctx, account, time.Now()); err != nil {
		t.Fatal(err)
	}
	agy := s.WithUpstreamProvider(UpstreamProviderAntigravity)
	if err := agy.EnsureUpstreamAccount(ctx, agyAccount, time.Now()); err != nil {
		t.Fatal(err)
	}
	base := SetUserUpstreamAccessParams{UserID: member.ID, Mode: "selected", AccountIDs: []string{account}, Reason: "scope validation", ActorUserID: owner.ID}
	if _, err := s.SetUserUpstreamAccess(ctx, base); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*SetUserUpstreamAccessParams)
		want error
	}{
		{"unknown account", func(p *SetUserUpstreamAccessParams) { p.AccountIDs = []string{"ffffffffffffffff"} }, ErrInvalid},
		{"other provider account", func(p *SetUserUpstreamAccessParams) { p.AccountIDs = []string{agyAccount} }, ErrInvalid},
		{"missing user", func(p *SetUserUpstreamAccessParams) { p.UserID = "00000000-0000-0000-0000-000000000001" }, ErrNotFound},
		{"pending user", func(p *SetUserUpstreamAccessParams) { p.UserID = pending.ID }, ErrConflict},
		{"audit failure", func(p *SetUserUpstreamAccessParams) { p.Mode, p.AccountIDs, p.SourceIP = "all", nil, "invalid-ip" }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := base
			tc.edit(&params)
			_, err := s.SetUserUpstreamAccess(ctx, params)
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("invalid write error = %v; want %v", err, tc.want)
			}
			got, err := s.GetUserUpstreamAccess(ctx, member.ID)
			if err != nil || got.Mode != "selected" || !reflect.DeepEqual(got.AccountIDs, []string{account}) {
				t.Fatalf("invalid write changed access: %+v, %v", got, err)
			}
		})
	}
	var audits int
	var provider, previousMode, mode, reason string
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE subject_id=$1 AND event_type='user.upstream_access_changed'`, member.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count = %d, %v", audits, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT metadata->>'provider',metadata->>'previous_mode',metadata->>'mode',metadata->>'reason'
		FROM audit_events WHERE subject_id=$1 AND event_type='user.upstream_access_changed'`, member.ID).Scan(&provider, &previousMode, &mode, &reason); err != nil || provider != "codex" || previousMode != "all" || mode != "selected" || reason != base.Reason {
		t.Fatalf("audit metadata = %q %q %q %q, %v", provider, previousMode, mode, reason, err)
	}
	if _, err := s.GetUserUpstreamAccess(ctx, pending.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending get = %v", err)
	}
	if _, err := s.GetUserUpstreamAccess(ctx, "00000000-0000-0000-0000-000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get = %v", err)
	}
	// An audited write failure must not turn an explicit empty deny into all.
	base.AccountIDs = nil
	if _, err := s.SetUserUpstreamAccess(ctx, base); err != nil {
		t.Fatal(err)
	}
	failedAll := base
	failedAll.Mode, failedAll.SourceIP = "all", "invalid-ip"
	if _, err := s.SetUserUpstreamAccess(ctx, failedAll); err == nil {
		t.Fatal("audit failure accepted")
	}
	assertUserUpstreamCandidates(t, ctx, s, member.ID, []string{account}, nil)
	base.AccountIDs = []string{account}
	if _, err := s.SetUserUpstreamAccess(ctx, base); err != nil {
		t.Fatal(err)
	}
	agyParams := base
	agyParams.AccountIDs = []string{agyAccount}
	if _, err := agy.SetUserUpstreamAccess(ctx, agyParams); err != nil {
		t.Fatal(err)
	}
	for _, repository := range []*Store{s, agy} {
		got, err := repository.GetUserUpstreamAccess(ctx, member.ID)
		want := account
		if repository.upstreamProviderName() == UpstreamProviderAntigravity {
			want = agyAccount
		}
		if err != nil || got.Mode != "selected" || !reflect.DeepEqual(got.AccountIDs, []string{want}) {
			t.Fatalf("independently saved provider changed: %+v, %v", got, err)
		}
	}
	// Disabled users remain administrable; their request authorization stays denied.
	if err := s.DisableUser(ctx, member.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUserUpstreamAccess(ctx, base); err != nil {
		t.Fatalf("disabled user access cannot be administered: %v", err)
	}
	assertUserUpstreamCandidates(t, ctx, s, member.ID, []string{account}, nil)
	// User deletion removes both providers' rules and associations through FKs.
	if _, err := s.DeleteInformationUsers(ctx, DeleteInformationUsersParams{
		BillingWriteParams: billingIntegrationWrite(t, owner.ID, "delete access fixture", time.Now()), UserIDs: []string{member.ID},
	}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM user_upstream_access WHERE user_id=$1)+
		(SELECT count(*) FROM user_upstream_access_accounts WHERE user_id=$1)`, member.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("deleted user access remains: %d, %v", remaining, err)
	}
}

func TestUserUpstreamAccessConcurrentReplacementPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	owner := globalUsageIntegrationUser(t, ctx, s, "access-concurrent-owner", UserRoleOwner)
	member := usageSummaryUser(t, ctx, s, "access-concurrent-member")
	var lists [][]string
	for batch := range 2 {
		var ids []string
		for item := range 2 {
			id := upstreamIntegrationAccountID(fmt.Sprintf("access-concurrent-%d-%d", batch, item))
			if err := s.EnsureUpstreamAccount(ctx, id, time.Now()); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		sort.Strings(ids)
		lists = append(lists, ids)
	}
	const writes = 12
	errs := make(chan error, writes)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writes {
		wg.Go(func() {
			<-start
			_, err := s.SetUserUpstreamAccess(ctx, SetUserUpstreamAccessParams{
				UserID: member.ID, Mode: "selected", AccountIDs: lists[i%2], Reason: "concurrent replace", ActorUserID: owner.ID,
			})
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent replacement: %v", err)
		}
	}
	got, err := s.GetUserUpstreamAccess(ctx, member.ID)
	if err != nil || got.Mode != "selected" || (!reflect.DeepEqual(got.AccountIDs, lists[0]) && !reflect.DeepEqual(got.AccountIDs, lists[1])) {
		t.Fatalf("concurrent lists mixed: %+v, %v", got, err)
	}
	var total, fromAll, fromSelected int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*),
		count(*) FILTER (WHERE metadata->>'previous_mode'='all' AND metadata->>'previous_account_count'='0'),
		count(*) FILTER (WHERE metadata->>'previous_mode'='selected' AND metadata->>'previous_account_count'='2')
		FROM audit_events WHERE subject_id=$1 AND event_type='user.upstream_access_changed'`, member.ID).Scan(&total, &fromAll, &fromSelected); err != nil || total != writes || fromAll != 1 || fromSelected != writes-1 {
		t.Fatalf("edits were not serialized: total=%d all=%d selected=%d, %v", total, fromAll, fromSelected, err)
	}
}

func TestUserUpstreamAccessDatabaseFailureDeniesPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	user := usageSummaryUser(t, ctx, s, "access-database-failure")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{UpstreamProviderCodex, UpstreamProviderAntigravity} {
		repository := s.WithUpstreamProvider(provider)
		ids := []string{"0123456789abcdef"}
		if got, err := repository.EligibleUpstreamAccounts(ctx, user.ID, ids); err == nil || len(got) != 0 {
			t.Fatalf("closed database eligibility = %v, %v", got, err)
		}
		if got, err := repository.EligibleUpstreamAccountLimits(ctx, user.ID, ids); err == nil || len(got) != 0 {
			t.Fatalf("closed database limits = %v, %v", got, err)
		}
		if got, err := repository.SelectUpstreamAccount(ctx, user.ID, ids, time.Now()); err == nil || got != "" {
			t.Fatalf("closed database allocation = %q, %v", got, err)
		}
	}
}

func TestUserUpstreamAccessAndExclusiveGrantAvoidAuditDeadlockPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	// Account authorization locks its users in ID order. Put the audit actor
	// first so it reaches the member while already holding the Owner lock.
	owner, err := s.CreateUser(ctx, CreateUserParams{ID: "00000000-0000-0000-0000-000000000001", Username: "access-lock-owner", Role: UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateUser(ctx, CreateUserParams{ID: "00000000-0000-0000-0000-000000000002", Username: "access-lock-member", Role: UserRoleMember})
	if err != nil {
		t.Fatal(err)
	}
	const scopeAccount, exclusiveAccount = "123456789abcdef0", "23456789abcdef01"
	for _, id := range []string{scopeAccount, exclusiveAccount} {
		if err := s.EnsureUpstreamAccount(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// Pause the real scope transaction after it locks the member and before its
	// audit insertion. A second real edit can then lock Owner and wait on member.
	const pauseLock int64 = 0x555053434f5045
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION test_pause_user_upstream_scope() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$;
		CREATE TRIGGER test_pause_user_upstream_scope BEFORE INSERT ON user_upstream_access
		FOR EACH ROW EXECUTE FUNCTION test_pause_user_upstream_scope()`, pauseLock)); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pauseLock); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	waitForBlocked := func(pid int) int {
		t.Helper()
		waitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting int
			if err := s.db.QueryRowContext(waitCtx, `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) LIMIT 1`, pid).Scan(&waiting); err == nil {
				return waiting
			}
			select {
			case <-waitCtx.Done():
				t.Fatal("concurrent account edit did not reach expected lock wait")
			case <-ticker.C:
			}
		}
	}
	scopeDone := make(chan error, 1)
	go func() {
		_, err := s.SetUserUpstreamAccess(ctx, SetUserUpstreamAccessParams{
			UserID: member.ID, Mode: "selected", AccountIDs: []string{scopeAccount}, ActorUserID: owner.ID, Reason: "concurrent scope edit",
		})
		scopeDone <- err
	}()
	scopePID := waitForBlocked(blockerPID)
	exclusiveDone := make(chan error, 1)
	go func() {
		_, err := s.SetUpstreamAccountAccess(ctx, SetUpstreamAccountAccessParams{
			AccountID: exclusiveAccount, Mode: "exclusive", UserIDs: []string{owner.ID, member.ID}, ActorUserID: owner.ID, Reason: "concurrent exclusive grant",
		})
		exclusiveDone <- err
	}()
	waitForBlocked(scopePID)
	// The compatible audit lock must not weaken protection from concurrent
	// non-key status updates or deletion/key changes while users are validated.
	for _, lockMode := range []string{"NO KEY UPDATE", "UPDATE"} {
		var locked string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 FOR `+lockMode+` NOWAIT`, owner.ID).Scan(&locked)
		var state interface{ SQLState() string }
		if !errors.As(err, &state) || state.SQLState() != "55P03" {
			t.Fatalf("validated user allowed %s lock: %v", lockMode, err)
		}
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	scopeErr, exclusiveErr := <-scopeDone, <-exclusiveDone
	if scopeErr != nil || exclusiveErr != nil {
		t.Fatalf("concurrent access edits failed: scope=%v exclusive=%v", scopeErr, exclusiveErr)
	}
	assertUserUpstreamCandidates(t, ctx, s, member.ID, []string{scopeAccount, exclusiveAccount}, []string{scopeAccount})
}
