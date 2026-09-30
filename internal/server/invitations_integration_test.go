//go:build integration

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

type invitationHTTPFixture struct {
	s      *Server
	owner  store.User
	cookie string
}

func newInvitationHTTPFixture(t *testing.T) invitationHTTPFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pgConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*pgConfig)
	schema := "invitation_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	pgConfig.RuntimeParams["search_path"] = schema
	pgConfig.RuntimeParams["statement_timeout"] = "15000"
	repository := store.New(stdlib.OpenDB(*pgConfig))
	t.Cleanup(func() {
		repository.Close()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	publicURL, _ := url.Parse("https://gateway.example")
	s, err := New(config.Config{
		PublicURL: publicURL, SidecarURL: publicURL, RPID: "gateway.example", RPOrigins: []string{publicURL.String()},
		TokenPepper: []byte(strings.Repeat("p", 32)), SessionIdle: time.Hour, SessionMax: 24 * time.Hour,
		ReauthMaxAge: 5 * time.Minute,
	}, repository, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := repository.CreateUser(ctx, store.CreateUserParams{Username: "invite-owner", DisplayName: "Owner", Role: store.UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	token, err := security.GenerateOpaqueToken(security.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := security.PepperTokenDigest(s.config.TokenPepper, token.Digest)
	now := time.Now().UTC()
	session, err := repository.CreateSession(ctx, store.CreateSessionParams{
		UserID: owner.ID, TokenHash: digest[:], CSRFSecret: []byte(strings.Repeat("c", 32)),
		CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkSessionVerified(ctx, session.ID, now); err != nil {
		t.Fatal(err)
	}
	return invitationHTTPFixture{s: s, owner: owner, cookie: token.Token}
}

func (f invitationHTTPFixture) send(t *testing.T, method, path string, body any, cookie string, status int) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r.Header.Set("Origin", "https://gateway.example")
	r.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, status, w.Body)
	}
	return w
}

func invitationResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInvitationPasswordHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	for _, body := range []map[string]any{
		{"max_uses": 0}, {"max_uses": int64(1) << 31}, {"kind": "group"},
		{"kind": "owner_bootstrap"}, {"expires_at": time.Now().Add(-time.Hour)},
	} {
		f.send(t, "POST", "/admin/invitations", body, f.cookie, 400)
	}
	created := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{
		"kind": "member", "max_uses": 2, "requires_approval": true, "expires_at": time.Now().Add(72 * time.Hour),
	}, f.cookie, 201))
	id, token := created["id"].(string), created["token"].(string)
	inspected := f.send(t, "POST", "/auth/invitations/inspect", map[string]any{"invitation_token": token}, "", 200)
	if strings.Contains(inspected.Body.String(), token) || strings.Contains(inspected.Body.String(), "token_hash") {
		t.Fatal("inspection leaked invitation secret")
	}
	register := func(username string) (store.User, []any) {
		t.Helper()
		w := f.send(t, "POST", "/auth/password/register", map[string]any{
			"invitation_token": token, "username": username, "display_name": "Pending " + username, "password": "test-password-123",
		}, "", 201)
		result := invitationResponse(t, w)
		if result["status"] != "pending" || result["requires_approval"] != true || len(w.Result().Cookies()) != 0 {
			t.Fatalf("pending response created session: %s", w.Body)
		}
		user, err := f.s.store.GetUserByUsername(ctx, username)
		if err != nil || user.Status != store.StatusPending {
			t.Fatalf("pending user: %+v %v", user, err)
		}
		return user, result["recovery_codes"].([]any)
	}
	first, codes := register("approval-first")
	second, _ := register("approval-second")
	f.send(t, "POST", "/auth/password/login", map[string]any{"username": first.Username, "password": "test-password-123"}, "", 401)
	f.send(t, "POST", "/auth/password/recovery", map[string]any{"username": first.Username, "recovery_code": codes[0], "password": "new-password-123"}, "", 400)
	f.send(t, "POST", "/auth/recovery/begin", map[string]any{"username": first.Username, "recovery_code": codes[0]}, "", 400)
	f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "recovery", "target_username": first.Username}, f.cookie, 400)
	f.send(t, "POST", "/auth/password/register", map[string]any{"invitation_token": token, "username": "over-capacity", "display_name": "Full", "password": "test-password-123"}, "", 400)
	applications, err := f.s.store.ListInvitationApplications(ctx, id, 50, 0)
	if err != nil || len(applications) != 2 {
		t.Fatalf("applications: %+v %v", applications, err)
	}
	ids := map[string]string{}
	for _, application := range applications {
		ids[application.Username] = application.ID
	}
	var sessions, linkedAudits, privateAudit int
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE user_id IN ($1,$2)`, first.ID, second.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id IN ($1::uuid,$2::uuid) OR subject_id IN ($1::text,$2::text)`, first.ID, second.ID).Scan(&linkedAudits); err != nil {
		t.Fatal(err)
	}
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE event_type='invitation.applied' AND (actor_user_id IS NOT NULL OR source_ip IS NOT NULL OR metadata::text LIKE '%approval-%')`).Scan(&privateAudit); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || linkedAudits != 0 || privateAudit != 0 {
		t.Fatalf("pending artifacts sessions=%d audit=%d private=%d", sessions, linkedAudits, privateAudit)
	}
	review := func(applicationID, decision string, want int) {
		f.send(t, "POST", "/admin/invitations/"+id+"/review", map[string]any{"application_ids": []string{applicationID}, "decision": decision}, f.cookie, want)
	}
	review(ids[second.Username], "reject", 200)
	replacement, _ := register(second.Username)
	if replacement.ID == second.ID {
		t.Fatal("re-registration reused deleted identity")
	}
	review(ids[second.Username], "reject", 200)
	review(ids[second.Username], "approve", 409)
	if _, err := f.s.store.GetUser(ctx, replacement.ID); err != nil {
		t.Fatalf("old rejection affected replacement: %v", err)
	}
	f.send(t, "POST", "/admin/invitations/"+id+"/revoke", nil, f.cookie, 200)
	review(ids[first.Username], "approve", 200)
	review(ids[first.Username], "approve", 200)
	login := f.send(t, "POST", "/auth/password/login", map[string]any{"username": first.Username, "password": "test-password-123"}, "", 200)
	if len(login.Result().Cookies()) != 1 {
		t.Fatal("approved original password did not establish a session")
	}
	listing := f.send(t, "GET", "/admin/invitations?limit=50&offset=0", nil, f.cookie, 200)
	if strings.Contains(listing.Body.String(), token) || strings.Contains(listing.Body.String(), "token_hash") {
		t.Fatal("invitation list leaked secret")
	}
	f.send(t, "GET", "/admin/invitations/"+id+"/applications", nil, f.cookie, 200)
}

func TestInvitationGroupHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	memberInvite := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{}, f.cookie, 201))
	if memberInvite["max_uses"] != float64(1) || memberInvite["requires_approval"] != false {
		t.Fatalf("legacy defaults changed: %+v", memberInvite)
	}
	registered := f.send(t, "POST", "/auth/password/register", map[string]any{
		"invitation_token": memberInvite["token"], "username": "group-member", "display_name": "Group Member", "password": "test-password-123",
	}, "", 201)
	if result := invitationResponse(t, registered); result["status"] != "approved" || len(registered.Result().Cookies()) != 1 {
		t.Fatalf("legacy registration failed: %s", registered.Body)
	}
	memberCookie := registered.Result().Cookies()[0].Value
	member, err := f.s.store.GetUserByUsername(ctx, "group-member")
	if err != nil {
		t.Fatal(err)
	}
	group, err := f.s.store.PutGroup(ctx, store.PutGroupParams{
		BillingWriteParams: store.BillingWriteParams{ActorUserID: f.owner.ID, OperationID: uuid.NewString(), Reason: "Invitation test group"},
		Name:               "Invited Group", LimitUSD: "10", Period: "month",
	})
	if err != nil {
		t.Fatal(err)
	}
	created := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{
		"kind": "group", "group_id": group.ID, "max_uses": 5, "requires_approval": true,
	}, f.cookie, 201))
	id, token := created["id"].(string), created["token"].(string)
	inspected := invitationResponse(t, f.send(t, "POST", "/auth/invitations/inspect", map[string]any{"invitation_token": token}, "", 200))
	if inspected["kind"] != "group" || inspected["group_name"] != group.Name || inspected["group_id"] != group.ID {
		t.Fatalf("group metadata: %+v", inspected)
	}
	for _, path := range []string{"/auth/password/register", "/auth/register/begin"} {
		body := map[string]any{"invitation_token": token, "username": "wrong-purpose", "display_name": "Wrong Purpose"}
		if strings.Contains(path, "password") {
			body["password"] = "test-password-123"
		}
		f.send(t, "POST", path, body, "", 400)
	}
	f.send(t, "POST", "/auth/password/recovery", map[string]any{"invitation_token": token, "password": "test-password-123"}, "", 400)
	f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": token}, "", 401)
	join := func() map[string]any {
		return invitationResponse(t, f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": token}, memberCookie, 200))
	}
	application := join()
	if application["status"] != "pending" {
		t.Fatalf("expected pending join: %+v", application)
	}
	applicationID := application["application"].(map[string]any)["id"].(string)
	if repeated := join(); repeated["application"].(map[string]any)["id"] != applicationID {
		t.Fatal("duplicate join created another application")
	}
	var appliedAudits int
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE subject_id=$1 AND event_type='invitation.applied'`, id).Scan(&appliedAudits); err != nil || appliedAudits != 1 {
		t.Fatalf("group application audit should occur once: %d %v", appliedAudits, err)
	}
	f.send(t, "POST", "/admin/invitations/"+id+"/review", map[string]any{"application_ids": []string{applicationID}, "decision": "approve"}, f.cookie, 200)
	if group, err = f.s.store.GetGroup(ctx, group.ID); err != nil || len(group.Members) != 1 || group.Members[0].UserID != member.ID {
		t.Fatalf("approved group: %+v %v", group, err)
	}
	_, err = f.s.store.SetGroupMembers(ctx, store.SetGroupMembersParams{
		BillingWriteParams: store.BillingWriteParams{ActorUserID: f.owner.ID, OperationID: uuid.NewString(), Reason: "Remove member"},
		GroupID:            group.ID, UserIDs: []string{member.ID}, Action: "remove",
	})
	if err != nil {
		t.Fatal(err)
	}
	join()
	f.send(t, "POST", "/admin/invitations/"+id+"/review", map[string]any{"application_ids": []string{applicationID}, "decision": "approve"}, f.cookie, 200)
	if group, err = f.s.store.GetGroup(ctx, group.ID); err != nil || len(group.Members) != 0 {
		t.Fatalf("replay rejoined removed member: %+v %v", group, err)
	}
}
