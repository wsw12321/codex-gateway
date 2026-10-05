//go:build integration

package server

import (
	"context"
	"testing"
)

func TestOIDCHTTPPostgresLateLoginCancellationAfterCookieCleared(t *testing.T) {
	for _, kind := range []string{"registration", "returning-login"} {
		t.Run(kind, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			if kind == "returning-login" {
				f.register(t)
			}
			state, browser := f.begin(t, "login", "")
			w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "valid"}, 200)
			flowID := ""
			if kind == "registration" {
				flowID = invitationResponse(t, w)["flow_id"].(string)
				w = f.request(t, "/auth/oidc/register", "POST", "", browser, oidcRegistrationInput(flowID), 200)
			}
			oidcAssertTransactionCookieCleared(t, w)
			cookie := oidcResponseCookie(t, w, sessionCookieName)
			session, err := f.s.oidcCurrentSession(cookieRequest(cookie))
			if err != nil || session.ID == "" {
				t.Fatalf("issued session missing: %+v %v", session, err)
			}
			newerCookie := f.localSession(t, session.UserID)
			// Neither another account nor a newer session of this account can
			// authorize cancellation after the browser transaction cookie clears.
			for _, wrongCookie := range []string{"", f.cookie, newerCookie} {
				f.request(t, "/auth/oidc/cancel", "POST", wrongCookie, "", map[string]string{"flow_id": state}, 400)
			}
			if flowID != "" {
				f.request(t, "/auth/oidc/cancel", "POST", cookie, "", map[string]string{"flow_id": flowID}, 400)
			}
			f.request(t, "/admin/state", "GET", cookie, "", nil, 200)
			cancelled := f.request(t, "/auth/oidc/cancel", "POST", cookie, "", map[string]string{"flow_id": state}, 200)
			if len(cancelled.Result().Cookies()) != 0 {
				t.Fatal("late cancellation altered browser cookies")
			}
			f.request(t, "/admin/state", "GET", cookie, "", nil, 401)
			f.request(t, "/admin/state", "GET", newerCookie, "", nil, 200)
			user, err := f.s.store.GetUser(context.Background(), session.UserID)
			if err != nil || user.ID != session.UserID {
				t.Fatalf("handoff cancellation removed the registered account: %+v %v", user, err)
			}
		})
	}
}
