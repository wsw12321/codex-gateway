//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func externalRegistrationParams(t *testing.T, subject string, at time.Time) CompleteExternalRegistrationParams {
	t.Helper()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	return CompleteExternalRegistrationParams{Issuer: externalIdentityTestIssuer, Subject: subject,
		Username: "sso_" + strings.ReplaceAll(id, "-", "")[:24], DisplayName: "新会员",
		MaskedEmail: "u***@example.test", At: at, VerifiedAt: at, Session: externalIdentityTestSession(t, "", at)}
}

func externalUserCount(t *testing.T, ctx context.Context, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestExternalRegistrationPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_access_defaults(model,enabled,catalog_active)
		VALUES ('sso-test-allowed',true,true),('sso-test-denied',false,true),('sso-test-retired',true,false)`); err != nil {
		t.Fatal(err)
	}
	params := externalRegistrationParams(t, "first-user", at)
	params.VerifiedAt = at.Add(-6 * time.Minute)
	user, session, err := s.CompleteExternalRegistration(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != UserRoleMember || user.Status != StatusActive || user.Username != params.Username || user.DisplayName != params.DisplayName ||
		user.ID != session.UserID || session.ExternalIdentityID == nil || session.RecentlyVerifiedAt == nil ||
		!session.RecentlyVerifiedAt.Equal(params.VerifiedAt) || session.RecentlyVerifiedAt.After(at.Add(-5*time.Minute)) {
		t.Fatalf("wrong registration defaults: user=%+v session=%+v", user, session)
	}
	methods, err := s.LoginMethods(ctx, user.ID)
	if err != nil || !methods.OIDC || methods.Passkey || methods.Password {
		t.Fatalf("login methods=%+v err=%v", methods, err)
	}
	var zeroBalance, defaultModels bool
	var groups, credentials int
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd=0,
		(SELECT count(*) FROM user_model_access WHERE user_id=$1 AND model='sso-test-allowed' AND enabled)=1
		AND (SELECT count(*) FROM user_model_access WHERE user_id=$1 AND model='sso-test-denied' AND NOT enabled)=1
		AND (SELECT count(*) FROM user_model_access WHERE user_id=$1 AND model='sso-test-retired')=0,
		(SELECT count(*) FROM billing_accounts WHERE user_id=$1 AND group_id IS NOT NULL),
		(SELECT count(*) FROM password_credentials WHERE user_id=$1)+(SELECT count(*) FROM webauthn_credentials WHERE user_id=$1)
		FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&zeroBalance, &defaultModels, &groups, &credentials); err != nil {
		t.Fatal(err)
	}
	if !zeroBalance || !defaultModels || groups != 0 || credentials != 0 {
		t.Fatalf("unexpected account privileges: zeroBalance=%v defaults=%v groups=%d credentials=%d", zeroBalance, defaultModels, groups, credentials)
	}
	count := externalUserCount(t, ctx, s)
	if _, _, err := s.CompleteExternalRegistration(ctx, externalRegistrationParams(t, "first-user", at)); !errors.Is(err, ErrConflict) {
		t.Fatalf("replayed registration=%v", err)
	}
	for _, scenario := range []string{"bad-session", "expired", "future", "missing-proof"} {
		t.Run(scenario, func(t *testing.T) {
			bad := externalRegistrationParams(t, scenario, at)
			switch scenario {
			case "bad-session":
				bad.Session.TokenHash = nil
			case "expired":
				bad.VerifiedAt = at.Add(-10 * time.Minute)
			case "future":
				bad.VerifiedAt = at.Add(time.Second)
			case "missing-proof":
				bad.VerifiedAt = time.Time{}
			}
			if _, _, err := s.CompleteExternalRegistration(ctx, bad); err == nil {
				t.Fatal("invalid registration succeeded")
			}
			if got := externalUserCount(t, ctx, s); got != count {
				t.Fatalf("registration left a user: count=%d want=%d", got, count)
			}
			var remnants int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM external_identities WHERE subject=$1`, scenario).Scan(&remnants); err != nil || remnants != 0 {
				t.Fatalf("registration left a binding: count=%d err=%v", remnants, err)
			}
		})
	}
	loggedIn, next, err := s.CompleteExternalLogin(ctx, CompleteExternalLoginParams{Issuer: params.Issuer, Subject: params.Subject,
		At: at, VerifiedAt: at, Session: externalIdentityTestSession(t, "", at)})
	if err != nil || loggedIn.ID != user.ID || next.ExternalIdentityID == nil || *next.ExternalIdentityID != *session.ExternalIdentityID ||
		next.RecentlyVerifiedAt == nil || !next.RecentlyVerifiedAt.Equal(at) || externalUserCount(t, ctx, s) != count {
		t.Fatalf("subsequent login=%+v session=%+v err=%v", loggedIn, next, err)
	}
	if _, err := s.UnlinkExternalIdentity(ctx, user.ID, next.ID, at); !errors.Is(err, ErrLastLoginMethod) {
		t.Fatalf("sole SSO login was removable: %v", err)
	}
	if err := s.SetPassword(ctx, user.ID, next.ID, strings.Repeat("x", 80), at); err != nil {
		t.Fatal(err)
	}
	local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
	if _, err := s.UnlinkExternalIdentity(ctx, user.ID, next.ID, at); err != nil {
		t.Fatalf("could not unlink after optional password: %v", err)
	}
	if current, err := s.GetActiveSession(ctx, local.TokenHash, at); err != nil || current.RecentlyVerifiedAt != nil {
		t.Fatalf("local login or verification after unlink=%+v %v", current, err)
	}
}

func TestExternalReauthenticationPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	started := at.Add(-10 * time.Minute)
	s.now = func() time.Time { return started }
	user := globalUsageIntegrationUser(t, ctx, s, "reauth-user", UserRoleMember)
	local := externalIdentityTestLocalSession(t, ctx, s, user.ID, started)
	passwordHash := strings.Repeat("x", 80)
	if err := s.SetPassword(ctx, user.ID, local.ID, passwordHash, started); err != nil {
		t.Fatal(err)
	}
	binding := externalIdentityTestLink(t, ctx, s, user.ID, local.ID, user.ID, started)
	_, external, err := s.CompleteExternalLogin(ctx, CompleteExternalLoginParams{Issuer: binding.Issuer, Subject: binding.Subject,
		At: started, VerifiedAt: started, Session: externalIdentityTestSession(t, "", started)})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return at }
	base := CompleteExternalReauthenticationParams{UserID: user.ID, SessionID: local.ID, ExternalIdentityID: binding.ID,
		Issuer: binding.Issuer, Subject: binding.Subject, At: at, VerifiedAt: at.Add(-time.Second)}
	other := globalUsageIntegrationUser(t, ctx, s, "reauth-other", UserRoleMember)
	otherSession := externalIdentityTestLocalSession(t, ctx, s, other.ID, at)
	for _, scenario := range []string{"wrong-user", "wrong-session", "wrong-binding", "wrong-subject", "wrong-issuer", "expired", "future"} {
		t.Run(scenario, func(t *testing.T) {
			bad := base
			switch scenario {
			case "wrong-user":
				bad.UserID = other.ID
			case "wrong-session":
				bad.SessionID = otherSession.ID
			case "wrong-binding":
				bad.ExternalIdentityID = other.ID
			case "wrong-subject":
				bad.Subject += "-wrong"
			case "wrong-issuer":
				bad.Issuer += "/other"
			case "expired":
				bad.VerifiedAt = at.Add(-5 * time.Minute)
			case "future":
				bad.VerifiedAt = at.Add(time.Second)
			}
			if marker, err := s.CompleteExternalReauthentication(ctx, bad); err == nil || !marker.IsZero() {
				t.Fatalf("mismatched proof accepted: marker=%v err=%v", marker, err)
			}
		})
	}
	marker, err := s.CompleteExternalReauthentication(ctx, base)
	if err != nil || !marker.Equal(base.VerifiedAt) {
		t.Fatalf("reauthentication=%v %v", marker, err)
	}
	current, err := s.GetActiveSession(ctx, local.TokenHash, at)
	if err != nil || current.ID != local.ID || current.UserID != user.ID || current.ExternalIdentityID != nil ||
		current.RecentlyVerifiedAt == nil || !current.RecentlyVerifiedAt.Equal(marker) {
		t.Fatalf("reauthentication switched session/source: %+v %v", current, err)
	}
	if _, err := s.CompleteExternalReauthentication(ctx, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("equal timestamp overwrote verification: %v", err)
	}
	older := base
	older.VerifiedAt = base.VerifiedAt.Add(-time.Microsecond)
	if _, err := s.CompleteExternalReauthentication(ctx, older); !errors.Is(err, ErrConflict) {
		t.Fatalf("older timestamp overwrote verification: %v", err)
	}
	if err := s.MarkSessionVerified(ctx, local.ID, marker); err == nil {
		t.Fatal("equal local timestamp accepted as a later proof")
	}
	if err := s.VerifyPasswordSession(ctx, user.ID, local.ID, passwordHash, marker); err == nil {
		t.Fatal("equal password timestamp accepted as a later proof")
	}
	if err := s.VerifyPasswordSession(ctx, user.ID, local.ID, passwordHash, at); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearSessionVerificationIfCurrent(ctx, user.ID, local.ID, marker); err != nil {
		t.Fatal(err)
	}
	current, err = s.GetActiveSession(ctx, local.TokenHash, at)
	if err != nil || current.RecentlyVerifiedAt == nil || !current.RecentlyVerifiedAt.Equal(at) {
		t.Fatalf("cancellation removed newer local proof: %+v %v", current, err)
	}
	if err := s.ClearSessionVerificationIfCurrent(ctx, user.ID, local.ID, at); err != nil {
		t.Fatal(err)
	}
	current, err = s.GetActiveSession(ctx, local.TokenHash, at)
	if err != nil || current.RecentlyVerifiedAt != nil || current.RevokedAt != nil {
		t.Fatalf("conditional cancellation revoked local login: %+v %v", current, err)
	}
	base.SessionID = external.ID
	if _, err := s.CompleteExternalReauthentication(ctx, base); err != nil {
		t.Fatal(err)
	}
	current, err = s.GetActiveSession(ctx, external.TokenHash, at)
	if err != nil || current.ExternalIdentityID == nil || *current.ExternalIdentityID != binding.ID {
		t.Fatalf("reauthentication changed external provenance: %+v %v", current, err)
	}
	if err := s.RevokeSession(ctx, external.ID, "logout", at); err != nil {
		t.Fatal(err)
	}
	base.VerifiedAt = at
	if _, err := s.CompleteExternalReauthentication(ctx, base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late proof restored logged-out session: %v", err)
	}
}

func TestExternalRegistrationConcurrencyPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	t.Run("one account for concurrent registrations", func(t *testing.T) {
		before := externalUserCount(t, ctx, s)
		start := make(chan struct{})
		results := make(chan error, 8)
		for range 8 {
			params := externalRegistrationParams(t, "concurrent-registration", at)
			go func() { <-start; _, _, err := s.CompleteExternalRegistration(ctx, params); results <- err }()
		}
		close(start)
		ok := 0
		for range 8 {
			if err := <-results; err == nil {
				ok++
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
		}
		if got := externalUserCount(t, ctx, s); ok != 1 || got != before+1 {
			t.Fatalf("successful registrations=%d user count=%d want=%d", ok, got, before+1)
		}
	})
	t.Run("registration and existing-account binding compete atomically", func(t *testing.T) {
		for i := range 6 {
			user := globalUsageIntegrationUser(t, ctx, s, fmt.Sprintf("register-link-%d", i), UserRoleMember)
			local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
			before := externalUserCount(t, ctx, s)
			params := externalRegistrationParams(t, user.ID, at)
			start := make(chan struct{})
			var registerErr, linkErr error
			var wg sync.WaitGroup
			wg.Go(func() { <-start; _, _, registerErr = s.CompleteExternalRegistration(ctx, params) })
			wg.Go(func() {
				<-start
				_, linkErr = s.LinkExternalIdentity(ctx, LinkExternalIdentityParams{UserID: user.ID, SessionID: local.ID,
					Issuer: params.Issuer, Subject: params.Subject, At: at})
			})
			close(start)
			wg.Wait()
			wantCount := before
			if registerErr == nil {
				wantCount++
				if !errors.Is(linkErr, ErrConflict) {
					t.Fatalf("link losing race=%v", linkErr)
				}
			} else if !errors.Is(registerErr, ErrConflict) || linkErr != nil {
				t.Fatalf("registration=%v link=%v", registerErr, linkErr)
			}
			if got := externalUserCount(t, ctx, s); got != wantCount {
				t.Fatalf("race left partial user: got=%d want=%d", got, wantCount)
			}
		}
	})
}

func TestExternalRegistrationUsernameConflictPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	for _, sameIdentity := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-identity-%t", sameIdentity), func(t *testing.T) {
			before := externalUserCount(t, ctx, s)
			start := make(chan struct{})
			results := make(chan error, 8)
			username := fmt.Sprintf("contested-name-%t", sameIdentity)
			for i := range 8 {
				subject := fmt.Sprintf("contested-subject-%d", i)
				if sameIdentity {
					subject = "same-identity-contested-name"
				}
				params := externalRegistrationParams(t, subject, at)
				params.Username = username
				go func() {
					<-start
					_, _, err := s.CompleteExternalRegistration(ctx, params)
					results <- err
				}()
			}
			close(start)
			successes := 0
			for range 8 {
				err := <-results
				if err == nil {
					successes++
				} else if sameIdentity {
					if !errors.Is(err, ErrConflict) || errors.Is(err, ErrUsernameTaken) {
						t.Fatalf("identity collision was offered a profile retry: %v", err)
					}
				} else if !errors.Is(err, ErrUsernameTaken) {
					t.Fatalf("username conflict not distinguished: %v", err)
				}
			}
			if got := externalUserCount(t, ctx, s); successes != 1 || got != before+1 {
				t.Fatalf("concurrent registration: successes=%d users=%d want=%d", successes, got, before+1)
			}
			var bindings, sessions int
			if err := s.db.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM external_identities e JOIN users u ON u.id=e.user_id WHERE u.username=$1),
				(SELECT count(*) FROM sessions s JOIN users u ON u.id=s.user_id WHERE u.username=$1)`, username).Scan(&bindings, &sessions); err != nil || bindings != 1 || sessions != 1 {
				t.Fatalf("registration remnants: bindings=%d sessions=%d err=%v", bindings, sessions, err)
			}
		})
	}
}

func TestExternalUnlinkReauthenticationConcurrencyPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	user := globalUsageIntegrationUser(t, ctx, s, "unlink-reauth", UserRoleMember)
	local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at.Add(-time.Minute))
	if err := s.SetPassword(ctx, user.ID, local.ID, strings.Repeat("x", 80), at); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		if i > 0 {
			if err := s.MarkSessionVerified(ctx, local.ID, at.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		binding := externalIdentityTestLink(t, ctx, s, user.ID, local.ID, user.ID, at)
		start := make(chan struct{})
		var proofErr, unlinkErr error
		var marker time.Time
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			marker, proofErr = s.CompleteExternalReauthentication(ctx, CompleteExternalReauthenticationParams{
				UserID: user.ID, SessionID: local.ID, ExternalIdentityID: binding.ID,
				Issuer: binding.Issuer, Subject: binding.Subject, At: at, VerifiedAt: at})
		})
		wg.Go(func() { <-start; _, unlinkErr = s.UnlinkExternalIdentity(ctx, user.ID, local.ID, at) })
		close(start)
		wg.Wait()
		if unlinkErr != nil || (proofErr != nil && !errors.Is(proofErr, ErrNotFound)) {
			t.Fatalf("round %d: proof=%v unlink=%v", i, proofErr, unlinkErr)
		}
		current, err := s.GetActiveSession(ctx, local.TokenHash, at)
		if err != nil || current.RecentlyVerifiedAt != nil || current.ExternalIdentityID != nil {
			t.Fatalf("proof survived unlink or local login was lost: %+v %v", current, err)
		}
		if err := s.ClearSessionVerificationIfCurrent(ctx, user.ID, local.ID, marker); err != nil {
			t.Fatal(err)
		}
	}
}
