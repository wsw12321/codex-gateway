//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestGroupRemoveAllPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	billingIntegrationModelScopes(t, ctx, s, "billing-priced-model")
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	owner := globalUsageIntegrationUser(t, ctx, s, "remove-all-owner", UserRoleOwner)
	write := func() BillingWriteParams { return billingIntegrationWrite(t, owner.ID, "remove all integration", now) }
	create := func(name string) Group {
		t.Helper()
		cap := "20"
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: name, LimitUSD: "100", MemberLimitUSD: &cap, Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	add := func(group Group, ids ...string) Group {
		t.Helper()
		g, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: group.ID, Action: "add", UserIDs: ids})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	clear := func(group Group) SetGroupMembersParams {
		return SetGroupMembersParams{BillingWriteParams: write(), GroupID: group.ID, Action: "remove_all"}
	}

	t.Run("preserves accounts usage other groups and in-flight funding", func(t *testing.T) {
		group, other := create("Clear with history"), create("Unaffected group")
		user, device, key := billingIntegrationPrincipal(t, ctx, s, "remove-all-history")
		outsider := globalUsageIntegrationUser(t, ctx, s, "remove-all-outsider", UserRoleMember)
		add(group, user.ID)
		add(other, outsider.ID)
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: user.ID, USDAmount: "5"}); err != nil {
			t.Fatal(err)
		}
		billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, "remove-all-paid-history", now, 2_000_000, "1", "billing-priced-model")
		if _, err := s.SettleBilling(ctx, "remove-all-paid-history", now); err != nil {
			t.Fatal(err)
		}
		billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, "remove-all-in-flight", now, 3_000_000, "1", "billing-priced-model")
		params := clear(group)
		result, err := s.SetGroupMembers(ctx, params)
		if err != nil || result.MemberCount != 0 || result.Members == nil || len(result.Members) != 0 ||
			result.ArchivedAt != nil || result.PeriodID != group.PeriodID || result.UsedUSD != "2.000000000000" ||
			result.LimitUSD != group.LimitUSD || result.MemberLimitUSD == nil || *result.MemberLimitUSD != *group.MemberLimitUSD {
			t.Fatalf("clear result: %+v %v", result, err)
		}
		var balance, used string
		var detached bool
		var users, ledger int
		if err := s.DB().QueryRowContext(ctx, `SELECT a.balance_usd::text,a.group_id IS NULL,
			(SELECT used_usd::text FROM group_member_usage WHERE period_id=$2 AND user_id=$1),
			(SELECT count(*) FROM users WHERE id=$1),
			(SELECT count(*) FROM billing_ledger_entries WHERE request_id='remove-all-paid-history')
			FROM billing_accounts a WHERE a.user_id=$1`, user.ID, group.PeriodID).Scan(&balance, &detached, &used, &users, &ledger); err != nil ||
			balance != "5.000000000000" || !detached || used != "2.000000000000" || users != 1 || ledger != 1 {
			t.Fatalf("preserved data balance=%s detached=%t used=%s users=%d ledger=%d err=%v", balance, detached, used, users, ledger, err)
		}
		if g, err := s.GetGroup(ctx, other.ID); err != nil || g.MemberCount != 1 || g.Members[0].UserID != outsider.ID {
			t.Fatalf("other group changed: %+v %v", g, err)
		}
		personal, err := s.ReserveBilling(ctx, BillingReservationParams{RequestID: "remove-all-personal", UserID: user.ID, APIKeyID: key.ID,
			Model: "billing-priced-model", InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: now})
		if err != nil || personal.GroupID != nil {
			t.Fatalf("removed member still uses group funds: %+v %v", personal, err)
		}
		if err := s.ReleaseBilling(ctx, personal.RequestID, now); err != nil {
			t.Fatal(err)
		}
		settled, err := s.SettleBilling(ctx, "remove-all-in-flight", now)
		if err != nil || settled.GroupPeriodID == nil || *settled.GroupPeriodID != group.PeriodID ||
			settled.GroupChargedUSD == nil || *settled.GroupChargedUSD != "3.000000000000" {
			t.Fatalf("accepted funding binding changed: %+v %v", settled, err)
		}
		rejoined := add(group, user.ID)
		if rejoined.UsedUSD != "5.000000000000" || rejoined.Members[0].UsedUSD != "5.000000000000" {
			t.Fatalf("rejoining restored usage: %+v", rejoined)
		}
		replay, err := s.SetGroupMembers(ctx, params)
		if err != nil || replay.MemberCount != 0 || replay.UsedUSD != result.UsedUSD {
			t.Fatalf("operation did not replay original response: %+v %v", replay, err)
		}
		if g, err := s.GetGroup(ctx, group.ID); err != nil || g.MemberCount != 1 || g.Members[0].UserID != user.ID {
			t.Fatalf("replay removed later member: %+v %v", g, err)
		}
		assertGroupRemoveAllOperationCount(t, ctx, s, params.OperationID, 1)
		params.Reason = "changed request"
		if _, err := s.SetGroupMembers(ctx, params); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed reason did not conflict: %v", err)
		}
	})

	t.Run("more than five thousand members", func(t *testing.T) {
		group := create("Large group")
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO users(username,display_name,webauthn_user_id)
			SELECT 'remove-all-bulk-'||n,'Bulk '||n,decode(md5('remove-all-'||n)||md5('remove-all-'||n),'hex')
			FROM generate_series(1,5001) n`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, `UPDATE billing_accounts SET group_id=$1,balance_usd=1
			WHERE user_id IN (SELECT id FROM users WHERE username LIKE 'remove-all-bulk-%')`, group.ID); err != nil {
			t.Fatal(err)
		}
		result, err := s.SetGroupMembers(ctx, clear(group))
		if err != nil || result.MemberCount != 0 || len(result.Members) != 0 {
			t.Fatalf("large clear: %+v %v", result, err)
		}
		var preserved int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM billing_accounts a JOIN users u ON u.id=a.user_id
			WHERE u.username LIKE 'remove-all-bulk-%' AND a.group_id IS NULL AND a.balance_usd=1`).Scan(&preserved); err != nil || preserved != 5001 {
			t.Fatalf("large group accounts preserved=%d err=%v", preserved, err)
		}
	})

	t.Run("empty archived and missing groups", func(t *testing.T) {
		group := create("Empty clear")
		params := clear(group)
		if g, err := s.SetGroupMembers(ctx, params); err != nil || g.MemberCount != 0 || len(g.Members) != 0 {
			t.Fatalf("empty clear: %+v %v", g, err)
		}
		if _, err := s.ArchiveGroup(ctx, ArchiveGroupParams{BillingWriteParams: write(), GroupID: group.ID}); err != nil {
			t.Fatal(err)
		}
		if g, err := s.SetGroupMembers(ctx, params); err != nil || g.ArchivedAt != nil {
			t.Fatalf("replay after archival changed: %+v %v", g, err)
		}
		archivedParams := clear(group)
		if _, err := s.SetGroupMembers(ctx, archivedParams); !errors.Is(err, ErrConflict) {
			t.Fatalf("archived group accepted clear: %v", err)
		}
		assertGroupRemoveAllOperationCount(t, ctx, s, archivedParams.OperationID, 0)
		missing := clear(Group{GroupSummary: GroupSummary{ID: write().OperationID}})
		if _, err := s.SetGroupMembers(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing group clear: %v", err)
		}
		assertGroupRemoveAllOperationCount(t, ctx, s, missing.OperationID, 0)
	})
}

