//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPendingUserBusinessGuardsPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := globalUsageIntegrationUser(t, ctx, s, "pending-guards-owner", UserRoleOwner)
	if err := s.SyncModelAccessCatalog(ctx, []string{"test-model"}); err != nil {
		t.Fatal(err)
	}
	u := globalUsageIntegrationUser(t, ctx, s, "pending-guards-member", UserRoleMember)
	if _, err := s.DB().ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "guard test", now), Name: "pending guards", LimitUSD: "10", Period: "day"})
	if err != nil {
		t.Fatal(err)
	}
	accountID := upstreamIntegrationAccountID("pending-guards")
	if err := s.EnsureUpstreamAccount(ctx, accountID, now); err != nil {
		t.Fatal(err)
	}
	write := func() BillingWriteParams { return billingIntegrationWrite(t, owner.ID, "pending guard", now) }
	hash := sha256.Sum256([]byte(u.ID))
	checks := []struct {
		name string
		call func() error
	}{
		{"recharge", func() error {
			_, err := s.RechargeUser(ctx, RechargeUserParams{BillingWriteParams: write(), UserID: u.ID, CNYAmount: "1"})
			return err
		}},
		{"balance adjustment", func() error {
			_, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: write(), UserID: u.ID, USDAmount: "1"})
			return err
		}},
		{"subscription", func() error {
			_, err := s.PutSubscription(ctx, PutSubscriptionParams{BillingWriteParams: write(), UserID: u.ID, Tier: BillingTierDay, AllowanceUSD: "1"})
			return err
		}},
		{"subscription deletion", func() error {
			_, err := s.DeleteSubscription(ctx, DeleteSubscriptionParams{BillingWriteParams: write(), UserID: u.ID, Tier: BillingTierDay})
			return err
		}},
		{"group membership", func() error {
			_, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write(), GroupID: group.ID, UserIDs: []string{u.ID}, Action: "add"})
			return err
		}},
		{"model permission", func() error {
			_, err := s.SetUserModelAccess(ctx, SetUserModelAccessParams{ModelAccessWriteParams: ModelAccessWriteParams{ActorUserID: owner.ID, Reason: "pending guard", At: now}, Model: "test-model", Scope: ModelAccessScopeSelected, UserIDs: []string{u.ID}, Enabled: false})
			return err
		}},
		{"upstream permission", func() error {
			_, err := s.SetUpstreamAccountAccess(ctx, SetUpstreamAccountAccessParams{ActorUserID: owner.ID, AccountID: accountID, Mode: "exclusive", UserIDs: []string{u.ID}, Reason: "pending guard", At: now})
			return err
		}},
		{"recovery invitation", func() error {
			_, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationRecovery, TokenHash: hash[:], InviterID: owner.ID, TargetUserID: u.ID, ExpiresAt: now.Add(time.Hour)})
			return err
		}},
		{"device", func() error {
			_, err := s.CreateDevice(ctx, CreateDeviceParams{UserID: u.ID, Name: "pending device"})
			return err
		}},
		{"project", func() error {
			_, err := s.CreateProject(ctx, CreateProjectParams{UserID: u.ID, Slug: "pending", Name: "pending project"})
			return err
		}},
		{"API key", func() error {
			_, err := s.CreateAPIKey(ctx, CreateAPIKeyParams{UserID: u.ID, DeviceID: owner.ID, Name: "pending key", PublicID: "pending-key", KeyPrefix: "pending-key", KeyHash: hash[:], SecretCiphertext: []byte("ciphertext"), CreatedAt: now})
			return err
		}},
		{"billing preference", func() error {
			return s.SetBillingSourceDisabled(ctx, SetBillingSourceDisabledParams{UserID: u.ID, Source: "cash", Disabled: true, At: now})
		}},
		{"ordinary deletion", func() error {
			_, err := s.DeleteInformationUsers(ctx, DeleteInformationUsersParams{BillingWriteParams: write(), UserIDs: []string{u.ID}})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.call()
			if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInvalid) {
				t.Fatalf("pending business operation error = %v", err)
			}
		})
	}
	if users, err := s.ListBillingUsers(ctx); err != nil || len(users) != 1 || users[0].UserID != owner.ID {
		t.Fatalf("billing list leaked pending account: %+v %v", users, err)
	}
	if _, err := s.GetBillingState(ctx, u.ID, 50, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending billing state: %v", err)
	}
	if users, err := s.ListDeletableInformationUsers(ctx, u.Username, 50, 0); err != nil || len(users) != 0 {
		t.Fatalf("deletion list leaked pending account: %+v %v", users, err)
	}
	if users, err := s.GlobalUsage(ctx, now.Add(-time.Hour), now.Add(time.Hour), "", false, time.Time{}); err != nil || len(users) != 1 || users[0].UserID != owner.ID {
		t.Fatalf("global usage leaked pending account: %+v %v", users, err)
	}
	if users, err := s.ListModelAccessUsers(ctx, "test-model"); err != nil || len(users) != 1 || users[0].UserID != owner.ID {
		t.Fatalf("model list leaked pending account: %+v %v", users, err)
	}
	if users, err := s.ListModelAccessUsersBatch(ctx, []string{"test-model"}); err != nil || len(users) != 1 || users[0].UserID != owner.ID {
		t.Fatalf("batch model list leaked pending account: %+v %v", users, err)
	}
	if models, err := s.ListModelAccessModels(ctx); err != nil || len(models) != 1 || models[0].EnabledUserCount != 1 {
		t.Fatalf("model counts included pending account: %+v %v", models, err)
	}
	if _, err := s.ListEnabledModelsForUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending account can list models: %v", err)
	}
	if err := s.RequireModelAccess(ctx, u.ID, "test-model"); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("pending account can use model: %v", err)
	}
	all, err := s.SetUserModelAccess(ctx, SetUserModelAccessParams{ModelAccessWriteParams: ModelAccessWriteParams{ActorUserID: owner.ID, Reason: "active accounts only", At: now.Add(time.Second)}, Model: "test-model", Scope: ModelAccessScopeAll, Enabled: false})
	if err != nil || all.TargetCount != 1 || all.ChangedCount != 1 {
		t.Fatalf("all-users operation included pending account: %+v %v", all, err)
	}
	var enabled bool
	if err := s.DB().QueryRowContext(ctx, `SELECT enabled FROM user_model_access WHERE user_id=$1 AND model='test-model'`, u.ID).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("pending permission snapshot changed: enabled=%v err=%v", enabled, err)
	}
	var records int
	if err := s.DB().QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM billing_ledger_entries WHERE user_id=$1) +
		(SELECT count(*) FROM billing_operations WHERE target_user_id=$1) +
		(SELECT count(*) FROM billing_subscriptions WHERE user_id=$1) +
		(SELECT count(*) FROM billing_cash_credit_lots WHERE user_id=$1) +
		(SELECT count(*) FROM devices WHERE user_id=$1) +
		(SELECT count(*) FROM projects WHERE user_id=$1) +
		(SELECT count(*) FROM api_key_history WHERE user_id=$1) +
		(SELECT count(*) FROM upstream_account_users WHERE user_id=$1) +
		(SELECT count(*) FROM invitations WHERE target_user_id=$1)`, u.ID).Scan(&records); err != nil || records != 0 {
		t.Fatalf("pending user acquired business records: count=%d err=%v", records, err)
	}
}

func TestInformationUserDeletionPreservesInvitationCapacityPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := globalUsageIntegrationUser(t, ctx, s, "invitation-delete-owner", UserRoleOwner)
	u := globalUsageIntegrationUser(t, ctx, s, "invitation-delete-member", UserRoleMember)
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "deletion invitations", now), Name: "application deletion", LimitUSD: "10", Period: "day"})
	if err != nil {
		t.Fatal(err)
	}
	var approvedID, pendingID, memberInvitationID, groupInvitationID string
	for _, kind := range []string{InvitationMember, InvitationGroup} {
		hash := sha256.Sum256([]byte(kind))
		params := CreateInvitationParams{Kind: kind, TokenHash: hash[:], InviterID: owner.ID, ExpiresAt: now.Add(time.Hour), RequiresApproval: true, MaxUses: 2}
		status := "approved"
		if kind == InvitationGroup {
			params.GroupID = group.ID
			status = "pending"
		}
		invitation, err := s.CreateInvitation(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		if err := s.DB().QueryRowContext(ctx, `INSERT INTO invitation_applications
			(invitation_id,user_id,username,display_name,registered_at,applied_at,status,reviewed_at,reviewed_by)
			VALUES($1,$2,$3,$4,$5,$5,$6,CASE WHEN $6='approved' THEN $5::timestamptz END,CASE WHEN $6='approved' THEN $7::uuid END)
			RETURNING id`, invitation.ID, u.ID, u.Username, u.DisplayName, now, status, owner.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if kind == InvitationMember {
			approvedID, memberInvitationID = id, invitation.ID
		} else {
			pendingID, groupInvitationID = id, invitation.ID
		}
	}
	if _, err := s.DeleteInformationUsers(ctx, DeleteInformationUsersParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "delete former applicant", now), UserIDs: []string{u.ID}}); err != nil {
		t.Fatal(err)
	}
	var detached bool
	var username string
	if err := s.DB().QueryRowContext(ctx, `SELECT user_id IS NULL,username FROM invitation_applications WHERE id=$1 AND status='approved'`, approvedID).Scan(&detached, &username); err != nil || !detached || username != u.Username {
		t.Fatalf("approved application snapshot was lost: detached=%v username=%s err=%v", detached, username, err)
	}
	var count int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM invitation_applications WHERE id=$1`, pendingID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pending group application retained: count=%d err=%v", count, err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM invitations WHERE id IN ($1,$2)`, memberInvitationID, groupInvitationID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("user deletion removed batch invitation: count=%d err=%v", count, err)
	}
}

