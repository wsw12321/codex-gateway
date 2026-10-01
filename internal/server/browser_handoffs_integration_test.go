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

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestBrowserHandoffPostgresHTTPIntegration(t *testing.T) {
	for _, change := range []string{"success", "logout", "disabled key", "deleted key", "disabled device", "disabled account"} {
		t.Run(change, func(t *testing.T) {
			f := newInvitationHTTPFixture(t)
			f.s.config.KeyPepper = []byte(strings.Repeat("k", 32))
			f.s.config.APIKeyEncryptionKey = []byte(strings.Repeat("e", 32))
			f.s.config.BrowserClientURL, _ = url.Parse("https://ai.example.test")
			ctx := context.Background()
			device, err := f.s.store.CreateDevice(ctx, store.CreateDeviceParams{UserID: f.owner.ID, Name: "网页工作台设备"})
			if err != nil {
				t.Fatal(err)
			}
			keyResponse := invitationResponse(t, f.send(t, "POST", "/admin/api-keys", map[string]any{"name": "网页工作台", "device_id": device.ID, "expires_days": 90, "models": []string{}}, f.cookie, 201))
			keyID := keyResponse["id"].(string)
			state := invitationResponse(t, f.send(t, "GET", "/admin/state", nil, f.cookie, 200))
			if state["browser_client_enabled"] != true {
				t.Fatal("management state did not enable browser client")
			}
			issue := invitationResponse(t, f.send(t, "POST", "/admin/browser-handoffs", map[string]any{"api_key_id": keyID, "remember_key": true}, f.cookie, 201))
			launch, _ := url.Parse(issue["launch_url"].(string))
			fragment, _ := url.ParseQuery(launch.Fragment)
			code := fragment.Get("code")
			switch change {
			case "logout":
				f.send(t, "POST", "/auth/logout", map[string]any{}, f.cookie, 200)
			case "disabled key":
				f.send(t, "PUT", "/admin/api-keys/"+keyID+"/status", map[string]any{"status": "disabled"}, f.cookie, 200)
			case "deleted key":
				f.send(t, "DELETE", "/admin/api-keys/"+keyID, map[string]any{}, f.cookie, 200)
			case "disabled device":
				err = f.s.store.DisableDevice(ctx, f.owner.ID, device.ID, time.Now())
			case "disabled account":
				err = f.s.store.DisableUser(ctx, f.owner.ID, time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			wrong := exchangeBrowserHandoffForTest(f.s, code, "https://other.example")
			if wrong.Code != 403 {
				t.Fatal("untrusted Origin accepted")
			}
			response := exchangeBrowserHandoffForTest(f.s, code, "https://ai.example.test")
			want := http.StatusForbidden
			if change == "success" {
				want = http.StatusOK
			}
			if response.Code != want {
				t.Fatalf("exchange status=%d want=%d body=%s", response.Code, want, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "https://ai.example.test" {
				t.Fatal("exchange lost no-store or CORS")
			}
			if change == "success" {
				body := invitationResponse(t, response)
				if body["api_key"] != keyResponse["api_key"] || body["api_key_id"] != keyID || body["remember_key"] != true {
					t.Fatal("exchange returned wrong key")
				}
			}
			if exchangeBrowserHandoffForTest(f.s, code, "https://ai.example.test").Code != 410 {
				t.Fatal("replayed code was accepted")
			}
		})
	}
}

func TestBrowserHandoffSessionReferencePostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx := context.Background()
	rawDigest, err := security.DigestOpaqueToken(security.SessionToken, f.cookie)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := security.PepperTokenDigest(f.s.config.TokenPepper, rawDigest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	session, err := f.s.store.GetActiveSession(ctx, digest[:], now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.store.GetActiveSessionByID(ctx, f.owner.ID, session.ID, now)
	if err != nil || got.ID != session.ID {
		t.Fatalf("active session reference: %v", err)
	}
	for _, test := range []struct {
		userID, sessionID string
		at                time.Time
	}{
		{uuid.NewString(), session.ID, now},
		{f.owner.ID, uuid.NewString(), now},
		{f.owner.ID, session.ID, session.IdleExpiresAt},
		{f.owner.ID, session.ID, session.AbsoluteExpiresAt},
	} {
		if _, err := f.s.store.GetActiveSessionByID(ctx, test.userID, test.sessionID, test.at); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("invalid session reference accepted: %v", err)
		}
	}
	if err := f.s.store.RevokeSession(ctx, session.ID, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.GetActiveSessionByID(ctx, f.owner.ID, session.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked session reference accepted: %v", err)
	}
}

func TestBrowserHandoffOwnershipAndRecentAuthPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	f.s.config.KeyPepper = []byte(strings.Repeat("k", 32))
	f.s.config.APIKeyEncryptionKey = []byte(strings.Repeat("e", 32))
	f.s.config.BrowserClientURL, _ = url.Parse("https://ai.example.test")
	ctx := context.Background()
	other, err := f.s.store.CreateUser(ctx, store.CreateUserParams{Username: "other-user", DisplayName: "Other", Role: store.UserRoleMember})
	if err != nil {
		t.Fatal(err)
	}
	device, err := f.s.store.CreateDevice(ctx, store.CreateDeviceParams{UserID: other.ID, Name: "Other device"})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := security.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := security.HashAPIKey(f.s.config.KeyPepper, generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := security.EncryptAPIKeySecret(f.s.config.APIKeyEncryptionKey, other.ID, generated.PublicID, generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.s.store.CreateAPIKey(ctx, store.CreateAPIKeyParams{UserID: other.ID, DeviceID: device.ID, PublicID: generated.PublicID, KeyPrefix: generated.Prefix, KeyHash: digest[:], SecretCiphertext: encrypted, Name: "Other key", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	f.send(t, "POST", "/admin/browser-handoffs", map[string]any{"api_key_id": key.ID}, f.cookie, 403)
	token, err := security.GenerateOpaqueToken(security.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	sessionDigest, err := security.PepperTokenDigest(f.s.config.TokenPepper, token.Digest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, err = f.s.store.CreateSession(ctx, store.CreateSessionParams{UserID: other.ID, TokenHash: sessionDigest[:], CSRFSecret: []byte(strings.Repeat("c", 32)), CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	response := f.send(t, "POST", "/admin/browser-handoffs", map[string]any{"api_key_id": key.ID}, token.Token, 403)
	if !strings.Contains(response.Body.String(), "recent_identity_verification_required") {
		t.Fatal("new session did not require reauthentication")
	}
	// An attacker cannot select a launch URL by sending extra fields.
	raw, _ := json.Marshal(map[string]any{"api_key_id": key.ID, "launch_url": "https://evil.example"})
	request := httptest.NewRequest("POST", "/admin/browser-handoffs", strings.NewReader(string(raw)))
	request.Header.Set("Origin", "https://gateway.example")
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.cookie})
	recorder := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(recorder, request)
	if recorder.Code != 400 {
		t.Fatalf("caller controlled target accepted: %d", recorder.Code)
	}
}
