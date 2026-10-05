//go:build integration

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

func profileString(value string) *string { return &value }

func TestProfileAtomicUpdateAndOwnershipPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "profile-original")
	session := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
	if err := s.SetPassword(ctx, user.ID, session.ID, strings.Repeat("x", 80), at); err != nil {
		t.Fatal(err)
	}
	passkeyHash := sha256.Sum256([]byte(user.ID))
	credential, err := s.AddWebAuthnCredential(ctx, AddWebAuthnCredentialParams{UserID: user.ID, CredentialID: passkeyHash[:], CredentialJSON: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	binding := externalIdentityTestLink(t, ctx, s, user.ID, session.ID, "profile-subject", at)
	recoveryHash := sha256.Sum256([]byte("profile-recovery-code"))
	recoveryHashes := make([][]byte, 10)
	for i := range recoveryHashes {
		hash := sha256.Sum256([]byte{byte(i)})
		recoveryHashes[i] = hash[:]
	}
	recoveryHashes[0] = recoveryHash[:]
	if _, err := s.ReplaceRecoveryCodes(ctx, user.ID, recoveryHashes); err != nil {
		t.Fatal(err)
	}
	invitationHash := sha256.Sum256([]byte("profile-recovery-invitation"))
	invitation, err := s.CreateInvitation(ctx, CreateInvitationParams{Kind: InvitationRecovery, TokenHash: invitationHash[:], InviterID: user.ID, TargetUserID: user.ID, ExpiresAt: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var originalBalance string
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&originalBalance); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, CreateUserParams{Username: "profile-taken", DisplayName: "Public Name"})
	if err != nil {
		t.Fatal(err)
	}
	params := UpdateUserProfileParams{UserID: user.ID, SessionID: session.ID, Username: profileString(other.Username), DisplayName: profileString("Should roll back")}
	if _, err := s.UpdateUserProfile(ctx, params); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("username conflict = %v", err)
	}
	after, err := s.GetUser(ctx, user.ID)
	if err != nil || after.Username != user.Username || after.DisplayName != user.DisplayName {
		t.Fatalf("partial update: %+v, %v", after, err)
	}
	params.Username = profileString("profile-renamed")
	params.DisplayName = profileString(other.DisplayName)
	params.SourceIP = "invalid-ip"
	if _, err := s.UpdateUserProfile(ctx, params); err == nil {
		t.Fatal("invalid audit data unexpectedly committed")
	}
	after, err = s.GetUser(ctx, user.ID)
	if err != nil || after.Username != user.Username || after.DisplayName != user.DisplayName {
		t.Fatalf("audit failure did not roll back: %+v, %v", after, err)
	}
	params.SourceIP = ""
	updated, err := s.UpdateUserProfile(ctx, params)
	if err != nil || updated.ID != user.ID || updated.Username != "profile-renamed" || updated.DisplayName != other.DisplayName || !bytes.Equal(updated.WebAuthnUserID, user.WebAuthnUserID) {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if _, err := s.GetUserByUsername(ctx, user.Username); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old username still aliases original: %v", err)
	}
	reused, err := s.CreateUser(ctx, CreateUserParams{Username: user.Username, DisplayName: other.DisplayName})
	if err != nil || reused.ID == user.ID {
		t.Fatalf("old username not reusable: %+v, %v", reused, err)
	}
	if current, err := s.GetActiveSession(ctx, session.TokenHash, at); err != nil || current.UserID != user.ID {
		t.Fatalf("session ownership: %+v, %v", current, err)
	}
	if current, err := s.GetPasswordCredential(ctx, user.ID); err != nil || current.EncodedHash != strings.Repeat("x", 80) {
		t.Fatalf("password ownership: %+v, %v", current, err)
	}
	if current, err := s.GetWebAuthnCredential(ctx, credential.CredentialID); err != nil || current.UserID != user.ID {
		t.Fatalf("passkey ownership: %+v, %v", current, err)
	}
	if current, err := s.LookupAPIKey(ctx, key.PublicID); err != nil || current.UserID != user.ID || current.DeviceID != device.ID {
		t.Fatalf("API key ownership: %+v, %v", current, err)
	}
	if current, err := s.GetExternalIdentity(ctx, user.ID); err != nil || current.ID != binding.ID {
		t.Fatalf("SSO ownership: %+v, %v", current, err)
	}
	if _, err := s.GetUnusedRecoveryCode(ctx, user.ID, recoveryHash[:]); err != nil {
		t.Fatalf("recovery code lost: %v", err)
	}
	if _, err := s.GetUnusedRecoveryCode(ctx, reused.ID, recoveryHash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recovery code followed old name: %v", err)
	}
	if current, err := s.GetAvailableInvitation(ctx, invitationHash[:], at); err != nil || current.ID != invitation.ID || current.TargetUserID == nil || *current.TargetUserID != user.ID {
		t.Fatalf("recovery invitation ownership: %+v, %v", current, err)
	}
	var balance string
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&balance); err != nil || balance != originalBalance {
		t.Fatalf("billing ownership: %q, %v", balance, err)
	}
	events, err := s.ListAuditEvents(ctx, AuditFilter{ActorUserID: user.ID, EventType: "identity.profile_updated", Limit: 10})
	if err != nil || len(events) != 1 || events[0].SubjectID != user.ID || events[0].Metadata["username_changed"] != true || events[0].Metadata["display_name_changed"] != true {
		t.Fatalf("profile audit: %+v, %v", events, err)
	}
}