func TestInvitationGroupReviewSerializesMembershipAndDeletionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := globalUsageIntegrationUser(t, ctx, s, "group-review-race-owner", UserRoleOwner)
	groups := make([]Group, 2)
	for i := range groups {
		var err error
		groups[i], err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "review race", now), Name: fmt.Sprintf("review race %d", i), LimitUSD: "10", Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256.Sum256([]byte("group review races"))
	invitation, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationGroup, TokenHash: hash[:], InviterID: owner.ID, ExpiresAt: now.Add(time.Hour), GroupID: groups[0].ID, RequiresApproval: true, MaxUses: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		u := globalUsageIntegrationUser(t, ctx, s, fmt.Sprintf("group-review-race-%d", i), UserRoleMember)
		application, err := s.JoinInvitation(ctx, hash[:], u.ID)
		if err != nil || application.Status != "pending" {
			t.Fatalf("apply: %+v %v", application, err)
		}
		start := make(chan struct{})
		reviewDone := make(chan error, 1)
		go func() {
			<-start
			_, err := s.ReviewInvitationApplications(ctx, invitation.ID, []string{application.ID}, "approve", owner.ID, now)
			reviewDone <- err
		}()
		write := billingIntegrationWrite(t, owner.ID, "competing membership", now)
		close(start)
		_, membershipErr := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write, GroupID: groups[1].ID, UserIDs: []string{u.ID}, Action: "add"})
		reviewErr := <-reviewDone
		if (reviewErr == nil) == (membershipErr == nil) || (reviewErr != nil && !errors.Is(reviewErr, ErrConflict)) || (membershipErr != nil && !errors.Is(membershipErr, ErrConflict)) {
			t.Fatalf("expected one winner: approval=%v manual membership=%v", reviewErr, membershipErr)
		}
		var actualGroup, actualStatus string
		if err := s.DB().QueryRowContext(ctx, `SELECT b.group_id,a.status FROM billing_accounts b JOIN invitation_applications a ON a.user_id=b.user_id WHERE a.id=$1`, application.ID).Scan(&actualGroup, &actualStatus); err != nil {
			t.Fatal(err)
		}
		if reviewErr == nil && (actualGroup != groups[0].ID || actualStatus != "approved") || membershipErr == nil && (actualGroup != groups[1].ID || actualStatus != "pending") {
			t.Fatalf("mixed membership result: group=%s status=%s", actualGroup, actualStatus)
		}
	}
	for i := range 8 {
		u := globalUsageIntegrationUser(t, ctx, s, fmt.Sprintf("group-delete-race-%d", i), UserRoleMember)
		application, err := s.JoinInvitation(ctx, hash[:], u.ID)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		reviewDone := make(chan error, 1)
		go func() {
			<-start
			_, err := s.ReviewInvitationApplications(ctx, invitation.ID, []string{application.ID}, "approve", owner.ID, now)
			reviewDone <- err
		}()
		write := billingIntegrationWrite(t, owner.ID, "delete competing applicant", now)
		close(start)
		_, deletionErr := s.DeleteInformationUsers(ctx, DeleteInformationUsersParams{BillingWriteParams: write, UserIDs: []string{u.ID}})
		reviewErr := <-reviewDone
		if deletionErr != nil || reviewErr != nil && !errors.Is(reviewErr, ErrConflict) {
			t.Fatalf("review/deletion race: approval=%v deletion=%v", reviewErr, deletionErr)
		}
		var approved, pending int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status='approved' AND user_id IS NULL),count(*) FILTER (WHERE status='pending') FROM invitation_applications WHERE id=$1`, application.ID).Scan(&approved, &pending); err != nil {
			t.Fatal(err)
		}
		if pending != 0 || reviewErr == nil && approved != 1 || reviewErr != nil && approved != 0 {
			t.Fatalf("review/deletion lost capacity snapshot: approved=%d pending=%d review=%v", approved, pending, reviewErr)
		}
	}
}

func TestInvitationReviewDoesNotInvertGroupOperationActorFKPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := globalUsageIntegrationUser(t, ctx, s, "group-fk-owner", UserRoleOwner)
	user := globalUsageIntegrationUser(t, ctx, s, "group-fk-member", UserRoleMember)
	groups := make([]Group, 2)
	for i := range groups {
		var err error
		groups[i], err = s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "actor FK race", now), Name: fmt.Sprintf("actor FK %d", i), LimitUSD: "10", Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256.Sum256([]byte("actor FK invitation"))
	invitation, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationGroup, TokenHash: hash[:], InviterID: owner.ID, ExpiresAt: now.Add(time.Hour), GroupID: groups[0].ID, RequiresApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	application, err := s.JoinInvitation(ctx, hash[:], user.ID)
	if err != nil {
		t.Fatal(err)
	}
	write := billingIntegrationWrite(t, owner.ID, "manual membership FK race", now)
	lockKey := int32(now.UnixNano() & 0x7fffffff)
	barrier, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	if _, err := barrier.ExecContext(ctx, `SELECT pg_advisory_lock(271828,$1)`, lockKey); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = barrier.ExecContext(context.Background(), `SELECT pg_advisory_unlock(271828,$1)`, lockKey)
	}()
	// Hold the real manual-member transaction after its actor FK lock, before
	// its target account lock. Review must not wait for that actor key-share
	// lock while already owning the target account needed by the manual write.
	trigger := fmt.Sprintf(`CREATE FUNCTION hold_invitation_group_operation() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.operation_id = '%s' THEN
			PERFORM 1 FROM users WHERE id=NEW.actor_user_id FOR KEY SHARE;
			PERFORM pg_advisory_xact_lock(271828,%d);
		END IF;
		RETURN NEW;
	END $$;
	CREATE TRIGGER hold_invitation_group_operation AFTER INSERT ON group_operations
	FOR EACH ROW EXECUTE FUNCTION hold_invitation_group_operation()`, write.OperationID, lockKey)
	if _, err := s.DB().ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	manualDone := make(chan error, 1)
	go func() {
		_, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: write, GroupID: groups[1].ID, UserIDs: []string{user.ID}, Action: "add"})
		manualDone <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := s.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=271828 AND objid=$1::oid AND NOT granted)`, lockKey).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-manualDone:
			t.Fatalf("manual mutation did not reach FK barrier: %v", err)
		case <-ctx.Done():
			t.Fatal("manual mutation did not reach FK barrier")
		case <-ticker.C:
		}
	}
	reviewDone := make(chan error, 1)
	go func() {
		_, err := s.ReviewInvitationApplications(ctx, invitation.ID, []string{application.ID}, "approve", owner.ID, now)
		reviewDone <- err
	}()
	select {
	case err := <-reviewDone:
		if err != nil {
			t.Fatalf("review failed while manual operation held actor FK: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("review waits on manual operation's actor FK while holding its target account")
	}
	if _, err := barrier.ExecContext(ctx, `SELECT pg_advisory_unlock(271828,$1)`, lockKey); err != nil {
		t.Fatal(err)
	}
	if err := <-manualDone; !errors.Is(err, ErrConflict) {
		t.Fatalf("manual mutation should observe approved membership: %v", err)
	}
}
