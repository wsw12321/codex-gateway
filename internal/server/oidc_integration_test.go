//go:build integration

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

type oidcHTTPFixture struct {
	invitationHTTPFixture
	provider *testOIDCProvider
}

func newOIDCHTTPFixture(t *testing.T) oidcHTTPFixture {
	f := oidcHTTPFixture{invitationHTTPFixture: newInvitationHTTPFixture(t), provider: &testOIDCProvider{external: identity.OIDCIdentity{Issuer: "https://auth.example/auth/v1", Subject: "oidc-subject", MaskedEmail: "a***@example.test"}}}
	f.s.config.OIDCEnabled = true
	f.s.oidc = f.provider
	return f
}
func (f oidcHTTPFixture) request(t *testing.T, path, method, session, browser string, input any, status int) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r.Header.Set("Origin", "https://gateway.example")
	r.Header.Set("Content-Type", "application/json")
	if session != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	}
	if browser != "" {
		r.AddCookie(&http.Cookie{Name: oidcCookieName, Value: browser})
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}
func oidcResponseCookie(t *testing.T, w *httptest.ResponseRecorder, name string) string {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == name && cookie.MaxAge > 0 {
			if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
				t.Fatalf("insecure cookie: %+v", cookie)
			}
			return cookie.Value
		}
	}
	t.Fatalf("missing cookie %s", name)
	return ""
}

