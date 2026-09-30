//go:build integration

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/store"
)

// invitationTestAuthenticator creates real ES256 registration data and signed
// discoverable assertions, so the HTTP tests exercise WebAuthn verification.
type invitationTestAuthenticator struct {
	key          *ecdsa.PrivateKey
	credentialID []byte
	userHandle   string
	counter      uint32
}

func newInvitationTestAuthenticator(t *testing.T, ceremony map[string]any) (*invitationTestAuthenticator, map[string]any) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := &invitationTestAuthenticator{key: key, credentialID: make([]byte, 32)}
	if _, err := rand.Read(a.credentialID); err != nil {
		t.Fatal(err)
	}
	options := ceremony["options"].(map[string]any)["publicKey"].(map[string]any)
	a.userHandle = options["user"].(map[string]any)["id"].(string)
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": options["challenge"], "origin": "https://gateway.example"})
	if err != nil {
		t.Fatal(err)
	}
	cose, err := cbor.Marshal(map[int]any{
		1: 2, 3: -7, -1: 1,
		-2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	authData := a.authenticatorData(0x45)            // User present, verified, attested credential.
	authData = append(authData, make([]byte, 16)...) // Anonymous AAGUID for none attestation.
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.credentialID)))
	authData = append(authData, a.credentialID...)
	authData = append(authData, cose...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": authData, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	encodedID := base64.RawURLEncoding.EncodeToString(a.credentialID)
	return a, map[string]any{
		"id": encodedID, "rawId": encodedID, "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"attestationObject": base64.RawURLEncoding.EncodeToString(attestation),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults": map[string]any{"credProps": map[string]any{"rk": true}},
	}
}

func (a *invitationTestAuthenticator) authenticatorData(flags byte) []byte {
	rpHash := sha256.Sum256([]byte("gateway.example"))
	data := append([]byte(nil), rpHash[:]...)
	data = append(data, flags)
	return binary.BigEndian.AppendUint32(data, a.counter)
}

func (a *invitationTestAuthenticator) login(t *testing.T, f invitationHTTPFixture, status int) *httptest.ResponseRecorder {
	t.Helper()
	ceremony := invitationResponse(t, f.send(t, "POST", "/auth/login/begin", nil, "", 200))
	options := ceremony["options"].(map[string]any)["publicKey"].(map[string]any)
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": options["challenge"], "origin": "https://gateway.example"})
	if err != nil {
		t.Fatal(err)
	}
	a.counter++
	authData := a.authenticatorData(0x05) // User present and verified.
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	encodedID := base64.RawURLEncoding.EncodeToString(a.credentialID)
	return f.send(t, "POST", "/auth/login/finish", map[string]any{
		"flow_id": ceremony["flow_id"],
		"credential": map[string]any{
			"id": encodedID, "rawId": encodedID, "type": "public-key",
			"response": map[string]any{
				"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
				"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
				"signature":         base64.RawURLEncoding.EncodeToString(signature), "userHandle": a.userHandle,
			},
		},
	}, "", status)
}

func TestInvitationPasskeyHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	created := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{
		"kind": "member", "max_uses": 2, "requires_approval": true,
	}, f.cookie, 201))
	id, token := created["id"].(string), created["token"].(string)
	ceremony := invitationResponse(t, f.send(t, "POST", "/auth/register/begin", map[string]any{
		"invitation_token": token, "username": "pending-passkey", "display_name": "Pending Passkey",
	}, "", 200))
	authenticator, credential := newInvitationTestAuthenticator(t, ceremony)
	registered := f.send(t, "POST", "/auth/register/finish", map[string]any{"flow_id": ceremony["flow_id"], "credential": credential}, "", 201)
	result := invitationResponse(t, registered)
	if result["status"] != "pending" || result["requires_approval"] != true || len(registered.Result().Cookies()) != 0 || len(result["recovery_codes"].([]any)) == 0 {
		t.Fatalf("pending Passkey registration response: %s", registered.Body)
	}
	user, err := f.s.store.GetUserByUsername(ctx, "pending-passkey")
	if err != nil || user.Status != store.StatusPending {
		t.Fatalf("pending Passkey account: %+v %v", user, err)
	}
	if w := authenticator.login(t, f, 400); len(w.Result().Cookies()) != 0 {
		t.Fatal("pending Passkey login issued a cookie")
	}
	var sessions, passkeys int
	if err := f.s.store.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM sessions WHERE user_id=$1),(SELECT count(*) FROM webauthn_credentials WHERE user_id=$1)`, user.ID).Scan(&sessions, &passkeys); err != nil || sessions != 0 || passkeys != 1 {
		t.Fatalf("pending Passkey artifacts: sessions=%d passkeys=%d err=%v", sessions, passkeys, err)
	}
	applications, err := f.s.store.ListInvitationApplications(ctx, id, 50, 0)
	if err != nil || len(applications) != 1 {
		t.Fatalf("pending Passkey application: %+v %v", applications, err)
	}
	f.send(t, "POST", "/admin/invitations/"+id+"/review", map[string]any{"application_ids": []string{applications[0].ID}, "decision": "approve"}, f.cookie, 200)
	login := authenticator.login(t, f, 200)
	if len(login.Result().Cookies()) != 1 {
		t.Fatal("approved original Passkey did not create a session")
	}
	memberCookie := login.Result().Cookies()[0].Value
	group, err := f.s.store.PutGroup(ctx, store.PutGroupParams{
		BillingWriteParams: store.BillingWriteParams{ActorUserID: f.owner.ID, OperationID: uuid.NewString(), Reason: "Passkey group invitation"},
		Name:               "Passkey group", LimitUSD: "10", Period: "day",
	})
	if err != nil {
		t.Fatal(err)
	}
	groupCreated := invitationResponse(t, f.send(t, "POST", "/admin/invitations", map[string]any{"kind": "group", "group_id": group.ID, "max_uses": 2, "requires_approval": true}, f.cookie, 201))
	groupToken, groupInvitationID := groupCreated["token"].(string), groupCreated["id"].(string)
	f.send(t, "POST", "/auth/register/begin", map[string]any{"invitation_token": groupToken, "username": "group-cannot-register", "display_name": "Invalid Registration"}, "", 400)
	f.send(t, "POST", "/auth/password/register", map[string]any{"invitation_token": groupToken, "username": "group-cannot-register", "display_name": "Invalid Registration", "password": "test-password-123"}, "", 400)
	f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": groupToken}, "", 401)
	joined := invitationResponse(t, f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": groupToken}, memberCookie, 200))
	if joined["status"] != "pending" {
		t.Fatalf("Passkey group application: %+v", joined)
	}
	applicationID := joined["application"].(map[string]any)["id"].(string)
	f.send(t, "POST", "/admin/invitations/"+groupInvitationID+"/review", map[string]any{"application_ids": []string{applicationID}, "decision": "approve"}, f.cookie, 200)
	state, err := f.s.store.GetBillingState(ctx, user.ID, 50, 0)
	if err != nil || state.Group == nil || state.Group.ID != group.ID {
		t.Fatalf("approved Passkey user group: %+v %v", state.Group, err)
	}
	f.send(t, "POST", "/auth/logout", nil, memberCookie, 200)
	f.send(t, "POST", "/auth/invitations/join", map[string]any{"invitation_token": groupToken}, memberCookie, 401)
}
