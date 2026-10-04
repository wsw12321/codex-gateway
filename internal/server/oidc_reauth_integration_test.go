//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

func (f oidcHTTPFixture) bindOwner(t *testing.T) {
	t.Helper()
	f.setLocalPassword(t)
	flow, browser := f.preview(t)
	f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 200)
}

func (f oidcHTTPFixture) expireVerification(t *testing.T, cookie string) store.Session {
	t.Helper()
	session, err := f.s.oidcCurrentSession(cookieRequest(cookie))
	if err != nil || session.ID == "" {
		t.Fatalf("missing session %+v %v", session, err)
	}
	if _, err := f.s.store.DB().ExecContext(context.Background(), `UPDATE sessions SET recently_verified_at=now()-interval '6 minutes' WHERE id=$1`, session.ID); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestOIDCHTTPPostgresReauthenticationPreservesOriginalSession(t *testing.T) {
	for _, kind := range []string{"local", "sso"} {
		t.Run(kind, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			cookie := f.cookie
			if kind == "local" {
				f.bindOwner(t)
			} else {
				_, cookie = f.register(t)
			}
			before := f.expireVerification(t, cookie)
			state, browser := f.begin(t, "reauth", cookie)
			w := f.request(t, "/auth/oidc/complete", "POST", cookie, browser, map[string]string{"state": state, "code": "reauth"}, 200)
			if invitationResponse(t, w)["result"] != "reauthenticated" {
				t.Fatal("wrong reauthentication result")
			}
			assertNoOIDCSessionCookie(t, w)
			oidcAssertTransactionCookieCleared(t, w)
			after, err := f.s.oidcCurrentSession(cookieRequest(cookie))
			if err != nil || after.ID != before.ID || after.UserID != before.UserID || !oidcRecent(after, time.Now()) ||
				(before.ExternalIdentityID == nil) != (after.ExternalIdentityID == nil) ||
				(before.ExternalIdentityID != nil && *before.ExternalIdentityID != *after.ExternalIdentityID) {
				t.Fatalf("reauth changed provenance or failed: before=%+v after=%+v err=%v", before, after, err)
			}
			f.request(t, "/auth/oidc/complete", "POST", cookie, browser, map[string]string{"state": state, "code": "reauth"}, 400)
		})
	}
}