func oidcAssertTransactionCookieCleared(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == oidcCookieName && cookie.MaxAge < 0 && cookie.Value == "" {
			return
		}
	}
	t.Fatal("successful terminal response retained transaction cookie")
}
func (f oidcHTTPFixture) begin(t *testing.T, kind, session string) (string, string) {
	t.Helper()
	path := "/auth/oidc/login"
	if kind == "link" {
		path = "/admin/identity-link/begin"
	}
	w := f.request(t, path, "POST", session, "", map[string]any{}, 200)
	result := invitationResponse(t, w)
	u, _ := url.Parse(result["authorization_url"].(string))
	return u.Query().Get("state"), oidcResponseCookie(t, w, oidcCookieName)
}
func (f oidcHTTPFixture) preview(t *testing.T) (string, string) {
	t.Helper()
	state, browser := f.begin(t, "link", f.cookie)
	w := f.request(t, "/auth/oidc/complete", "POST", f.cookie, browser, map[string]string{"state": state, "code": "authorization-code"}, 200)
	result := invitationResponse(t, w)
	if result["result"] != "confirm" || result["masked_email"] != f.provider.external.MaskedEmail || strings.Contains(w.Body.String(), "oidc-subject") {
		t.Fatal("unsafe binding preview", w.Body.String())
	}
	return result["flow_id"].(string), browser
}
func TestOIDCHTTPPostgresLifecycle(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	ctx := context.Background()
	f.request(t, "/auth/oidc/login", "POST", f.cookie, "", nil, 409)
	loginState, browser := f.begin(t, "login", "")
	f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": loginState, "code": "unbound"}, 403)
	flow, browser := f.preview(t)
	if _, err := f.s.store.GetExternalIdentity(ctx, f.owner.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("preview wrote mapping")
	}
	oidcAssertTransactionCookieCleared(t, f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 200))
	f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 400)
	f.request(t, "/admin/state", "GET", f.cookie, "", nil, 200)
	// Local session remains unchanged and keeps its original provenance.
	session, err := f.s.oidcCurrentSession(cookieRequest(f.cookie))
	if err != nil || session.ExternalIdentityID != nil {
		t.Fatal("binding changed original session")
	}
	state, browser := f.begin(t, "login", "")
	w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "bound"}, 200)
	oidcAssertTransactionCookieCleared(t, w)
	externalCookie := oidcResponseCookie(t, w, sessionCookieName)
	f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "bound"}, 400)
	extSession, err := f.s.oidcCurrentSession(cookieRequest(externalCookie))
	if err != nil || extSession.UserID != f.owner.ID || extSession.RecentlyVerifiedAt != nil || extSession.ExternalIdentityID == nil {
		t.Fatalf("external session: %+v %v", extSession, err)
	}
	f.request(t, "/admin/api-keys/key-id/reveal", "POST", externalCookie, "", nil, 403)
	f.request(t, "/admin/identity-link", "DELETE", externalCookie, "", nil, 403)
	if err := f.s.store.MarkSessionVerified(ctx, extSession.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	w = f.request(t, "/admin/identity-link", "DELETE", externalCookie, "", nil, 200)
	if invitationResponse(t, w)["logged_out"] != true {
		t.Fatal("current external session not logged out")
	}
	f.request(t, "/admin/state", "GET", externalCookie, "", nil, 401)
	f.request(t, "/admin/state", "GET", f.cookie, "", nil, 200)
}
func cookieRequest(value string) *http.Request {
	r := httptest.NewRequest("POST", "https://gateway.example/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	return r
}
func TestOIDCHTTPPostgresRejectsCookieReplayAndPurposeMixing(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	state, browser := f.begin(t, "link", f.cookie)
	input := map[string]string{"state": state, "code": "code-secret"}
	f.request(t, "/auth/oidc/complete", "POST", f.cookie, "", input, 400)
	if f.provider.exchanges != 0 {
		t.Fatal("missing transaction cookie reached provider")
	}
	f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": state}, 400)
	f.request(t, "/auth/oidc/complete", "POST", f.cookie, browser, input, 400)
	if f.provider.exchanges != 0 {
		t.Fatal("purpose mismatch reached provider")
	}
	flow, browser := f.preview(t)
	oidcAssertTransactionCookieCleared(t, f.request(t, "/admin/identity-link/cancel", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 200))
	f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 400)
	if _, err := f.s.store.GetExternalIdentity(context.Background(), f.owner.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cancel wrote identity")
	}
	state, browser = f.begin(t, "login", "")
	f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "error": "access_denied"}, 400)
	calls := f.provider.exchanges
	f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "denied-code"}, 400)
	if f.provider.exchanges != calls {
		t.Fatal("denied transaction reused")
	}
}
func TestOIDCHTTPPostgresRejectsChangedLocalState(t *testing.T) {
	for _, change := range []string{"logout", "disabled", "stale", "switched", "expired-flow", "exchange-failure"} {
		t.Run(change, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			ctx := context.Background()
			state, browser := f.begin(t, "link", f.cookie)
			session, err := f.s.oidcCurrentSession(cookieRequest(f.cookie))
			if err != nil {
				t.Fatal(err)
			}
			cookie := f.cookie
			switch change {
			case "logout":
				_ = f.s.store.RevokeSession(ctx, session.ID, "test", time.Now())
			case "disabled":
				_, err = f.s.store.DB().ExecContext(ctx, `UPDATE users SET status='disabled',disabled_at=now() WHERE id=$1`, f.owner.ID)
			case "stale":
				_, err = f.s.store.DB().ExecContext(ctx, `UPDATE sessions SET recently_verified_at=$2 WHERE id=$1`, session.ID, time.Now().Add(-6*time.Minute))
			case "switched":
				cookie = ""
			case "expired-flow":
				f.s.oidcFlows.mu.Lock()
				for key, flow := range f.s.oidcFlows.entries {
					flow.Expires = time.Now().Add(-time.Second)
					f.s.oidcFlows.entries[key] = flow
				}
				f.s.oidcFlows.mu.Unlock()
			case "exchange-failure":
				f.provider.exchangeError = errors.New("secret provider error")
			}
			if err != nil {
				t.Fatal(err)
			}
			w := f.request(t, "/auth/oidc/complete", "POST", cookie, browser, map[string]string{"state": state, "code": "secret-code"}, 400)
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("provider detail leaked")
			}
			if _, err := f.s.store.GetExternalIdentity(ctx, f.owner.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("rejected flow wrote binding")
			}
		})
	}
}
func TestOIDCHTTPPostgresRechecksSessionAfterProvider(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	ctx := context.Background()
	state, browser := f.begin(t, "link", f.cookie)
	session, _ := f.s.oidcCurrentSession(cookieRequest(f.cookie))
	f.provider.beforeExchange = func() {
		if err := f.s.store.RevokeSession(ctx, session.ID, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	f.request(t, "/auth/oidc/complete", "POST", f.cookie, browser, map[string]string{"state": state, "code": "code"}, 400)
}

func (f oidcHTTPFixture) localSession(t *testing.T, userID string) string {
	t.Helper()
	ctx := context.Background()
	token, err := security.GenerateOpaqueToken(security.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := security.PepperTokenDigest(f.s.config.TokenPepper, token.Digest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session, err := f.s.store.CreateSession(ctx, store.CreateSessionParams{
		UserID: userID, TokenHash: digest[:], CSRFSecret: []byte(strings.Repeat("s", 32)),
		CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.store.MarkSessionVerified(ctx, session.ID, now); err != nil {
		t.Fatal(err)
	}
	return token.Token
}

func assertNoOIDCSessionCookie(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" && cookie.MaxAge >= 0 {
			t.Fatal("rejected OIDC operation issued or replaced a local session cookie")
		}
	}
}

func (f oidcHTTPFixture) assertNoExternalSessions(t *testing.T) {
	t.Helper()
	var count int
	if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM sessions WHERE external_identity_id IS NOT NULL`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected operation created %d external sessions", count)
	}
}

func TestOIDCHTTPPostgresConfirmationRequiresOriginalLocalSession(t *testing.T) {
	for _, change := range []string{"same-user-new-session", "different-user-session", "logout", "disabled", "session-expired", "reauth-expired", "original-proof-expired"} {
		t.Run(change, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			ctx := context.Background()
			flowID, browser := f.preview(t)
			session, err := f.s.oidcCurrentSession(cookieRequest(f.cookie))
			if err != nil {
				t.Fatal(err)
			}
			cookie, status := f.cookie, http.StatusBadRequest
			switch change {
			case "same-user-new-session":
				cookie = f.localSession(t, f.owner.ID)
			case "different-user-session":
				other, createErr := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: "different-user", DisplayName: "Other", Role: store.UserRoleMember})
				if createErr != nil {
					t.Fatal(createErr)
				}
				cookie = f.localSession(t, other.ID)
			case "logout":
				err = f.s.store.RevokeSession(ctx, session.ID, "test", time.Now().UTC())
				status = http.StatusUnauthorized
			case "disabled":
				_, err = f.s.store.DB().ExecContext(ctx, `UPDATE users SET status='disabled', disabled_at=now() WHERE id=$1`, f.owner.ID)
				status = http.StatusUnauthorized
			case "session-expired":
				_, err = f.s.store.DB().ExecContext(ctx, `UPDATE sessions SET created_at=now()-interval '2 hours', idle_expires_at=now()-interval '1 hour' WHERE id=$1`, session.ID)
				status = http.StatusUnauthorized
			case "reauth-expired":
				_, err = f.s.store.DB().ExecContext(ctx, `UPDATE sessions SET recently_verified_at=now()-interval '6 minutes' WHERE id=$1`, session.ID)
				status = http.StatusForbidden
			case "original-proof-expired":
				// A fresh proof on the same session must not extend the deadline
				// captured by the original binding transaction.
				err = f.s.store.MarkSessionVerified(ctx, session.ID, time.Now().UTC())
				f.s.oidcFlows.mu.Lock()
				for key, flow := range f.s.oidcFlows.entries {
					flow.VerifiedUntil = time.Now().Add(-time.Second)
					f.s.oidcFlows.entries[key] = flow
				}
				f.s.oidcFlows.mu.Unlock()
			}
			if err != nil {
				t.Fatal(err)
			}
			w := f.request(t, "/admin/identity-link/confirm", "POST", cookie, browser, map[string]string{"flow_id": flowID}, status)
			assertNoOIDCSessionCookie(t, w)
			if f.provider.exchanges != 1 {
				t.Fatal("confirmation exchanged the authorization code again")
			}
			var links int
			if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM external_identities`).Scan(&links); err != nil || links != 0 {
				t.Fatalf("rejected confirmation changed binding data: count=%d err=%v", links, err)
			}
			f.assertNoExternalSessions(t)
			if change == "same-user-new-session" || change == "different-user-session" {
				current, err := f.s.oidcCurrentSession(cookieRequest(cookie))
				if err != nil || current.ID == "" || current.ID == session.ID {
					t.Fatalf("replacement local session changed: %+v %v", current, err)
				}
				// The mismatch also consumes the confirmation; switching back
				// to the original session cannot revive it.
				f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flowID}, http.StatusBadRequest)
			}
		})
	}
}