func TestGroupRemoveAllConcurrentJoinRollsBackPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := globalUsageIntegrationUser(t, ctx, s, "remove-race-owner", UserRoleOwner)
	current := globalUsageIntegrationUser(t, ctx, s, "remove-race-current", UserRoleMember)
	joining := globalUsageIntegrationUser(t, ctx, s, "remove-race-joining", UserRoleMember)
	write := func() BillingWriteParams { return billingIntegrationWrite(t, owner.ID, "concurrent clear", now) }
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Concurrent join", LimitUSD: "10", Period: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: group.ID, Action: "add", UserIDs: []string{current.ID}}); err != nil {
		t.Fatal(err)
	}
	// Pause a joining transaction after its account -> user -> group locks,
	// before its membership update. The clear cannot see its new member yet.
	joiningTx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer joiningTx.Rollback()
	if _, err := joiningTx.ExecContext(ctx, `SELECT user_id FROM billing_accounts WHERE user_id=$1 FOR UPDATE`, joining.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := joiningTx.ExecContext(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, joining.ID); err != nil {
		t.Fatal(err)
	}
	if err := lockGroupTx(ctx, joiningTx, group.ID); err != nil {
		t.Fatal(err)
	}
	var joiningPID int
	if err := joiningTx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&joiningPID); err != nil {
		t.Fatal(err)
	}
	params := SetGroupMembersParams{BillingWriteParams: write(), GroupID: group.ID, Action: "remove_all"}
	done := make(chan error, 1)
	go func() {
		_, err := s.SetGroupMembers(ctx, params)
		done <- err
	}()
	waitGroupRemoveAllBlocked(t, ctx, s, done, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
		WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT id FROM user_groups%')`, joiningPID)
	if _, err := joiningTx.ExecContext(ctx, `UPDATE billing_accounts SET group_id=$2 WHERE user_id=$1`, joining.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if err := joiningTx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("multi-group remove-all failed: %v", err)
	}
	if g, err := s.GetGroup(ctx, group.ID); err != nil || g.MemberCount != 0 {
		t.Fatalf("remove-all did not clear membership: %+v %v", g, err)
	}
	assertGroupRemoveAllOperationCount(t, ctx, s, params.OperationID, 1)
	if g, err := s.SetGroupMembers(ctx, params); err != nil || g.MemberCount != 0 {
		t.Fatalf("rolled-back operation could not retry: %+v %v", g, err)
	}
	assertGroupRemoveAllOperationCount(t, ctx, s, params.OperationID, 1)
}

