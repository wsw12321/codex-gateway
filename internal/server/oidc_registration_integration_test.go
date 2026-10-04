//go:build integration

package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/store"
)

func (f oidcHTTPFixture) registrationChoice(t *testing.T) (string, string) {
	t.Helper()
	state, browser := f.begin(t, "login", "")
	w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "new-account"}, 200)
	assertNoOIDCSessionCookie(t, w)
	value := invitationResponse(t, w)
	if value["result"] != "registration_required" || value["flow_id"] == state || strings.Contains(w.Body.String(), f.provider.external.Subject) {
		t.Fatalf("unsafe provisioning choice: %s", w.Body)
	}
	return value["flow_id"].(string), browser
}

func (f oidcHTTPFixture) register(t *testing.T) (store.Session, string) {
	t.Helper()
	flow, browser := f.registrationChoice(t)
	w := f.request(t, "/auth/oidc/register", "POST", "", browser, map[string]string{"flow_id": flow}, 200)
	if invitationResponse(t, w)["result"] != "login" {
		t.Fatal("registration did not log in")
	}
	cookie := oidcResponseCookie(t, w, sessionCookieName)
	session, err := f.s.oidcCurrentSession(cookieRequest(cookie))
	if err != nil {
		t.Fatal(err)
	}
	return session, cookie
}