func TestOIDCHTTPPostgresBindingFlowsCannotIssueLoginSessions(t *testing.T) {
	for _, phase := range []string{"link-without-local-session", "confirmation-without-local-session", "confirmation-with-local-session", "login-used-as-confirmation"} {
		t.Run(phase, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			var state, browser, cookie string
			switch phase {
			case "link-without-local-session":
				state, browser = f.begin(t, "link", f.cookie)
			case "confirmation-without-local-session", "confirmation-with-local-session":
				state, browser = f.preview(t)
				if phase == "confirmation-with-local-session" {
					cookie = f.cookie
				}
			case "login-used-as-confirmation":
				state, browser = f.begin(t, "login", "")
			}
			calls := f.provider.exchanges
			var w *httptest.ResponseRecorder
			if phase == "login-used-as-confirmation" {
				w = f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": state}, http.StatusBadRequest)
			} else {
				w = f.request(t, "/auth/oidc/complete", "POST", cookie, browser, map[string]string{"state": state, "code": "synthetic-code"}, http.StatusBadRequest)
			}
			assertNoOIDCSessionCookie(t, w)
			if f.provider.exchanges != calls {
				t.Fatal("flow purpose mismatch reached the provider")
			}
			f.assertNoExternalSessions(t)
			if _, err := f.s.store.GetExternalIdentity(context.Background(), f.owner.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("flow purpose mismatch wrote binding: %v", err)
			}
			// Purpose errors are terminal even when the request is retried
			// with the original browser and original local identity.
			f.request(t, "/auth/oidc/complete", "POST", f.cookie, browser, map[string]string{"state": state, "code": "synthetic-code"}, http.StatusBadRequest)
		})
	}
}

