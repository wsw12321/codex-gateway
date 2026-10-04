//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const externalIdentityTestIssuer = "https://accounts.example.test/auth/v1"

func externalIdentityTestSession(t *testing.T, userID string, at time.Time) CreateSessionParams {
	t.Helper()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(id))
	return CreateSessionParams{UserID: userID, TokenHash: hash[:], CSRFSecret: hash[:], CreatedAt: at,
		IdleExpiresAt: at.Add(time.Hour), AbsoluteExpiresAt: at.Add(24 * time.Hour)}
}

func externalIdentityTestLocalSession(t *testing.T, ctx context.Context, s *Store, userID string, at time.Time) Session {
	t.Helper()
	session, err := s.CreateSession(ctx, externalIdentityTestSession(t, userID, at))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionVerified(ctx, session.ID, at); err != nil {
		t.Fatal(err)
	}
	return session
}

func externalIdentityTestLink(t *testing.T, ctx context.Context, s *Store, userID, sessionID, subject string, at time.Time) ExternalIdentity {
	t.Helper()
	identity, err := s.LinkExternalIdentity(ctx, LinkExternalIdentityParams{UserID: userID, SessionID: sessionID,
		Issuer: externalIdentityTestIssuer, Subject: subject, MaskedEmail: "u***@example.test", At: at})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestExternalIdentityLifecyclePostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	user, _, key := billingIntegrationPrincipal(t, ctx, s, "external-lifecycle")
	local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
	if err := s.SetPassword(ctx, user.ID, local.ID, strings.Repeat("x", 80), at); err != nil {
		t.Fatal(err)
	}
	passkeyHash := sha256.Sum256([]byte(user.ID))
	credential, err := s.AddWebAuthnCredential(ctx, AddWebAuthnCredentialParams{UserID: user.ID, CredentialID: passkeyHash[:], CredentialJSON: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var userCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CompleteExternalLogin(ctx, CompleteExternalLoginParams{Issuer: externalIdentityTestIssuer,
		Subject: "unbound", Session: externalIdentityTestSession(t, "", at), At: at}); !errors.Is(err, ErrExternalIdentityUnbound) {
		t.Fatalf("unbound login = %v", err)
	}
	var afterCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&afterCount); err != nil || userCount != afterCount {
		t.Fatalf("unbound login created a user: before=%d after=%d err=%v", userCount, afterCount, err)
	}
	identity := externalIdentityTestLink(t, ctx, s, user.ID, local.ID, "bound-user", at)
	if got, err := s.GetExternalIdentity(ctx, user.ID); err != nil || got.ID != identity.ID || got.MaskedEmail != "u***@example.test" {
		t.Fatalf("binding lookup = %+v %v", got, err)
	}
	login := func(label string) Session {
		t.Helper()
		loggedIn, session, err := s.CompleteExternalLogin(ctx, CompleteExternalLoginParams{Issuer: externalIdentityTestIssuer,
			Subject: "bound-user", Session: externalIdentityTestSession(t, "", at), At: at})
		if err != nil || loggedIn.ID != user.ID || session.UserID != user.ID || session.ExternalIdentityID == nil || *session.ExternalIdentityID != identity.ID || session.RecentlyVerifiedAt == nil || !session.RecentlyVerifiedAt.Equal(at) {
			t.Fatalf("%s login changed ownership/source/verification: user=%+v session=%+v err=%v", label, loggedIn, session, err)
		}
		return session
	}
	one, two := login("first"), login("second")
	if _, err := s.LinkExternalIdentity(ctx, LinkExternalIdentityParams{UserID: user.ID, SessionID: local.ID, Issuer: externalIdentityTestIssuer, Subject: "bound-user", At: at}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate confirm = %v", err)
	}
	removed, err := s.UnlinkExternalIdentity(ctx, user.ID, one.ID, at)
	if err != nil || removed.ID != identity.ID || removed.UnlinkedAt == nil {
		t.Fatalf("unlink = %+v %v", removed, err)
	}
	for _, session := range []Session{one, two} {
		if _, err := s.GetActiveSession(ctx, session.TokenHash, at); !errors.Is(err, ErrNotFound) {
			t.Fatalf("external session survived unlink: %v", err)
		}
	}
	if current, err := s.GetActiveSession(ctx, local.TokenHash, at); err != nil || current.ExternalIdentityID != nil || current.RecentlyVerifiedAt != nil {
		t.Fatalf("local session changed or revoked: %+v %v", current, err)
	}
	if got, err := s.GetPasswordCredential(ctx, user.ID); err != nil || got.EncodedHash != strings.Repeat("x", 80) {
		t.Fatalf("local password changed: %v", err)
	}
	if got, err := s.GetWebAuthnCredential(ctx, passkeyHash[:]); err != nil || got.ID != credential.ID || got.UserID != user.ID {
		t.Fatalf("Passkey changed: %v", err)
	}
	if got, err := s.LookupAPIKey(ctx, key.PublicID); err != nil || got.ID != key.ID || got.UserID != user.ID {
		t.Fatalf("API key ownership changed: %v", err)
	}
	if _, err := s.GetExternalIdentity(ctx, user.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlinked identity still active: %v", err)
	}
	if err := s.MarkSessionVerified(ctx, local.ID, at); err != nil {
		t.Fatal(err)
	}
	newIdentity := externalIdentityTestLink(t, ctx, s, user.ID, local.ID, "bound-user", at)
	if newIdentity.ID == identity.ID {
		t.Fatal("relink reused an old binding ID")
	}
	identity = newIdentity
	_ = login("relinked")
	for _, session := range []Session{one, two} {
		if _, err := s.GetActiveSession(ctx, session.TokenHash, at); !errors.Is(err, ErrNotFound) {
			t.Fatalf("relink restored revoked session: %v", err)
		}
	}
}

func TestExternalIdentityAuthorizationPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	t.Run("original confirmation deadline survives newer local proof", func(t *testing.T) {
		user := globalUsageIntegrationUser(t, ctx, s, "original-deadline", UserRoleMember)
		session := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
		params := LinkExternalIdentityParams{UserID: user.ID, SessionID: session.ID, Issuer: externalIdentityTestIssuer,
			Subject: user.ID, At: at, VerificationExpiresAt: at}
		if _, err := s.LinkExternalIdentity(ctx, params); !errors.Is(err, ErrNotFound) {
			t.Fatalf("new local proof extended original flow: %v", err)
		}
		params.VerificationExpiresAt = at.Add(time.Minute)
		if _, err := s.LinkExternalIdentity(ctx, params); err != nil {
			t.Fatalf("unexpired confirmation rejected: %v", err)
		}
	})
	for _, scenario := range []string{"no-verification", "expired-verification", "exact-expiry", "future-verification", "revoked-session", "idle-expired", "absolute-expired", "wrong-session", "disabled", "pending"} {
		for _, operation := range []string{"link", "unlink"} {
			t.Run(scenario+"/"+operation, func(t *testing.T) {
				user := globalUsageIntegrationUser(t, ctx, s, scenario+"-"+operation, UserRoleMember)
				session := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
				if operation == "unlink" {
					externalIdentityTestLink(t, ctx, s, user.ID, session.ID, user.ID, at)
				}
				var query string
				switch scenario {
				case "no-verification":
					query = `UPDATE sessions SET recently_verified_at=NULL WHERE id=$1`
				case "expired-verification":
					query = `UPDATE sessions SET recently_verified_at=$2::timestamptz - interval '5 minutes 1 microsecond' WHERE id=$1`
				case "exact-expiry":
					query = `UPDATE sessions SET recently_verified_at=$2::timestamptz - interval '5 minutes' WHERE id=$1`
				case "future-verification":
					query = `UPDATE sessions SET recently_verified_at=$2::timestamptz + interval '1 second' WHERE id=$1`
				case "revoked-session":
					query = `UPDATE sessions SET revoked_at=$2 WHERE id=$1`
				case "idle-expired":
					query = `UPDATE sessions SET created_at=$2::timestamptz - interval '1 hour',idle_expires_at=$2 WHERE id=$1`
				case "absolute-expired":
					query = `UPDATE sessions SET created_at=$2::timestamptz - interval '1 hour',idle_expires_at=$2,absolute_expires_at=$2 WHERE id=$1`
				case "wrong-session":
					other := globalUsageIntegrationUser(t, ctx, s, "other-"+operation, UserRoleMember)
					session = externalIdentityTestLocalSession(t, ctx, s, other.ID, at)
				case "disabled":
					if err := s.DisableUser(ctx, user.ID, at); err != nil {
						t.Fatal(err)
					}
				case "pending":
					if _, err := s.db.ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, user.ID); err != nil {
						t.Fatal(err)
					}
				}
				if query != "" {
					args := []any{session.ID}
					if strings.Contains(query, "$2") {
						args = append(args, at)
					}
					if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if operation == "link" {
					_, err = s.LinkExternalIdentity(ctx, LinkExternalIdentityParams{UserID: user.ID, SessionID: session.ID, Issuer: externalIdentityTestIssuer, Subject: user.ID, At: at})
				} else {
					_, err = s.UnlinkExternalIdentity(ctx, user.ID, session.ID, at)
				}
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("unauthorized %s = %v", operation, err)
				}
				if operation == "unlink" && (scenario == "disabled" || scenario == "pending") {
					if _, _, err := s.CompleteExternalLogin(ctx, CompleteExternalLoginParams{Issuer: externalIdentityTestIssuer, Subject: user.ID,
						Session: externalIdentityTestSession(t, "", at), At: at}); !errors.Is(err, ErrNotFound) {
						t.Fatalf("inactive user logged in: %v", err)
					}
				}
			})
		}
	}
}

func TestExternalIdentityConcurrencyPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	t.Run("one external account cannot bind two users", func(t *testing.T) {
		var params []LinkExternalIdentityParams
		for i := range 2 {
			user := globalUsageIntegrationUser(t, ctx, s, fmt.Sprintf("compete-%d", i), UserRoleMember)
			session := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
			params = append(params, LinkExternalIdentityParams{UserID: user.ID, SessionID: session.ID, Issuer: externalIdentityTestIssuer, Subject: "contested", At: at})
		}
		start := make(chan struct{})
		results := make(chan error, len(params))
		for _, p := range params {
			go func() { <-start; _, err := s.LinkExternalIdentity(ctx, p); results <- err }()
		}
		close(start)
		successes, conflicts := 0, 0
		for range params {
			err := <-results
			if err == nil {
				successes++
			} else if errors.Is(err, ErrConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
		}
	})
	t.Run("unlink revokes racing logins", func(t *testing.T) {
		user := globalUsageIntegrationUser(t, ctx, s, "racing-login", UserRoleMember)
		local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
		if err := s.SetPassword(ctx, user.ID, local.ID, strings.Repeat("x", 80), at); err != nil {
			t.Fatal(err)
		}
		for i := range 12 {
			if i > 0 {
				if err := s.MarkSessionVerified(ctx, local.ID, at); err != nil {
					t.Fatal(err)
				}
			}
			identity := externalIdentityTestLink(t, ctx, s, user.ID, local.ID, user.ID, at)
			params := CompleteExternalLoginParams{Issuer: externalIdentityTestIssuer, Subject: user.ID, Session: externalIdentityTestSession(t, "", at), At: at}
			var session Session
			var loginErr, unlinkErr error
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Go(func() { <-start; _, session, loginErr = s.CompleteExternalLogin(ctx, params) })
			wg.Go(func() { <-start; _, unlinkErr = s.UnlinkExternalIdentity(ctx, user.ID, local.ID, at) })
			close(start)
			wg.Wait()
			if unlinkErr != nil || (loginErr != nil && !errors.Is(loginErr, ErrNotFound) && !errors.Is(loginErr, ErrExternalIdentityUnbound)) {
				t.Fatalf("round %d: login=%v unlink=%v", i, loginErr, unlinkErr)
			}
			if loginErr == nil {
				if _, err := s.GetActiveSession(ctx, session.TokenHash, at); !errors.Is(err, ErrNotFound) {
					t.Fatalf("racing login survived unlink: %v", err)
				}
			}
			var active int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE external_identity_id=$1 AND revoked_at IS NULL`, identity.ID).Scan(&active); err != nil || active != 0 {
				t.Fatalf("unrevoked old-binding sessions=%d err=%v", active, err)
			}
		}
	})
	t.Run("waiting login cannot follow an unlink and relink", func(t *testing.T) {
		user := globalUsageIntegrationUser(t, ctx, s, "waiting-login", UserRoleMember)
		local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
		identity := externalIdentityTestLink(t, ctx, s, user.ID, local.ID, user.ID, at)
		blocker, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		var pid int
		if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM users WHERE id=$1 FOR UPDATE`, user.ID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		params := CompleteExternalLoginParams{Issuer: externalIdentityTestIssuer, Subject: user.ID, Session: externalIdentityTestSession(t, "", at), At: at}
		result := make(chan error, 1)
		go func() { _, _, err := s.CompleteExternalLogin(ctx, params); result <- err }()
		billingSourcePreferenceWaitForLock(t, ctx, s, pid, 1)
		// The blocked login has read its binding ID. Perform the unlink/relink
		// writes while holding the same user lock as the public transactions.
		if _, err := blocker.ExecContext(ctx, `UPDATE external_identities SET unlinked_at=$2 WHERE id=$1`, identity.ID, at); err != nil {
			t.Fatal(err)
		}
		if _, err := blocker.ExecContext(ctx, `INSERT INTO external_identities(user_id,issuer,subject,linked_at) VALUES($1,$2,$3,$4)`, user.ID, externalIdentityTestIssuer, user.ID, at); err != nil {
			t.Fatal(err)
		}
		if err := blocker.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, ErrNotFound) {
			t.Fatalf("stale binding login = %v", err)
		}
	})
	t.Run("verification expires while waiting for a lock", func(t *testing.T) {
		user := globalUsageIntegrationUser(t, ctx, s, "waiting-confirm", UserRoleMember)
		local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
		var clock atomic.Int64
		clock.Store(at.UnixNano())
		s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
		defer func() { s.now = func() time.Time { return at } }()
		blocker, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		var pid int
		if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM sessions WHERE id=$1 FOR UPDATE`, local.ID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := s.LinkExternalIdentity(ctx, LinkExternalIdentityParams{UserID: user.ID, SessionID: local.ID, Issuer: externalIdentityTestIssuer, Subject: user.ID, At: at})
			result <- err
		}()
		billingSourcePreferenceWaitForLock(t, ctx, s, pid, 1)
		clock.Store(at.Add(5*time.Minute + time.Microsecond).UnixNano())
		if err := blocker.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, ErrNotFound) {
			t.Fatalf("confirmation accepted stale request timestamp: %v", err)
		}
	})
}

func TestExternalIdentityDeletionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return at }
	owner := globalUsageIntegrationUser(t, ctx, s, "external-delete-owner", UserRoleOwner)
	for _, mode := range []string{"information", "pending-invitation"} {
		t.Run(mode, func(t *testing.T) {
			user := globalUsageIntegrationUser(t, ctx, s, "delete-"+mode, UserRoleMember)
			local := externalIdentityTestLocalSession(t, ctx, s, user.ID, at)
			identity := externalIdentityTestLink(t, ctx, s, user.ID, local.ID, user.ID, at)
			_, external, err := s.CompleteExternalLogin(ctx, CompleteExternalLoginParams{Issuer: externalIdentityTestIssuer,
				Subject: user.ID, Session: externalIdentityTestSession(t, "", at), At: at})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "information" {
				_, err = s.DeleteInformationUsers(ctx, DeleteInformationUsersParams{BillingWriteParams: billingIntegrationWrite(t, owner.ID, "external identity deletion", at), UserIDs: []string{user.ID}})
			} else {
				// Pending users cannot normally link; seed the state defensively
				// to ensure the second deletion path also removes mappings.
				err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `UPDATE users SET status='pending' WHERE id=$1`, user.ID); err != nil {
						return err
					}
					user.Status = StatusPending
					return deletePendingInvitationUserTx(ctx, tx, user)
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM external_identities WHERE id=$1)
				+ (SELECT count(*) FROM sessions WHERE id=$2 OR id=$3)`, identity.ID, local.ID, external.ID).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatalf("local identity artifacts survived deletion: count=%d err=%v", remaining, err)
			}
		})
	}
}
