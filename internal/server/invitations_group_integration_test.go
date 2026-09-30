//go:build integration

package server

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestInvitationGroupPasswordHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	created := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "member"}, f.cookie, 201))
	registered := f.send(t, "POST", "/auth/password/register", map[string]any{
		"invitation_token": created["token"], "username": "group-password", "display_name": "Group Password", "password": "group-password-123",
	}, "", 201)
	if len(registered.Result().Cookies()) != 1 {
		t.Fatal("default member invitation did not establish a session")
	}
	f.send(t, "POST", "/auth/logout", nil, registered.Result().Cookies()[0].Value, 200)
	user, err := f.s.store.GetUserByUsername(ctx, "group-password")
	if err != nil {
		t.Fatal(err)
	}
	group, err := f.s.store.PutGroup(ctx, store.PutGroupParams{
		BillingWriteParams: store.BillingWriteParams{ActorUserID: f.owner.ID, OperationID: uuid.NewString(), Reason: "Password group invitation"},
		Name:               "Password group", LimitUSD: "10", Period: "day",
	})
	if err != nil {
		t.Fatal(err)
	}
	groupInvitation := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "group", "group_id": group.ID, "max_uses": 3}, f.cookie, 201))
	token, id := groupInvitation["token"].(string), groupInvitation["id"].(string)
	inspected := invitationResponse(t, f.send(t, "POST", "/auth/invitations/inspect", map[string]any{"invitation_token": token}, "", 200))
	if inspected["kind"] != "group" || inspected["group_id"] != group.ID || inspected["group_name"] != group.Name {
		t.Fatalf("public group metadata: %+v", inspected)
	}
	login := f.send(t, "POST", "/auth/password/login", map[string]any{"username": user.Username, "password": "group-password-123"}, "", 200)
	if len(login.Result().Cookies()) != 1 {
		t.Fatal("password login did not establish a session")
	}
	cookie := login.Result().Cookies()[0].Value
	state, err := f.s.store.GetBillingState(ctx, user.ID, 50, 0)
	if err != nil || state.Group != nil {
		t.Fatalf("login joined a group without confirmation: %+v %v", state.Group, err)
	}
	joined := invitationResponse(t, f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": token}, cookie, 200))
	if joined["status"] != "approved" {
		t.Fatalf("automatic group admission: %+v", joined)
	}
	applicationID := joined["application"].(map[string]any)["id"]
	replay := invitationResponse(t, f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": token}, cookie, 200))
	if replay["application"].(map[string]any)["id"] != applicationID {
		t.Fatal("repeated join consumed another slot")
	}
	state, err = f.s.store.GetBillingState(ctx, user.ID, 50, 0)
	if err != nil || state.Group == nil || state.Group.ID != group.ID {
		t.Fatalf("password join failed: %+v %v", state.Group, err)
	}
	if _, err := f.s.store.SetGroupMembers(ctx, store.SetGroupMembersParams{
		BillingWriteParams: store.BillingWriteParams{ActorUserID: f.owner.ID, OperationID: uuid.NewString(), Reason: "Remove joined member"},
		GroupID:            group.ID, UserIDs: []string{user.ID}, Action: "remove",
	}); err != nil {
		t.Fatal(err)
	}
	f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": token}, cookie, 200)
	state, err = f.s.store.GetBillingState(ctx, user.ID, 50, 0)
	if err != nil || state.Group != nil {
		t.Fatalf("old application rejoined removed member: %+v %v", state.Group, err)
	}
	applications, err := f.s.store.ListInvitationApplications(ctx, id, 50, 0)
	if err != nil || len(applications) != 1 || applications[0].ID != applicationID || applications[0].Status != "approved" {
		t.Fatalf("replay changed application history: %+v %v", applications, err)
	}
}