func TestOIDCHTTPPostgresReauthenticationRejectsChangedContext(t *testing.T) {
	for _, change := range []string{"wrong-subject", "wrong-issuer", "same-user-new-session", "different-user", "logout-during-exchange", "cancel-during-exchange", "disabled", "unlink-during-exchange", "replaced-binding"} {
		t.Run(change, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			f.bindOwner(t)
			before := f.expireVerification(t, f.cookie)
			state, browser := f.begin(t, "reauth", f.cookie)
			cookie := f.cookie
			switch change {
			case "wrong-subject":
				f.provider.external.Subject += "-wrong"
			case "wrong-issuer":
				f.provider.external.Issuer += "/wrong"
			case "same-user-new-session":
				cookie = f.localSession(t, f.owner.ID)
			case "different-user":
				user, err := f.s.store.CreateUser(context.Background(), store.CreateUserParams{Username: "other-reauth", DisplayName: "Other", Role: store.UserRoleMember})
				if err != nil {
					t.Fatal(err)
				}
				cookie = f.localSession(t, user.ID)
			case "logout-during-exchange":
				f.provider.beforeExchange = func() { f.request(t, "/auth/logout", "POST", f.cookie, browser, nil, 200) }
			case "cancel-during-exchange":
				f.provider.beforeExchange = func() {
					f.request(t, "/auth/oidc/cancel", "POST", f.cookie, browser, map[string]string{"flow_id": state}, 200)
				}
			case "disabled":
				if _, err := f.s.store.DB().ExecContext(context.Background(), `UPDATE users SET status='disabled', disabled_at=now() WHERE id=$1`, f.owner.ID); err != nil {
					t.Fatal(err)
				}
			case "unlink-during-exchange", "replaced-binding":
				f.provider.beforeExchange = func() {
					// Another session performs a valid unlink while exchange runs.
					other := f.localSession(t, f.owner.ID)
					f.request(t, "/admin/identity-link", "DELETE", other, "", nil, 200)
					if change == "replaced-binding" {
						otherSession, err := f.s.oidcCurrentSession(cookieRequest(other))
						if err != nil {
							t.Fatal(err)
						}
						if err := f.s.store.MarkSessionVerified(context.Background(), otherSession.ID, time.Now().UTC()); err != nil {
							t.Fatal(err)
						}
						_, err = f.s.store.LinkExternalIdentity(context.Background(), store.LinkExternalIdentityParams{UserID: f.owner.ID, SessionID: otherSession.ID,
							Issuer: f.provider.external.Issuer, Subject: f.provider.external.Subject, At: time.Now().UTC()})
						if err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			w := f.request(t, "/auth/oidc/complete", "POST", cookie, browser, map[string]string{"state": state, "code": "late"}, 400)
			assertNoOIDCSessionCookie(t, w)
			var after *time.Time
			if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT recently_verified_at FROM sessions WHERE id=$1`, before.ID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != nil && after.After(time.Now().Add(-5*time.Minute)) {
				t.Fatal("rejected flow granted recent verification")
			}
		})
	}
}

func TestOIDCHTTPPostgresCancelledVerificationOnlyClearsItsOwnWrite(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-proof", true: "later-local-proof"}[newer], func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			f.bindOwner(t)
			before := f.expireVerification(t, f.cookie)
			state, browser := f.begin(t, "reauth", f.cookie)
			f.request(t, "/auth/oidc/complete", "POST", f.cookie, browser, map[string]string{"state": state, "code": "valid"}, 200)
			var expected time.Time
			if newer {
				expected = time.Now().UTC().Truncate(time.Microsecond)
				if err := f.s.store.MarkSessionVerified(context.Background(), before.ID, expected); err != nil {
					t.Fatal(err)
				}
			}
			// A cancelled tab can deliver this request after the success response.
			w := f.request(t, "/auth/oidc/cancel", "POST", f.cookie, browser, map[string]string{"flow_id": state}, 200)
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("late cancellation changed cookies")
			}
			after, err := f.s.oidcCurrentSession(cookieRequest(f.cookie))
			if err != nil || after.ID != before.ID || after.ExternalIdentityID != nil {
				t.Fatalf("cancel revoked or replaced local session: %+v %v", after, err)
			}
			if newer {
				if after.RecentlyVerifiedAt == nil || !after.RecentlyVerifiedAt.Equal(expected) {
					t.Fatal("cancel cleared a later verification")
				}
			} else if after.RecentlyVerifiedAt != nil {
				t.Fatal("cancel retained its own verification")
			}
		})
	}
}

func TestOIDCHTTPPostgresReauthenticationCancellationAfterCookieCleared(t *testing.T) {
	for _, action := range []string{"pagehide", "local-login", "logout"} {
		t.Run(action, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			f.bindOwner(t)
			before := f.expireVerification(t, f.cookie)
			state, browser := f.begin(t, "reauth", f.cookie)
			f.request(t, "/auth/oidc/complete", "POST", f.cookie, browser, map[string]string{"state": state, "code": "valid"}, 200)
			// Headers clear the transaction cookie before JS consumes the body.
			// Neither missing flow ID nor another session may use the fallback.
			f.request(t, "/auth/oidc/cancel", "POST", f.cookie, "", map[string]string{}, 400)
			other := f.localSession(t, f.owner.ID)
			f.request(t, "/auth/oidc/cancel", "POST", other, "", map[string]string{"flow_id": state}, 400)
			switch action {
			case "pagehide":
				f.request(t, "/auth/oidc/cancel", "POST", f.cookie, "", map[string]string{"flow_id": state}, 200)
			case "local-login":
				w := f.request(t, "/auth/password/login", "POST", f.cookie, "", map[string]string{"username": f.owner.Username, "password": "local-password-123"}, 200)
				f.request(t, "/admin/state", "GET", oidcResponseCookie(t, w, sessionCookieName), "", nil, 200)
			case "logout":
				f.request(t, "/auth/logout", "POST", f.cookie, "", nil, 200)
			}
			var verified, revoked *time.Time
			if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT recently_verified_at, revoked_at FROM sessions WHERE id=$1`, before.ID).Scan(&verified, &revoked); err != nil {
				t.Fatal(err)
			}
			if verified != nil || (revoked != nil) != (action == "logout") {
				t.Fatalf("cancel after cleared cookie: proof=%v revoked=%v", verified, revoked)
			}
		})
	}
}

type oidcReauthInterceptRepository struct {
	oidcRepository
	after func()
}

func (repo oidcReauthInterceptRepository) CompleteExternalReauthentication(ctx context.Context, p store.CompleteExternalReauthenticationParams) (time.Time, error) {
	marker, err := repo.oidcRepository.CompleteExternalReauthentication(ctx, p)
	if err == nil {
		repo.after()
	}
	return marker, err
}

func TestOIDCHTTPPostgresDisconnectedReauthenticationRollsBackConditionally(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(map[bool]string{false: "own-proof", true: "newer-proof"}[newer], func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			f.bindOwner(t)
			before := f.expireVerification(t, f.cookie)
			state, browser := f.begin(t, "reauth", f.cookie)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.s.oidcRepo = oidcReauthInterceptRepository{oidcRepository: f.s.store, after: func() {
				if newer {
					if err := f.s.store.MarkSessionVerified(context.Background(), before.ID, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
				cancel()
			}}
			raw, _ := json.Marshal(map[string]string{"state": state, "code": "valid"})
			r := httptest.NewRequest("POST", "/auth/oidc/complete", strings.NewReader(string(raw))).WithContext(ctx)
			r.Header.Set("Origin", "https://gateway.example")
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.cookie})
			r.AddCookie(&http.Cookie{Name: oidcCookieName, Value: browser})
			w := httptest.NewRecorder()
			f.s.Handler().ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatalf("disconnected reauth status %d: %s", w.Code, w.Body)
			}
			after, err := f.s.oidcCurrentSession(cookieRequest(f.cookie))
			if err != nil || after.ID != before.ID || (after.RecentlyVerifiedAt != nil) != newer {
				t.Fatalf("unsafe rollback: %+v %v", after, err)
			}
		})
	}
}
