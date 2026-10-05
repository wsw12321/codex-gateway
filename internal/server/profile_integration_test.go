//go:build integration

package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func profileHTTPMember(t *testing.T, f invitationHTTPFixture, username string) (store.User, store.Session, string) {
	t.Helper()
	ctx := context.Background()
	user, err := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: username, DisplayName: "Original display name"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := security.GenerateOpaqueToken(security.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := security.PepperTokenDigest(f.s.config.TokenPepper, token.Digest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session, err := f.s.store.CreateSession(ctx, store.CreateSessionParams{UserID: user.ID, TokenHash: digest[:], CSRFSecret: []byte(strings.Repeat("c", 32)), CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.store.MarkSessionVerified(ctx, session.ID, now); err != nil {
		t.Fatal(err)
	}
	return user, session, token.Token
}

func TestProfileHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	member, session, cookie := profileHTTPMember(t, f, "profile-http-member")
	password := "Original-test-password-1!"
	if err := f.s.identity.SetPassword(ctx, member.ID, session.ID, password); err != nil {
		t.Fatal(err)
	}
	other, err := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: "profile-http-occupied", DisplayName: "Shared display name"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.DB().ExecContext(ctx, `UPDATE sessions SET recently_verified_at=NULL WHERE id=$1`, session.ID); err != nil {
		t.Fatal(err)
	}
	f.send(t, "PATCH", "/admin/profile", map[string]any{"display_name": "  中文显示名称  "}, cookie, 200)
	f.send(t, "PATCH", "/admin/profile", map[string]any{"username": "profile-http-renamed"}, cookie, 403)
	f.send(t, "POST", "/auth/password/reauth", map[string]any{"password": password}, cookie, 200)
	conflict := f.send(t, "PATCH", "/admin/profile", map[string]any{"username": strings.ToUpper(other.Username), "display_name": "Must roll back"}, cookie, 409)
	if !strings.Contains(conflict.Body.String(), "username_taken") {
		t.Fatalf("wrong conflict: %s", conflict.Body)
	}
	current, err := f.s.store.GetUser(ctx, member.ID)
	if err != nil || current.DisplayName != "中文显示名称" || current.Username != member.Username {
		t.Fatalf("partial conflict save: %+v, %v", current, err)
	}
	response := invitationResponse(t, f.send(t, "PATCH", "/admin/profile", map[string]any{"username": "  Profile-HTTP-Renamed  ", "display_name": other.DisplayName}, cookie, 200))
	profile, ok := response["user"].(map[string]any)
	if !ok || profile["id"] != member.ID || profile["username"] != "profile-http-renamed" || profile["display_name"] != other.DisplayName || profile["role"] != store.UserRoleMember {
		t.Fatalf("saved profile response: %+v", response)
	}
	if result, err := f.s.identity.PasswordLogin(ctx, "profile-http-renamed", password, nil, ""); err != nil || result.User.ID != member.ID {
		t.Fatalf("new username login: %+v, %v", result, err)
	}
	if _, err := f.s.identity.PasswordLogin(ctx, member.Username, password, nil, ""); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("old username login survived: %v", err)
	}
	reused, err := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: member.Username, DisplayName: "Reused name"})
	if err != nil || reused.ID == member.ID {
		t.Fatalf("old username reuse: %+v, %v", reused, err)
	}
	if _, err := f.s.identity.PasswordLogin(ctx, member.Username, password, nil, ""); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("original credential followed old name: %v", err)
	}
	f.send(t, "PATCH", "/admin/profile", map[string]any{"display_name": "Still same session"}, cookie, 200)
	f.send(t, "PATCH", "/admin/profile", map[string]any{"username": "profile-owner-renamed", "display_name": "Owner changed"}, f.cookie, 200)
	legacy, legacySession, legacyCookie := profileHTTPMember(t, f, "water5_"+strings.Repeat("a", 32))
	if _, err := f.s.store.DB().ExecContext(ctx, `UPDATE sessions SET recently_verified_at=NULL WHERE id=$1`, legacySession.ID); err != nil {
		t.Fatal(err)
	}
	f.send(t, "PATCH", "/admin/profile", map[string]any{"display_name": "历史用户"}, legacyCookie, 200)
	if current, err := f.s.store.GetUser(ctx, legacy.ID); err != nil || current.Username != legacy.Username || current.DisplayName != "历史用户" {
		t.Fatalf("legacy HTTP update: %+v, %v", current, err)
	}
}

func TestRecoveryInvitationIDSurvivesRenamePostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	target, _, cookie := profileHTTPMember(t, f, "recovery-original-name")
	before := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "recovery", "target_user_id": target.ID}, f.cookie, 201))
	f.send(t, "PATCH", "/admin/profile", map[string]any{"username": "recovery-renamed"}, cookie, 200)
	reused, err := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: target.Username, DisplayName: "New name holder"})
	if err != nil {
		t.Fatal(err)
	}
	after := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "recovery", "target_user_id": target.ID}, f.cookie, 201))
	legacy := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "recovery", "target_username": target.Username}, f.cookie, 201))
	for _, test := range []struct {
		result map[string]any
		want   string
	}{{before, target.ID}, {after, target.ID}, {legacy, reused.ID}} {
		var actual string
		if err := f.s.store.DB().QueryRowContext(ctx, `SELECT target_user_id FROM invitations WHERE id=$1`, test.result["id"]).Scan(&actual); err != nil || actual != test.want {
			t.Fatalf("invitation target=%s want=%s err=%v", actual, test.want, err)
		}
	}
	f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "recovery", "target_user_id": target.ID, "target_username": target.Username}, f.cookie, 400)
	result, err := f.s.identity.RecoverWithPassword(ctx, "", "", before["token"].(string), "Recovered-test-password-1!", nil, "")
	if err != nil || result.User.ID != target.ID || result.User.Username != "recovery-renamed" {
		t.Fatalf("recovery followed reused name: %+v, %v", result, err)
	}
	if _, err := f.s.store.GetPasswordCredential(ctx, reused.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recovery changed reused-name account: %v", err)
	}
}