func TestOIDCHTTPPostgresLocalLoginBetweenBeginAndCompleteCannotSwitchUser(t *testing.T) {
	for _, target := range []string{"same-user", "different-user"} {
		t.Run(target, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			ctx := context.Background()
			flow, browser := f.preview(t)
			f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, http.StatusOK)
			state, browser := f.begin(t, "login", "")
			userID := f.owner.ID
			if target == "different-user" {
				other, err := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: "other-local-user", DisplayName: "Other", Role: store.UserRoleMember})
				if err != nil {
					t.Fatal(err)
				}
				userID = other.ID
			}
			cookie := f.localSession(t, userID)
			before, err := f.s.oidcCurrentSession(cookieRequest(cookie))
			if err != nil {
				t.Fatal(err)
			}
			calls := f.provider.exchanges
			w := f.request(t, "/auth/oidc/complete", "POST", cookie, browser, map[string]string{"state": state, "code": "valid-linked-code"}, http.StatusConflict)
			assertNoOIDCSessionCookie(t, w)
			if f.provider.exchanges != calls {
				t.Fatal("existing local login was checked only after provider exchange")
			}
			after, err := f.s.oidcCurrentSession(cookieRequest(cookie))
			if err != nil || after.ID != before.ID || after.UserID != userID || after.ExternalIdentityID != nil {
				t.Fatalf("external response replaced current local login: %+v %v", after, err)
			}
			f.assertNoExternalSessions(t)
			f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "valid-linked-code"}, http.StatusBadRequest)
		})
	}
}

