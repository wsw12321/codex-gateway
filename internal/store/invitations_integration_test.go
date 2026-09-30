//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestBatchInvitationsPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := fmt.Sprint(now.UnixNano())
	actor := globalUsageIntegrationUser(t, ctx, s, "inviter-"+suffix, UserRoleMember)
	hash := func(label string) []byte { v := sha256.Sum256([]byte(label + suffix)); return v[:] }
	session := func(label string) CreateSessionParams {
		return CreateSessionParams{TokenHash: hash("session-" + label), CSRFSecret: hash("csrf-" + label), CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}
	}
	registration := func(i Invitation, name string, passkey bool) CompleteInvitationParams {
		params := CompleteInvitationParams{InvitationHash: i.TokenHash, User: CreateUserParams{Username: name + "-" + suffix, DisplayName: name}, Credential: IdentityCredential{EncodedPasswordHash: strings.Repeat("p", 80)}, Session: session(name)}
		if passkey {
			params.Credential = IdentityCredential{WebAuthnCredential: &AddWebAuthnCredentialParams{CredentialID: hash("credential-" + name), CredentialJSON: []byte(`{}`)}}
		}
		for n := range 10 {
			params.RecoveryHashes = append(params.RecoveryHashes, hash(fmt.Sprintf("recovery-%s-%d", name, n)))
		}
		return params
	}
	invitation := func(label, kind string, capacity int, approval bool, group string) Invitation {
		i, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: kind, TokenHash: hash(label), InviterID: actor.ID, MaxUses: capacity, RequiresApproval: approval, GroupID: group, ExpiresAt: now.Add(72 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	applications := func(i Invitation) []InvitationApplication {
		a, err := s.ListInvitationApplications(ctx, i.ID, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	review := func(i Invitation, decision string, ids ...string) (int, error) {
		return s.ReviewInvitationApplications(ctx, i.ID, ids, decision, actor.ID, now)
	}
	assertCount := func(i Invitation, want int) {
		if got := len(applications(i)); got != want {
			t.Fatalf("applications = %d, want %d", got, want)
		}
	}
	group := func(name string) Group {
		g, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, name, now), Name: name, LimitUSD: "10", Period: "day"})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	currentGroup := func(u User) *string {
		var id *string
		if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM billing_accounts WHERE user_id=$1`, u.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("concurrent last slot and failed identity rollback", func(t *testing.T) {
		i := invitation("last-slot", InvitationMember, 1, false, "")
		bad := registration(i, "rollback", false)
		bad.RecoveryHashes[1] = bad.RecoveryHashes[0]
		if _, _, err := s.CompleteInvitationRegistration(ctx, bad); err == nil {
			t.Fatal("duplicate recovery hash must fail")
		}
		if _, err := s.GetUserByUsername(ctx, bad.User.Username); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rolled-back identity: %v", err)
		}
		assertCount(i, 0)
		const competitors = 12
		errs := make(chan error, competitors)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for n := range competitors {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				<-start
				_, _, err := s.CompleteInvitationRegistration(ctx, registration(i, fmt.Sprintf("race%d", n), false))
				errs <- err
			}(n)
		}
		close(start)
		wg.Wait()
		close(errs)
		success := 0
		for err := range errs {
			if err == nil {
				success++
			} else if !errors.Is(err, ErrInvitationUnavailable) {
				t.Fatalf("race: %v", err)
			}
		}
		if inspected, err := s.InspectInvitation(ctx, i.TokenHash, now); err != nil || inspected.UsedCount != 1 {
			t.Fatalf("inspect full invitation: %+v %v", inspected, err)
		}
		if success != 1 {
			t.Fatalf("successes = %d", success)
		}
		assertCount(i, 1)
	})

	t.Run("pending password and passkey require approval without sessions", func(t *testing.T) {
		for _, passkey := range []bool{false, true} {
			label := fmt.Sprintf("pending-%t", passkey)
			i := invitation(label, InvitationMember, 2, true, "")
			p := registration(i, label, passkey)
			user, createdSession, err := s.CompleteInvitationRegistration(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if user.Status != StatusPending || createdSession.ID != "" {
				t.Fatalf("pending registration = %+v, session %s", user, createdSession.ID)
			}
			var counts struct{ sessions, recovery, password, passkey int }
			if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM sessions WHERE user_id=$1),(SELECT count(*) FROM recovery_codes WHERE user_id=$1),(SELECT count(*) FROM password_credentials WHERE user_id=$1),(SELECT count(*) FROM webauthn_credentials WHERE user_id=$1)`, user.ID).Scan(&counts.sessions, &counts.recovery, &counts.password, &counts.passkey); err != nil {
				t.Fatal(err)
			}
			if counts.sessions != 0 || counts.recovery != 10 || counts.password+counts.passkey != 1 {
				t.Fatalf("pending artifacts: %+v", counts)
			}
			directSession := session(label + "-direct-session")
			directSession.UserID = user.ID
			if _, err := s.CreateSession(ctx, directSession); !errors.Is(err, ErrNotFound) {
				t.Fatalf("pending session creation: %v", err)
			}
			_, _, err = s.CompletePasswordLogin(ctx, CompletePasswordLoginParams{UserID: user.ID, ExpectedHash: p.Credential.EncodedPasswordHash, Session: session(label + "-login"), At: now})
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("pending login: %v", err)
			}
			_, _, err = s.CompleteAccountRecovery(ctx, CompleteRecoveryParams{UserID: user.ID, RecoveryHash: p.RecoveryHashes[0], Credential: p.Credential, RecoveryHashes: p.RecoveryHashes, Session: session(label + "-recovery"), At: now})
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("pending recovery: %v", err)
			}
			a := applications(i)[0]
			if err := s.RevokeInvitation(ctx, i.ID, actor.ID, now); err != nil {
				t.Fatal(err)
			}
			if _, err := review(i, "approve", a.ID); err != nil {
				t.Fatalf("approval after revoke: %v", err)
			}
			user, err = s.GetUser(ctx, user.ID)
			if err != nil || user.Status != StatusActive {
				t.Fatalf("approved user: %+v %v", user, err)
			}
			if n, err := review(i, "approve", a.ID); err != nil || n != 0 {
				t.Fatalf("replayed approval: %d %v", n, err)
			}
			if !passkey {
				_, loggedIn, err := s.CompletePasswordLogin(ctx, CompletePasswordLoginParams{UserID: user.ID, ExpectedHash: p.Credential.EncodedPasswordHash, Session: session(label + "-approved-login"), At: now})
				if err != nil || loggedIn.ID == "" {
					t.Fatalf("approved login: %v", err)
				}
			}
		}
	})

	t.Run("registration rejection erases identity and old ids cannot affect reapply", func(t *testing.T) {
		i := invitation("reject-registration", InvitationMember, 1, true, "")
		p := registration(i, "reusable-name", false)
		u, _, err := s.CompleteInvitationRegistration(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		old := applications(i)[0]
		if n, err := review(i, "reject", old.ID); err != nil || n != 1 {
			t.Fatalf("reject: %d %v", n, err)
		}
		var remains int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM users WHERE id=$1)+(SELECT count(*) FROM billing_accounts WHERE user_id=$1)+(SELECT count(*) FROM user_model_access WHERE user_id=$1)+(SELECT count(*) FROM recovery_codes WHERE user_id=$1)+(SELECT count(*) FROM password_credentials WHERE user_id=$1)+(SELECT count(*) FROM invitation_applications WHERE user_id=$1)`, u.ID).Scan(&remains); err != nil || remains != 0 {
			t.Fatalf("rejection artifacts = %d: %v", remains, err)
		}
		assertCount(i, 0)
		next, _, err := s.CompleteInvitationRegistration(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		fresh := applications(i)[0]
		if fresh.ID == old.ID || next.ID == u.ID {
			t.Fatal("reapplication reused identity")
		}
		if n, err := review(i, "reject", old.ID); err != nil || n != 0 {
			t.Fatalf("old reject: %d %v", n, err)
		}
		if _, err := review(i, "approve", old.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("old approve: %v", err)
		}
		assertCount(i, 1)
		if _, err := review(i, "reject", fresh.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("group purpose duplicate replay conflicts and archive", func(t *testing.T) {
		g := group("Invite group " + suffix)
		other := group("Other invite group " + suffix)
		i := invitation("join", InvitationGroup, 3, false, g.ID)
		if _, _, err := s.CompleteInvitationRegistration(ctx, registration(i, "group-register", false)); !errors.Is(err, ErrInvitationUnavailable) {
			t.Fatalf("group allowed registration: %v", err)
		}
		if _, err := s.CreateUserFromInvitation(ctx, i.TokenHash, CreateUserParams{Username: "group-helper-" + suffix}); !errors.Is(err, ErrInvitationUnavailable) {
			t.Fatalf("group allowed helper registration: %v", err)
		}
		u := globalUsageIntegrationUser(t, ctx, s, "group-join-"+suffix, UserRoleMember)
		a, err := s.JoinInvitation(ctx, i.TokenHash, u.ID)
		if err != nil || a.Status != "approved" {
			t.Fatalf("join: %+v %v", a, err)
		}
		same, err := s.JoinInvitation(ctx, i.TokenHash, u.ID)
		if err != nil || same.ID != a.ID {
			t.Fatalf("duplicate: %+v %v", same, err)
		}
		assertCount(i, 1)
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "remove invite member", now), GroupID: g.ID, Action: "remove", UserIDs: []string{u.ID}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.JoinInvitation(ctx, i.TokenHash, u.ID); err != nil {
			t.Fatal(err)
		}
		if currentGroup(u) != nil {
			t.Fatal("old approved application rejoined removed member")
		}
		existing := globalUsageIntegrationUser(t, ctx, s, "existing-join-"+suffix, UserRoleMember)
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "seed target member", now), GroupID: g.ID, Action: "add", UserIDs: []string{existing.ID}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.JoinInvitation(ctx, i.TokenHash, existing.ID); err != nil {
			t.Fatal(err)
		}
		assertCount(i, 1)
		outsider := globalUsageIntegrationUser(t, ctx, s, "other-join-"+suffix, UserRoleMember)
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "seed other member", now), GroupID: other.ID, Action: "add", UserIDs: []string{outsider.ID}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.JoinInvitation(ctx, i.TokenHash, outsider.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("cross group: %v", err)
		}
		pending := invitation("pending-group", InvitationGroup, 1, true, g.ID)
		applicant := globalUsageIntegrationUser(t, ctx, s, "pending-join-"+suffix, UserRoleMember)
		application, err := s.JoinInvitation(ctx, pending.TokenHash, applicant.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "empty archived group", now), GroupID: g.ID, Action: "remove", UserIDs: []string{existing.ID}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ArchiveGroup(ctx, ArchiveGroupParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "archive invitation group", now), GroupID: g.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := review(pending, "approve", application.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("archived approval: %v", err)
		}
		if _, err := review(pending, "reject", application.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetUser(ctx, applicant.ID); err != nil {
			t.Fatalf("rejection removed existing user: %v", err)
		}
		assertCount(pending, 0)
	})

	t.Run("review and invitation creation share actor lock order", func(t *testing.T) {
		g := group("Concurrent invite actor " + suffix)
		i := invitation("actor-lock-order", InvitationGroup, 1, true, g.ID)
		member := globalUsageIntegrationUser(t, ctx, s, "actor-lock-member-"+suffix, UserRoleMember)
		a, err := s.JoinInvitation(ctx, i.TokenHash, member.ID)
		if err != nil {
			t.Fatal(err)
		}
		blocker, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		if err := lockGroupTx(ctx, blocker, g.ID); err != nil {
			t.Fatal(err)
		}
		var blockerPID int
		if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
			t.Fatal(err)
		}
		reviewed := make(chan error, 1)
		go func() { _, err := review(i, "approve", a.ID); reviewed <- err }()
		waitForBlocked := func(pids ...int) int {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			excludedPID := 0
			if len(pids) > 1 {
				excludedPID = pids[len(pids)-1]
			}
			for time.Now().Before(deadline) {
				for _, pid := range pids {
					var waiting int
					err := s.db.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND pid<>$2 LIMIT 1`, pid, excludedPID).Scan(&waiting)
					if err == nil {
						return waiting
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Fatal("concurrent operation did not reach expected lock wait")
			return 0
		}
		reviewerPID := waitForBlocked(blockerPID)
		created := make(chan error, 1)
		go func() {
			_, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationGroup, TokenHash: hash("actor-lock-created"), InviterID: actor.ID, GroupID: g.ID, ExpiresAt: now.Add(time.Hour)})
			created <- err
		}()
		waitForBlocked(blockerPID, reviewerPID)
		if err := blocker.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-reviewed; err != nil {
			t.Fatalf("concurrent review: %v", err)
		}
		if err := <-created; err != nil {
			t.Fatalf("concurrent invitation creation: %v", err)
		}
	})

	t.Run("batch review is atomic when one member conflicts", func(t *testing.T) {
		g := group("Batch target " + suffix)
		other := group("Batch conflict " + suffix)
		i := invitation("batch-review", InvitationGroup, 2, true, g.ID)
		one := globalUsageIntegrationUser(t, ctx, s, "batch-one-"+suffix, UserRoleMember)
		two := globalUsageIntegrationUser(t, ctx, s, "batch-two-"+suffix, UserRoleMember)
		a, err := s.JoinInvitation(ctx, i.TokenHash, one.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.JoinInvitation(ctx, i.TokenHash, two.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "conflict batch member", now), GroupID: other.ID, Action: "add", UserIDs: []string{two.ID}}); err != nil {
			t.Fatal(err)
		}
		if n, err := review(i, "approve", a.ID, b.ID); !errors.Is(err, ErrConflict) || n != 0 {
			t.Fatalf("conflicting batch: %d %v", n, err)
		}
		if currentGroup(one) != nil {
			t.Fatal("failed batch partially joined member")
		}
		for _, a := range applications(i) {
			if a.Status != "pending" {
				t.Fatal("failed batch partially approved application")
			}
		}
		if n, err := review(i, "reject", a.ID, b.ID); err != nil || n != 2 {
			t.Fatalf("batch reject: %d %v", n, err)
		}
		assertCount(i, 0)
	})
}

func TestInvitationMigrationBackfillsUsesPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connection, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*connection)
	defer admin.Close()
	id, _ := newUUID()
	schema := "invitation_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	}()
	connection.RuntimeParams["search_path"] = schema
	s := New(stdlib.OpenDB(*connection))
	defer s.Close()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.Name >= "0026" {
			break
		}
		if _, err := s.db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
	}
	user := globalUsageIntegrationUser(t, ctx, s, "legacy-invited", UserRoleMember)
	token := sha256.Sum256([]byte("legacy used invitation"))
	var invitationID string
	if err := s.db.QueryRowContext(ctx, `INSERT INTO invitations(kind,token_hash,inviter_id,expires_at,used_at,used_by_user_id)
  VALUES('member',$1,$2,now()+interval '1 hour',now(),$2) RETURNING id`, token[:], user.ID).Scan(&invitationID); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.Name >= "0026" {
			if _, err := s.db.ExecContext(ctx, m.SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	apps, err := s.ListInvitationApplications(ctx, invitationID, 50, 0)
	if err != nil || len(apps) != 1 || apps[0].Status != "approved" || apps[0].Username != user.Username {
		t.Fatalf("legacy applications: %+v %v", apps, err)
	}
	if _, err := s.GetAvailableInvitation(ctx, token[:], time.Now()); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("legacy invitation reusable: %v", err)
	}
	if err := s.RevokeInvitation(ctx, invitationID, user.ID, time.Now()); err != nil {
		t.Fatalf("revoke migrated invitation: %v", err)
	}
	bootstrapHash := sha256.Sum256([]byte("bootstrap compatibility"))
	bootstrap, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationOwnerBootstrap, TokenHash: bootstrapHash[:]})
	if err != nil || bootstrap.MaxUses != 1 || bootstrap.RequiresApproval {
		t.Fatalf("bootstrap defaults: %+v %v", bootstrap, err)
	}
	owner, err := s.CreateUserFromInvitation(ctx, bootstrapHash[:], CreateUserParams{Username: "bootstrap-owner"})
	if err != nil || owner.Role != UserRoleOwner || owner.Status != StatusActive {
		t.Fatalf("bootstrap user: %+v %v", owner, err)
	}
	if _, err := s.GetAvailableInvitation(ctx, bootstrapHash[:], time.Now()); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("bootstrap reuse: %v", err)
	}
	recoveryHash := sha256.Sum256([]byte("recovery compatibility"))
	recovery, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationRecovery, TokenHash: recoveryHash[:], InviterID: owner.ID, TargetUserID: user.ID})
	if err != nil || recovery.MaxUses != 1 {
		t.Fatalf("recovery defaults: %+v %v", recovery, err)
	}
	if _, err := s.CreateUserFromInvitation(ctx, recoveryHash[:], CreateUserParams{Username: "recovery-wrong-purpose"}); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("recovery used for signup: %v", err)
	}
	if _, err := s.ConsumeInvitation(ctx, recoveryHash[:], owner.ID, time.Now()); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("wrong recovery target: %v", err)
	}
	if _, err := s.ConsumeInvitation(ctx, recoveryHash[:], user.ID, time.Now()); err != nil {
		t.Fatalf("consume recovery: %v", err)
	}
	if _, err := s.ConsumeInvitation(ctx, recoveryHash[:], user.ID, time.Now()); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("recovery reuse: %v", err)
	}
}
