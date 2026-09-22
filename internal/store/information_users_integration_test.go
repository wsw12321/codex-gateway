//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestInformationUserDeletionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := fmt.Sprint(now.UnixNano())
	var actor User
	var existingID string
	if err := s.DB().QueryRowContext(ctx, `SELECT id FROM users WHERE role='owner' AND status='active'`).Scan(&existingID); err == nil {
		actor, err = s.GetUser(ctx, existingID)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		actor = globalUsageIntegrationUser(t, ctx, s, "delete-owner-"+suffix, UserRoleOwner)
		t.Cleanup(func() {
			_, _ = s.DB().ExecContext(context.Background(), `UPDATE users SET status='disabled',disabled_at=now() WHERE id=$1`, actor.ID)
		})
	}
	deletion := func(ids ...string) DeleteInformationUsersParams {
		return DeleteInformationUsersParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "delete integration", now), UserIDs: ids}
	}
	makeUser := func(label string) (User, Device, APIKey) {
		return billingIntegrationPrincipal(t, ctx, s, "del-"+label+"-"+suffix)
	}

	t.Run("empty account credentials audit group and idempotent replay", func(t *testing.T) {
		u, _, key := makeUser("empty")
		hash := sha256.Sum256([]byte(u.ID))
		session, err := s.CreateSession(ctx, CreateSessionParams{UserID: u.ID, TokenHash: hash[:], CSRFSecret: hash[:], CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO password_credentials(user_id,encoded_hash) VALUES($1,$2)`, u.ID, strings.Repeat("x", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddWebAuthnCredential(ctx, AddWebAuthnCredentialParams{UserID: u.ID, CredentialID: hash[:], CredentialJSON: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO recovery_codes(user_id,batch_id,code_hash) VALUES($1,gen_random_uuid(),$2)`, u.ID, hash[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationRecovery, TokenHash: hash[:], InviterID: actor.ID, TargetUserID: u.ID, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		event, err := s.AppendAuditEvent(ctx, AppendAuditEventParams{ActorUserID: u.ID, ActorSessionID: session.ID, ActorAPIKeyID: key.ID, EventType: "auth.login", Success: true})
		if err != nil {
			t.Fatal(err)
		}
		group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "group test", now), Name: "deletion group", LimitUSD: "10", Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "membership test", now), GroupID: group.ID, UserIDs: []string{u.ID}, Action: "add"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=3 WHERE id=$1`, group.PeriodID); err != nil {
			t.Fatal(err)
		}
		users, err := s.ListDeletableInformationUsers(ctx, u.Username, 100, 0)
		if err != nil || len(users) != 1 || users[0].ID != u.ID {
			t.Fatalf("candidates: %+v %v", users, err)
		}
		if _, err := s.DB().ExecContext(ctx, `DELETE FROM api_key_history WHERE id=$1`, key.ID); err == nil {
			t.Fatal("ordinary history deletion allowed")
		}
		params := deletion(u.ID)
		result, err := s.DeleteInformationUsers(ctx, params)
		if err != nil || result.DeletedCount != 1 {
			t.Fatalf("delete: %+v %v", result, err)
		}
		if _, err := s.GetUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("user survived: %v", err)
		}
		if _, err := s.LookupAPIKey(ctx, key.PublicID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("API credential survived: %v", err)
		}
		if _, err := s.GetActiveSession(ctx, hash[:], now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session survived: %v", err)
		}
		if _, err := s.GetWebAuthnCredential(ctx, hash[:]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Passkey survived: %v", err)
		}
		if _, err := s.GetPasswordCredential(ctx, u.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("password survived: %v", err)
		}
		var credentials int
		if err := s.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM recovery_codes WHERE user_id=$1)+(SELECT count(*) FROM invitations WHERE target_user_id=$1)`, u.ID).Scan(&credentials); err != nil || credentials != 0 {
			t.Fatalf("recovery credentials survived: count=%d err=%v", credentials, err)
		}
		var uid, sid, kid string
		var detached bool
		if err := s.DB().QueryRowContext(ctx, `SELECT actor_user_id IS NULL AND actor_session_id IS NULL AND actor_api_key_id IS NULL,actor_user_id_snapshot,actor_session_id_snapshot,actor_api_key_id_snapshot FROM audit_events WHERE id=$1`, event.ID).Scan(&detached, &uid, &sid, &kid); err != nil {
			t.Fatal(err)
		}
		if !detached || uid != u.ID || sid != session.ID || kid != key.ID {
			t.Fatalf("audit snapshot: %t %s %s %s", detached, uid, sid, kid)
		}
		var used string
		if err := s.DB().QueryRowContext(ctx, `SELECT used_usd::text FROM group_usage_periods WHERE id=$1`, group.PeriodID).Scan(&used); err != nil || used != "3.000000000000" {
			t.Fatalf("group usage changed: %s %v", used, err)
		}
		if _, err := s.DeleteInformationUsers(ctx, params); err != nil {
			t.Fatalf("idempotent replay: %v", err)
		}
		other, _, _ := makeUser("other")
		params.UserIDs = []string{other.ID}
		if _, err := s.DeleteInformationUsers(ctx, params); !errors.Is(err, ErrConflict) {
			t.Fatalf("replay mismatch: %v", err)
		}
	})

	t.Run("recharge after selection rolls entire batch back", func(t *testing.T) {
		u, _, _ := makeUser("credited")
		clean, _, _ := makeUser("rollback")
		users, err := s.ListDeletableInformationUsers(ctx, u.Username, 100, 0)
		if err != nil || len(users) != 1 {
			t.Fatal("initial candidate absent", err)
		}
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "new credit", now), UserID: u.ID, USDAmount: "1"}); err != nil {
			t.Fatal(err)
		}
		params := deletion(clean.ID, u.ID)
		_, err = s.DeleteInformationUsers(ctx, params)
		var blocked *UserDeletionBlockedError
		if !errors.As(err, &blocked) || len(blocked.Blockers) != 1 {
			t.Fatalf("expected detailed rollback: %v", err)
		}
		if _, err := s.GetUser(ctx, clean.ID); err != nil {
			t.Fatalf("clean member partially deleted: %v", err)
		}
		var count int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM information_user_deletions WHERE operation_id=$1`, params.OperationID).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed deletion receipt committed", err)
		}
	})

	t.Run("owner and newly started request are blocked", func(t *testing.T) {
		if _, err := s.DeleteInformationUsers(ctx, deletion(actor.ID)); !errors.Is(err, ErrConflict) {
			t.Fatalf("Owner deletion: %v", err)
		}
		u, d, k := makeUser("request")
		if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{RequestID: "information-request-" + suffix, UserID: u.ID, DeviceID: d.ID, APIKeyID: k.ID, Model: "catalog", Endpoint: "models", RequestedAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteInformationUsers(ctx, deletion(u.ID)); !errors.Is(err, ErrConflict) {
			t.Fatalf("new request deletion: %v", err)
		}
	})

	t.Run("zero balance historical user becomes eligible after cleanup", func(t *testing.T) {
		u, _, _ := makeUser("historical")
		old := now.AddDate(0, 0, -120)
		for _, amount := range []string{"1", "-1"} {
			if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "historical credit", old), UserID: u.ID, USDAmount: amount}); err != nil {
				t.Fatal(err)
			}
		}
		users, err := s.ListDeletableInformationUsers(ctx, u.Username, 100, 0)
		if err != nil || len(users) != 0 {
			t.Fatalf("uncleaned history must block deletion: %+v %v", users, err)
		}
		cutoff, err := InformationCutoff(now, 90)
		if err != nil {
			t.Fatal(err)
		}
		write := billingIntegrationWrite(t, actor.ID, "cleanup before delete", now)
		job, err := s.CreateInformationCleanupJob(ctx, InformationCleanupParams{OperationID: write.OperationID, ActorUserID: actor.ID, RetentionDays: 90, Cutoff: cutoff})
		if err != nil {
			t.Fatal(err)
		}
		for batch := 0; batch < 50 && job.Status != "completed"; batch++ {
			progress, err := s.RunInformationCleanupBatch(ctx, 10)
			if err != nil || progress == nil {
				t.Fatalf("cleanup progress: %+v %v", progress, err)
			}
			job = *progress
		}
		if job.Status != "completed" {
			t.Fatal("cleanup did not finish")
		}
		users, err = s.ListDeletableInformationUsers(ctx, u.Username, 100, 0)
		if err != nil || len(users) != 1 {
			t.Fatalf("cleaned user absent: %+v %v", users, err)
		}
		if _, err := s.DeleteInformationUsers(ctx, deletion(u.ID)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("concurrent credit and request admission cannot escape recheck", func(t *testing.T) {
		for i := 0; i < 6; i++ {
			u, d, k := makeUser(fmt.Sprintf("race%d", i))
			start := make(chan struct{})
			mutation := make(chan error, 1)
			params := deletion(u.ID)
			credit := billingIntegrationWrite(t, actor.ID, "racing credit", now)
			go func() {
				<-start
				var err error
				if i%2 == 0 {
					_, err = s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: credit, UserID: u.ID, USDAmount: "1"})
				} else {
					id := fmt.Sprintf("information-race-%d-%s", i, suffix)
					_, err = s.AdmitRequest(ctx, AdmitRequestParams{Quota: ReserveQuotaParams{RequestID: id, UserID: u.ID, APIKeyID: k.ID, Now: now}, Usage: BeginUsageRequestParams{RequestID: id, UserID: u.ID, DeviceID: d.ID, APIKeyID: k.ID, Model: "catalog", Endpoint: "models", RequestedAt: now}})
				}
				mutation <- err
			}()
			close(start)
			_, deletionErr := s.DeleteInformationUsers(ctx, params)
			mutationErr := <-mutation
			if deletionErr == nil {
				if mutationErr == nil {
					t.Fatal("request or credit committed for a deleted user")
				}
			} else {
				var blocked *UserDeletionBlockedError
				if !errors.As(deletionErr, &blocked) || mutationErr != nil {
					t.Fatalf("unexpected race failure: deletion=%v mutation=%v", deletionErr, mutationErr)
				}
				if _, err := s.GetUser(ctx, u.ID); err != nil {
					t.Fatal("mutation winner was deleted", err)
				}
			}
		}
	})
}
