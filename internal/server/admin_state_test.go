package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestAdminStateDTOFieldWhitelist(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	projectID := "project-id"
	lastUsed := now.Add(-time.Hour)
	response := newAdminStateResponse(
		store.User{
			ID: "user-id", Username: "member", DisplayName: "Member", Role: store.UserRoleMember,
			WebAuthnUserID: []byte("sensitive-user-handle"), Status: store.StatusActive,
		},
		[]store.Device{{
			ID: "device-id", UserID: "sensitive-user-id", Name: "Laptop", Status: store.StatusActive,
			CreatedAt: now,
		}},
		[]store.Project{{
			ID: projectID, UserID: "sensitive-user-id", Slug: "demo", Name: "Demo",
			Status: store.StatusActive, CreatedAt: now,
		}},
		[]store.APIKey{{
			ID: "key-id", PublicID: "sensitive-public-id", KeyPrefix: "cgk_v1_demo",
			KeyHash: []byte("sensitive-key-hash"), UserID: "sensitive-user-id", DeviceID: "device-id",
			DefaultProjectID: &projectID, Name: "CLI", Status: store.StatusActive,
			ModelAllowlist: nil, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), LastUsedAt: &lastUsed,
		}},
		[]store.WebAuthnCredential{{
			ID: "passkey-id", UserID: "sensitive-user-id", CredentialID: []byte("sensitive-credential-id"),
			CredentialJSON: []byte("sensitive-credential-json"), SignCount: 42, Transports: []string{"internal"},
			Nickname: "Phone", BackupEligible: true, BackupState: true, CreatedAt: now, LastUsedAt: &lastUsed,
		}},
		true,
		&now,
	)

	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"KeyHash", "key_hash", "CredentialJSON", "credential_json", "CredentialID", "credential_id",
		"sensitive-key-hash", "sensitive-credential-json", "sensitive-credential-id", "sensitive-user-id",
		"sign_count", "public_id", "user_id", "rotated_from_id", "revoke_reason", "aaguid", "transports",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("admin state leaked forbidden field or value %q: %s", forbidden, raw)
		}
	}

	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, document,
		"api_keys", "devices", "passkeys", "projects", "recent_verification_expires_at", "recently_verified", "user",
	)
	assertJSONKeys(t, document["user"].(map[string]any), "display_name", "id", "role", "username")
	assertJSONKeys(t, document["devices"].([]any)[0].(map[string]any), "created_at", "id", "last_seen_at", "name", "status")
	assertJSONKeys(t, document["projects"].([]any)[0].(map[string]any), "created_at", "id", "name", "slug", "status")
	assertJSONKeys(t, document["api_keys"].([]any)[0].(map[string]any),
		"created_at", "default_project_id", "device_id", "expires_at", "id", "key_prefix", "last_used_at", "model_allowlist", "name", "status",
	)
	assertJSONKeys(t, document["passkeys"].([]any)[0].(map[string]any),
		"backup_eligible", "backup_state", "created_at", "id", "last_used_at", "nickname",
	)
	if models := document["api_keys"].([]any)[0].(map[string]any)["model_allowlist"]; models == nil || len(models.([]any)) != 0 {
		t.Fatalf("empty model_allowlist = %#v, want []", models)
	}
}

func TestAdminStateDTOUsesArraysAndNullAssociations(t *testing.T) {
	response := newAdminStateResponse(store.User{}, nil, nil, nil, nil, false, nil)
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); !strings.Contains(got, `"devices":[]`) ||
		!strings.Contains(got, `"projects":[]`) || !strings.Contains(got, `"api_keys":[]`) ||
		!strings.Contains(got, `"passkeys":[]`) || !strings.Contains(got, `"recent_verification_expires_at":null`) {
		t.Fatalf("empty admin state does not preserve arrays/null: %s", got)
	}

	response = newAdminStateResponse(
		store.User{}, nil, nil,
		[]store.APIKey{{ModelAllowlist: []string{}}}, nil, false, nil,
	)
	raw, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); !strings.Contains(got, `"default_project_id":null`) ||
		!strings.Contains(got, `"last_used_at":null`) {
		t.Fatalf("nullable API key associations were not encoded as null: %s", got)
	}
}

func TestRecoveryInvitationLinkCarriesPresentationHint(t *testing.T) {
	if got := invitationLink("https://gateway.example/", "secret-token", store.InvitationRecovery); got !=
		"https://gateway.example/join#token=secret-token&kind=recovery" {
		t.Fatalf("recovery invitation link = %q", got)
	}
	if got := invitationLink("https://gateway.example", "secret-token", store.InvitationMember); got !=
		"https://gateway.example/join#token=secret-token" {
		t.Fatalf("member invitation link = %q", got)
	}
}

func assertJSONKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	if len(object) != len(want) {
		t.Fatalf("JSON keys = %#v, want exactly %#v", object, want)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			t.Fatalf("JSON object missing key %q: %#v", key, object)
		}
	}
}