func TestOIDCHTTPPostgresRegistrationChoiceAndDefaults(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	ctx := context.Background()
	countUsers := func() int {
		t.Helper()
		var count int
		if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countUsers()
	flow, browser := f.registrationChoice(t)
	if countUsers() != before {
		t.Fatal("preview created an account")
	}
	oidcAssertTransactionCookieCleared(t, f.request(t, "/auth/oidc/register/cancel", "POST", "", browser, map[string]string{"flow_id": flow}, 200))
	f.request(t, "/auth/oidc/register", "POST", "", browser, map[string]string{"flow_id": flow}, 400)
	if countUsers() != before {
		t.Fatal("cancel created an account")
	}
	session, cookie := f.register(t)
	user, err := f.s.store.GetUser(ctx, session.UserID)
	if err != nil || user.Role != store.UserRoleMember || user.Status != store.StatusActive || user.Username == "" || user.ID == f.owner.ID {
		t.Fatalf("registration user %+v %v", user, err)
	}
	methods, err := f.s.store.LoginMethods(ctx, user.ID)
	if err != nil || methods.Passkey || methods.Password || !methods.OIDC || !oidcRecent(session, time.Now()) {
		t.Fatalf("SSO-only methods %+v %v", methods, err)
	}
	view := invitationResponse(t, f.request(t, "/admin/state", "GET", cookie, "", nil, 200))
	if view["login_methods"].(map[string]any)["oidc"] != true || view["recently_verified"] != true {
		t.Fatal("SSO state missing capabilities", view)
	}
	billing, err := f.s.store.GetBillingState(ctx, user.ID, 100, 0)
	if err != nil || billing.BalanceUSD != "0.000000000000" {
		t.Fatalf("initial balance %+v %v", billing, err)
	}
	var groups int
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM billing_accounts WHERE user_id=$1 AND group_id IS NOT NULL`, user.ID).Scan(&groups); err != nil || groups != 0 {
		t.Fatalf("automatic group membership %d %v", groups, err)
	}
	w := f.request(t, "/admin/identity-link", "DELETE", cookie, "", nil, 409)
	if !strings.Contains(w.Body.String(), "last_login_method") {
		t.Fatal(w.Body.String())
	}
	state, browser := f.begin(t, "login", "")
	w = f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "returning"}, 200)
	if invitationResponse(t, w)["result"] != "login" || countUsers() != before+1 {
		t.Fatal("returning SSO created or prompted again")
	}
	returning, err := f.s.oidcCurrentSession(cookieRequest(oidcResponseCookie(t, w, sessionCookieName)))
	if err != nil || returning.UserID != user.ID {
		t.Fatal("returning SSO changed account")
	}
	// Local methods are optional, but adding one permits unlinking.
	f.request(t, "/admin/password", "PUT", cookie, "", map[string]string{"password": "optional-password-123"}, 200)
	f.request(t, "/admin/identity-link", "DELETE", cookie, "", nil, 200)
	f.request(t, "/admin/state", "GET", cookie, "", nil, 401)
}

func TestOIDCHTTPPostgresRegistrationRetainsExchangeTime(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	flowID, browser := f.registrationChoice(t)
	verifiedAt := time.Now().UTC().Add(-6 * time.Minute).Truncate(time.Microsecond)
	f.s.oidcFlows.mu.Lock()
	key := sha256.Sum256([]byte(flowID))
	flow := f.s.oidcFlows.entries[key]
	flow.VerifiedAt = verifiedAt
	f.s.oidcFlows.entries[key] = flow
	f.s.oidcFlows.mu.Unlock()
	w := f.request(t, "/auth/oidc/register", "POST", "", browser, map[string]string{"flow_id": flowID}, 200)
	cookie := oidcResponseCookie(t, w, sessionCookieName)
	session, err := f.s.oidcCurrentSession(cookieRequest(cookie))
	if err != nil || session.RecentlyVerifiedAt == nil || !session.RecentlyVerifiedAt.Equal(verifiedAt) {
		t.Fatalf("choice refreshed proof: %+v %v", session, err)
	}
	f.request(t, "/admin/api-keys", "POST", cookie, "", nil, 403)
	f.request(t, "/auth/oidc/register", "POST", "", browser, map[string]string{"flow_id": flowID}, 400)
}

func TestOIDCHTTPPostgresRegistrationRejectsRaceAndChangedBrowser(t *testing.T) {
	for _, change := range []string{"local-login", "logout", "expired", "bound-elsewhere", "registered-elsewhere", "purpose", "extra-identity"} {
		t.Run(change, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			flow, browser := f.registrationChoice(t)
			status := 400
			input := map[string]string{"flow_id": flow}
			switch change {
			case "local-login":
				f.setLocalPassword(t)
				f.request(t, "/auth/password/login", "POST", "", browser, map[string]string{"username": f.owner.Username, "password": "local-password-123"}, 200)
			case "logout":
				f.request(t, "/auth/logout", "POST", f.cookie, browser, nil, 200)
			case "expired":
				f.s.oidcFlows.mu.Lock()
				key := sha256.Sum256([]byte(flow))
				entry := f.s.oidcFlows.entries[key]
				entry.Expires = time.Now().Add(-time.Second)
				f.s.oidcFlows.entries[key] = entry
				f.s.oidcFlows.mu.Unlock()
			case "bound-elsewhere":
				preview, localBrowser := f.preview(t)
				f.request(t, "/admin/identity-link/confirm", "POST", f.cookie, localBrowser, map[string]string{"flow_id": preview}, 200)
				status = 409
			case "registered-elsewhere":
				f.register(t)
				status = 409
			case "purpose":
				f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": flow, "code": "code"}, 400)
			case "extra-identity":
				input["subject"] = "attacker"
			}
			var before, after int
			if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM users`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			w := f.request(t, "/auth/oidc/register", "POST", "", browser, input, status)
			assertNoOIDCSessionCookie(t, w)
			if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM users`).Scan(&after); err != nil || after != before {
				t.Fatalf("failed registration left a user: %d %d %v", before, after, err)
			}
		})
	}
}

func TestOIDCHTTPPostgresSSOOnlySensitiveActions(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	session, cookie := f.register(t)
	f.s.config.KeyPepper = []byte(strings.Repeat("k", 32))
	f.s.config.APIKeyEncryptionKey = []byte(strings.Repeat("e", 32))
	f.s.config.BrowserClientURL, _ = url.Parse("https://client.example/")
	device, err := f.s.store.CreateDevice(context.Background(), store.CreateDeviceParams{UserID: session.UserID, Name: "SSO device"})
	if err != nil {
		t.Fatal(err)
	}
	key := invitationResponse(t, f.request(t, "/admin/api-keys", "POST", cookie, "", map[string]any{"name": "SSO key", "device_id": device.ID}, 201))
	revealed := invitationResponse(t, f.request(t, "/admin/api-keys/"+key["id"].(string)+"/reveal", "POST", cookie, "", nil, 200))
	if revealed["api_key"] != key["api_key"] {
		t.Fatal("SSO key reveal mismatch")
	}
	f.request(t, "/admin/browser-handoffs", "POST", cookie, "", map[string]any{"api_key_id": key["id"]}, 201)
	planResponse := f.request(t, "/admin/billing/plans", "POST", f.cookie, "", map[string]any{
		"operation_id": uuid.NewString(), "reason": "SSO regression", "name": "SSO daily", "price_usd": "1", "tier": "day", "allowance_usd": "5", "min_period_count": 1, "active": true,
	}, 200)
	var plan store.BillingPlan
	if err := json.Unmarshal(planResponse.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	purchase := map[string]any{"operation_id": uuid.NewString(), "plan_id": plan.ID, "plan_version": plan.Version, "subscription_config_version": 0, "period_count": 1}
	w := f.request(t, "/admin/billing/me/purchases", "POST", cookie, "", purchase, 400)
	if !strings.Contains(w.Body.String(), "insufficient_balance") {
		t.Fatalf("SSO failed authentication or default billing: %s", w.Body)
	}
	// Explicit funding is test setup; registration itself never grants credit.
	f.request(t, "/admin/billing/users/"+session.UserID+"/adjustments", "POST", f.cookie, "", map[string]any{"operation_id": uuid.NewString(), "reason": "SSO test funding", "usd_amount": "2"}, 200)
	purchase["operation_id"] = uuid.NewString()
	f.request(t, "/admin/billing/me/purchases", "POST", cookie, "", purchase, 200)
	if methods, err := f.s.store.LoginMethods(context.Background(), session.UserID); err != nil || methods.Password || methods.Passkey {
		t.Fatalf("sensitive action required local credential: %+v %v", methods, err)
	}
}

func TestOIDCHTTPPostgresUnavailableBoundAccountNeverOffersRegistration(t *testing.T) {
	for _, state := range []string{"disabled", "pending"} {
		t.Run(state, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			session, _ := f.register(t)
			if _, err := f.s.store.DB().ExecContext(context.Background(), `UPDATE users SET status=$2, disabled_at=CASE WHEN $2='disabled' THEN now() ELSE NULL END WHERE id=$1`, session.UserID, state); err != nil {
				t.Fatal(err)
			}
			flow, browser := f.begin(t, "login", "")
			w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": flow, "code": "disabled"}, 403)
			if strings.Contains(w.Body.String(), "flow_id") {
				t.Fatal("unavailable account got provisioning flow")
			}
			_, err := f.s.store.GetExternalIdentity(context.Background(), session.UserID)
			if errors.Is(err, store.ErrNotFound) {
				t.Fatal("binding lost")
			}
		})
	}
}

func TestOIDCHTTPPostgresBindingStorageFailureNeverOffersRegistration(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	ctx := context.Background()
	state, browser := f.begin(t, "login", "")
	var before, after int
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.DB().ExecContext(ctx, `ALTER TABLE external_identities RENAME TO unavailable_external_identities`); err != nil {
		t.Fatal(err)
	}
	w := f.request(t, "/auth/oidc/complete", "POST", "", browser, map[string]string{"state": state, "code": "valid"}, 500)
	assertNoOIDCSessionCookie(t, w)
	if strings.Contains(w.Body.String(), "flow_id") || strings.Contains(w.Body.String(), "registration_required") {
		t.Fatal("storage failure offered account creation")
	}
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&after); err != nil || before != after {
		t.Fatalf("storage failure created account: %d %d %v", before, after, err)
	}
	f.request(t, "/auth/oidc/register", "POST", "", browser, map[string]string{"flow_id": state}, 400)
}