func TestGroupRemoveAllOwnerMemberLockOrderPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := globalUsageIntegrationUser(t, ctx, s, "remove-lock-owner", UserRoleOwner)
	write := func() BillingWriteParams { return billingIntegrationWrite(t, owner.ID, "owner member lock order", now) }
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write(), Name: "Owner member", LimitUSD: "10", Period: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: group.ID, Action: "add", UserIDs: []string{owner.ID}}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"add", "remove", "remove_all"} {
		t.Run(action, func(t *testing.T) {
			blockedWrite := write()
			barrier, err := s.DB().Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Close()
			if _, err := barrier.ExecContext(ctx, `SELECT pg_advisory_lock(314159,271828)`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, _ = barrier.ExecContext(context.Background(), `SELECT pg_advisory_unlock(314159,271828)`)
			}()
			// Hold a real groupWrite after its actor FK lock, before account locks.
			// The operation under test must finish while that KEY SHARE is held.
			trigger := fmt.Sprintf(`CREATE FUNCTION hold_remove_all_actor_fk() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.operation_id = '%s' THEN
					PERFORM 1 FROM users WHERE id=NEW.actor_user_id FOR KEY SHARE;
					PERFORM pg_advisory_xact_lock(314159,271828);
				END IF;
				RETURN NEW;
			END $$;
			CREATE TRIGGER hold_remove_all_actor_fk AFTER INSERT ON group_operations
			FOR EACH ROW EXECUTE FUNCTION hold_remove_all_actor_fk()`, blockedWrite.OperationID)
			if _, err := s.DB().ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				_, _ = barrier.ExecContext(cleanupCtx, `SELECT pg_advisory_unlock(314159,271828)`)
				_, _ = s.DB().ExecContext(cleanupCtx, `DROP TRIGGER hold_remove_all_actor_fk ON group_operations; DROP FUNCTION hold_remove_all_actor_fk()`)
			}()
			blockedDone := make(chan error, 1)
			go func() {
				_, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: blockedWrite, GroupID: group.ID, Action: "add", UserIDs: []string{owner.ID}})
				blockedDone <- err
			}()
			waitGroupRemoveAllBlocked(t, ctx, s, blockedDone, `SELECT EXISTS(SELECT 1 FROM pg_locks
				WHERE locktype='advisory' AND classid=314159 AND objid=271828 AND NOT granted)`)
			params := SetGroupMembersParams{BillingWriteParams: write(), GroupID: group.ID, Action: action}
			if action != "remove_all" {
				params.UserIDs = []string{owner.ID}
			}
			operationCtx, operationCancel := context.WithTimeout(ctx, 3*time.Second)
			defer operationCancel()
			if _, err := s.SetGroupMembers(operationCtx, params); err != nil {
				t.Fatalf("member operation waited on actor FK: %v", err)
			}
			if _, err := barrier.ExecContext(ctx, `SELECT pg_advisory_unlock(314159,271828)`); err != nil {
				t.Fatal(err)
			}
			if err := <-blockedDone; err != nil {
				t.Fatalf("blocked operation did not finish: %v", err)
			}
		})
	}
}

func assertGroupRemoveAllOperationCount(t *testing.T, ctx context.Context, s *Store, operationID string, want int) {
	t.Helper()
	var operations, audits int
	if err := s.DB().QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM group_operations WHERE operation_id=$1),
		(SELECT count(*) FROM audit_events WHERE event_type='group.members.remove_all' AND metadata->>'operation_id'=$1)`, operationID).Scan(&operations, &audits); err != nil || operations != want || audits != want {
		t.Fatalf("operation records=%d audit records=%d want=%d err=%v", operations, audits, want, err)
	}
}

func waitGroupRemoveAllBlocked(t *testing.T, ctx context.Context, s *Store, done <-chan error, query string, args ...any) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := s.DB().QueryRowContext(ctx, query, args...).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("operation finished before lock barrier: %v", err)
		case <-ctx.Done():
			t.Fatal("operation did not reach lock barrier")
		case <-ticker.C:
		}
	}
}