func (f oidcHTTPFixture) setLocalPassword(t *testing.T) {
	t.Helper()
	session, err := f.s.oidcCurrentSession(cookieRequest(f.cookie))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.identity.SetPassword(context.Background(), f.owner.ID, session.ID, "local-password-123"); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCHTTPPostgresCancelsLoginDuringExchange(t *testing.T) {
	for _, operation := range []string{"local-login", "logout"} {
		t.Run(operation, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			f.setLocalPassword(t)
			flow, browser := f.preview(t)
			f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 200)
			state, browser := f.begin(t, "login", "")
			var localCookie string
			f.provider.beforeExchange = func() {
				if operation == "local-login" {
					w := f.request(t, "/auth/password/login", "POST", "", browser,
						map[string]string{"username": f.owner.Username, "password": "local-password-123"}, 200)
					localCookie = oidcResponseCookie(t, w, sessionCookieName)
				} else {
					f.request(t, "/auth/logout", "POST", f.cookie, browser, nil, 200)
				}
			}
			w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "late-exchange"}, 400)
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == sessionCookieName && cookie.MaxAge > 0 {
					t.Fatal("cancelled login issued a cookie")
				}
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("cancelled response overwrote a newer browser transaction cookie")
			}
			var externalSessions int
			if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM sessions WHERE external_identity_id IS NOT NULL`).Scan(&externalSessions); err != nil || externalSessions != 0 {
				t.Fatalf("cancelled exchange created a session: %d %v", externalSessions, err)
			}
			if operation == "local-login" {
				f.request(t, "/admin/state", "GET", localCookie, "", nil, 200)
			}
		})
	}
}

func TestOIDCHTTPPostgresRevokesIssuedLoginBeforeRacingLocalResponse(t *testing.T) {
	for _, operation := range []string{"local-login", "logout"} {
		t.Run(operation, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			f.setLocalPassword(t)
			flow, browser := f.preview(t)
			f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 200)
			state, browser := f.begin(t, "login", "")
			w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "issued-login"}, 200)
			externalCookie := oidcResponseCookie(t, w, sessionCookieName)
			// This request captured the transaction cookie before the OIDC
			// response arrived. Response arrival order must not restore access.
			if operation == "local-login" {
				w = f.request(t, "/auth/password/login", "POST", "", browser,
					map[string]string{"username": f.owner.Username, "password": "local-password-123"}, 200)
				localCookie := oidcResponseCookie(t, w, sessionCookieName)
				f.request(t, "/admin/state", "GET", localCookie, "", nil, 200)
			} else {
				f.request(t, "/auth/logout", "POST", f.cookie, browser, nil, 200)
			}
			f.request(t, "/admin/state", "GET", externalCookie, "", nil, 401)
		})
	}
}

func TestOIDCHTTPPostgresLocalLoginCancelsPreviewAndInflightLink(t *testing.T) {
	for _, stage := range []string{"exchange", "confirmation"} {
		t.Run(stage, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			f.setLocalPassword(t)
			login := func(browser string) {
				f.request(t, "/auth/password/login", "POST", "", browser,
					map[string]string{"username": f.owner.Username, "password": "local-password-123"}, 200)
			}
			if stage == "exchange" {
				state, browser := f.begin(t, "link", f.cookie)
				f.provider.beforeExchange = func() { login(browser) }
				f.request(t, "/auth/oidc/complete", "POST", f.cookie, browser, map[string]string{"state": state, "code": "code"}, 400)
			} else {
				flow, browser := f.preview(t)
				login(browser)
				f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 400)
			}
			// Password login preserves the original session, so rejection must
			// come from the browser transaction's shared cancellation state.
			f.request(t, "/admin/state", "GET", f.cookie, "", nil, 200)
			if _, err := f.s.store.GetExternalIdentity(context.Background(), f.owner.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("cancelled binding wrote a mapping")
			}
		})
	}
}

func TestOIDCHTTPPostgresStaleResponsesLeaveNewTransactionCookie(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	state, browser := f.begin(t, "login", "")
	// A second begin cancels the first flow and installs a new cookie. Neither
	// the old response nor a replay may delete that new cookie by name.
	f.request(t, "/auth/oidc/login", "POST", "", browser, nil, 200)
	w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "late"}, 400)
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("stale completion modified cookies")
	}
	flow, browser := f.preview(t)
	f.request(t, "/admin/identity-link/begin", "POST", f.cookie, browser, nil, 200)
	for _, path := range []string{"/admin/identity-link/confirm", "/admin/identity-link/cancel"} {
		w := f.request(t, path, "POST", f.cookie, browser, map[string]string{"flow_id": flow}, 400)
		if len(w.Result().Cookies()) != 0 {
			t.Fatalf("stale %s modified cookies", path)
		}
	}
}