func TestProfileConcurrentNamesAndReservedNamesPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	users := make([]User, 2)
	sessions := make([]Session, 2)
	for i, name := range []string{"profile-racer-one", "profile-racer-two"} {
		var err error
		users[i], err = s.CreateUser(ctx, CreateUserParams{Username: name, DisplayName: name})
		if err != nil {
			t.Fatal(err)
		}
		sessions[i] = externalIdentityTestLocalSession(t, ctx, s, users[i].ID, at)
	}
	start := make(chan struct{})
	type result struct {
		index int
		err   error
	}
	results := make(chan result, 2)
	for i := range users {
		go func(i int) {
			<-start
			_, err := s.UpdateUserProfile(ctx, UpdateUserProfileParams{UserID: users[i].ID, SessionID: sessions[i].ID, Username: profileString("profile-contested"), DisplayName: profileString("Saved")})
			results <- result{i, err}
		}(i)
	}
	close(start)
	winners := 0
	for range users {
		got := <-results
		if got.err == nil {
			winners++
			continue
		}
		if !errors.Is(got.err, ErrUsernameTaken) {
			t.Fatalf("unexpected race error: %v", got.err)
		}
		after, err := s.GetUser(ctx, users[got.index].ID)
		if err != nil || after.Username != users[got.index].Username || after.DisplayName != users[got.index].DisplayName {
			t.Fatalf("race loser partially updated: %+v, %v", after, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d", winners)
	}
	for _, status := range []string{StatusDisabled, StatusPending} {
		reserved, err := s.CreateUser(ctx, CreateUserParams{Username: "profile-" + status, DisplayName: status})
		if err != nil {
			t.Fatal(err)
		}
		if status == StatusDisabled {
			err = s.DisableUser(ctx, reserved.ID, at)
		} else {
			_, err = s.db.ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, reserved.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateUserProfile(ctx, UpdateUserProfileParams{UserID: users[0].ID, SessionID: sessions[0].ID, Username: &reserved.Username}); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("%s username not reserved: %v", status, err)
		}
	}
}

func TestProfileLegacyNameAndSessionAuthorizationPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	user, err := s.CreateUser(ctx, CreateUserParams{Username: "water5_" + strings.Repeat("a", 32), DisplayName: "Old"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateSession(ctx, externalIdentityTestSession(t, user.ID, at))
	if err != nil {
		t.Fatal(err)
	}
	params := UpdateUserProfileParams{UserID: user.ID, SessionID: session.ID, DisplayName: profileString("新的显示名称")}
	if got, err := s.UpdateUserProfile(ctx, params); err != nil || got.Username != user.Username || got.DisplayName != *params.DisplayName {
		t.Fatalf("legacy display-only edit: %+v, %v", got, err)
	}
	params.Username = profileString("new-valid-name")
	for _, verified := range []any{nil, at.Add(-5 * time.Minute), at.Add(time.Second)} {
		if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET recently_verified_at=$2 WHERE id=$1`, session.ID, verified); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateUserProfile(ctx, params); !errors.Is(err, ErrProfileVerificationRequired) {
			t.Fatalf("verification %v allowed rename: %v", verified, err)
		}
	}
	other, err := s.CreateUser(ctx, CreateUserParams{Username: "profile-other-session", DisplayName: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	params.SessionID = externalIdentityTestLocalSession(t, ctx, s, other.ID, at).ID
	params.Username = nil
	if _, err := s.UpdateUserProfile(ctx, params); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign session accepted: %v", err)
	}
	params.SessionID = session.ID
	if err := s.RevokeSession(ctx, session.ID, "profile-test", at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateUserProfile(ctx, params); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked session accepted: %v", err)
	}
}

func TestProfileAuthorizationExpiresBeforeCommitPostgresIntegration(t *testing.T) {
	for _, test := range []struct {
		name     string
		username *string
		delay    time.Duration
		want     error
	}{
		{name: "username proof expires", username: profileString("profile-after-delay"), delay: 5 * time.Minute, want: ErrProfileVerificationRequired},
		{name: "display session expires", delay: time.Hour, want: ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s := informationIntegrationStore(t, ctx)
			at := time.Now().UTC().Truncate(time.Microsecond)
			user, err := s.CreateUser(ctx, CreateUserParams{Username: "profile-before-delay", DisplayName: "Before delay"})
			if err != nil {
				t.Fatal(err)
			}
			session := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
			checks := 0
			s.now = func() time.Time {
				checks++
				if checks == 1 {
					return at
				}
				return at.Add(test.delay)
			}
			_, err = s.UpdateUserProfile(ctx, UpdateUserProfileParams{UserID: user.ID, SessionID: session.ID,
				Username: test.username, DisplayName: profileString("Must roll back")})
			if !errors.Is(err, test.want) || checks != 2 {
				t.Fatalf("delayed update error=%v want=%v authorization checks=%d", err, test.want, checks)
			}
			after, err := s.GetUser(ctx, user.ID)
			if err != nil || after.Username != user.Username || after.DisplayName != user.DisplayName {
				t.Fatalf("expired authorization committed profile: %+v, %v", after, err)
			}
			events, err := s.ListAuditEvents(ctx, AuditFilter{ActorUserID: user.ID, EventType: "identity.profile_updated", Limit: 10})
			if err != nil || len(events) != 0 {
				t.Fatalf("expired authorization committed audit: %+v, %v", events, err)
			}
		})
	}
}
