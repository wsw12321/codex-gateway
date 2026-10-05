//go:build integration

package server

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func oidcRegistrationUserCount(t *testing.T, f oidcHTTPFixture) int {
	t.Helper()
	var count int
	if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOIDCHTTPPostgresRegistrationProfileRetry(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	before := oidcRegistrationUserCount(t, f)
	flowID, browser := f.registrationChoice(t)
	f.s.oidcFlows.mu.Lock()
	original := f.s.oidcFlows.entries[sha256.Sum256([]byte(flowID))]
	f.s.oidcFlows.mu.Unlock()
	for index, input := range []map[string]string{
		{}, {"username": "new-member"}, {"display_name": "会员"},
		{"username": "ab", "display_name": "会员"},
		{"username": "1member", "display_name": "会员"},
		{"username": "member.name", "display_name": "会员"},
		{"username": strings.Repeat("a", 33), "display_name": "会员"},
		{"username": "new-member", "display_name": " \t "},
		{"username": "new-member", "display_name": strings.Repeat("名", 81)},
		{"username": strings.ToUpper(f.owner.Username), "display_name": "会员"},
	} {
		input["flow_id"] = flowID
		status, code := 400, "invalid_profile"
		if strings.EqualFold(input["username"], f.owner.Username) {
			status, code = 409, "username_taken"
		}
		w := f.request(t, "/auth/oidc/register", "POST", "", browser, input, status)
		assertNoOIDCSessionCookie(t, w)
		value := invitationResponse(t, w)
		next, _ := value["flow_id"].(string)
		expires, _ := time.Parse(time.RFC3339Nano, value["expires_at"].(string))
		if next == "" || next == flowID || value["error"].(map[string]any)["code"] != code || !expires.Equal(original.Expires) {
			t.Fatalf("unsafe retry response: %s", w.Body)
		}
		if index == 0 {
			// One HTTP replay proves rejection without consuming the public
			// endpoint's rate budget for every invalid-profile test case.
			f.request(t, "/auth/oidc/register", "POST", "", browser, oidcRegistrationInput(flowID), 400)
		}
		f.s.oidcFlows.mu.Lock()
		retry := f.s.oidcFlows.entries[sha256.Sum256([]byte(next))]
		f.s.oidcFlows.mu.Unlock()
		if retry.VerifiedAt != original.VerifiedAt || retry.originalKey != original.originalKey || retry.Expires != original.Expires {
			t.Fatal("retry extended proof, deadline or replaced cancellation state")
		}
		if count := oidcRegistrationUserCount(t, f); count != before {
			t.Fatalf("invalid profile left an account: %d want %d", count, before)
		}
		flowID = next
	}
	input := oidcRegistrationInput(flowID)
	input["display_name"] = " " + strings.Repeat("名", 80) + " "
	w := f.request(t, "/auth/oidc/register", "POST", "", browser, input, 200)
	session, err := f.s.oidcCurrentSession(cookieRequest(oidcResponseCookie(t, w, sessionCookieName)))
	if err != nil || session.RecentlyVerifiedAt == nil || !session.RecentlyVerifiedAt.Equal(original.VerifiedAt) {
		t.Fatalf("successful retry changed verification time: %+v %v", session, err)
	}
	user, err := f.s.store.GetUser(context.Background(), session.UserID)
	if err != nil || user.Username != "sso-member" || user.DisplayName != strings.Repeat("名", 80) || oidcRegistrationUserCount(t, f) != before+1 {
		t.Fatalf("chosen profile was not saved: %+v %v", user, err)
	}
	f.request(t, "/auth/oidc/register", "POST", "", browser, input, 400)
}

func TestOIDCHTTPPostgresRegistrationRetryCancellationAndExpiry(t *testing.T) {
	for _, ending := range []string{"cancel-original-state", "expire"} {
		t.Run(ending, func(t *testing.T) {
			f := newOIDCHTTPFixture(t)
			before := oidcRegistrationUserCount(t, f)
			state, browser := f.begin(t, "login", "")
			value := invitationResponse(t, f.request(t, "/auth/oidc/complete", "POST", "", browser,
				map[string]string{"state": state, "code": "new-account"}, 200))
			flowID := value["flow_id"].(string)
			for range 3 {
				value = invitationResponse(t, f.request(t, "/auth/oidc/register", "POST", "", browser, map[string]string{"flow_id": flowID}, 400))
				flowID = value["flow_id"].(string)
			}
			if ending == "cancel-original-state" {
				f.request(t, "/auth/oidc/cancel", "POST", "", browser, map[string]string{"flow_id": state}, 200)
			} else {
				f.s.oidcFlows.mu.Lock()
				key := sha256.Sum256([]byte(flowID))
				flow := f.s.oidcFlows.entries[key]
				flow.Expires = time.Now().Add(-time.Second)
				f.s.oidcFlows.entries[key] = flow
				f.s.oidcFlows.mu.Unlock()
			}
			w := f.request(t, "/auth/oidc/register", "POST", "", browser, oidcRegistrationInput(flowID), 400)
			if strings.Contains(w.Body.String(), "flow_id") || oidcRegistrationUserCount(t, f) != before {
				t.Fatalf("ended flow was retryable or created user: %s", w.Body)
			}
		})
	}
}

func TestOIDCHTTPPostgresRegistrationStrictInputAndStorageFailure(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	before := oidcRegistrationUserCount(t, f)
	for _, field := range []string{"user_id", "role", "subject", "issuer", "balance", "password"} {
		flowID, browser := f.registrationChoice(t)
		input := oidcRegistrationInput(flowID)
		input[field] = "attacker"
		w := f.request(t, "/auth/oidc/register", "POST", "", browser, input, 400)
		if strings.Contains(w.Body.String(), "flow_id") || oidcRegistrationUserCount(t, f) != before {
			t.Fatalf("unexpected fields were accepted: %s", w.Body)
		}
	}
	flowID, browser := f.registrationChoice(t)
	input := oidcRegistrationInput(flowID)
	input["display_name"] = strings.Repeat("a", 4096)
	f.request(t, "/auth/oidc/register", "POST", "", browser, input, 413)
	if oidcRegistrationUserCount(t, f) != before {
		t.Fatal("oversize request created user")
	}
	if _, err := f.s.store.DB().ExecContext(context.Background(), `ALTER TABLE external_identities RENAME TO unavailable_external_identities`); err != nil {
		t.Fatal(err)
	}
	w := f.request(t, "/auth/oidc/register", "POST", "", browser, oidcRegistrationInput(flowID), 500)
	if strings.Contains(w.Body.String(), "flow_id") || oidcRegistrationUserCount(t, f) != before {
		t.Fatalf("storage failure was retryable or created user: %s", w.Body)
	}
	f.request(t, "/auth/oidc/register", "POST", "", browser, oidcRegistrationInput(flowID), 400)
}

func TestOIDCHTTPPostgresRegistrationAllowsDuplicateDisplayName(t *testing.T) {
	f := newOIDCHTTPFixture(t)
	first, _ := f.register(t)
	f.provider.external.Subject = "second-sso-subject"
	flowID, browser := f.registrationChoice(t)
	input := oidcRegistrationInput(flowID)
	input["username"] = "another-member"
	w := f.request(t, "/auth/oidc/register", "POST", "", browser, input, 200)
	second, err := f.s.oidcCurrentSession(cookieRequest(oidcResponseCookie(t, w, sessionCookieName)))
	if err != nil || second.UserID == first.UserID {
		t.Fatalf("distinct users were merged: %+v %v", second, err)
	}
	user, err := f.s.store.GetUser(context.Background(), second.UserID)
	if err != nil || user.Username != "another-member" || user.DisplayName != "新会员" {
		t.Fatalf("duplicate display name failed: %+v %v", user, err)
	}
}
