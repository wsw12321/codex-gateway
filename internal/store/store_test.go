package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedMigrationsCoverRequiredSchema(t *testing.T) {
	t.Parallel()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatalf("EmbeddedMigrations: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("migration count = %d, want 1", len(migrations))
	}
	sql := migrations[0].SQL
	for _, table := range []string{
		"users", "invitations", "webauthn_credentials", "recovery_codes",
		"sessions", "devices", "projects", "api_keys", "usage_requests",
		"usage_daily", "audit_events", "alerts", "quota_locks",
		"usage_monthly",
		"quota_counters", "quota_rate_windows", "quota_reservations",
		"concurrency_leases",
	} {
		needle := "CREATE TABLE " + table
		if !strings.Contains(sql, needle) {
			t.Errorf("migration missing %q", needle)
		}
	}
	for _, forbidden := range []string{"prompt_text", "request_body BYTEA", "response_body BYTEA", "oauth_token"} {
		if strings.Contains(strings.ToLower(sql), strings.ToLower(forbidden)) {
			t.Errorf("migration contains sensitive payload column %q", forbidden)
		}
	}
}

func TestNormalizeCreateUserWebAuthnHandle(t *testing.T) {
	t.Parallel()
	generated, err := normalizeCreateUser(CreateUserParams{Username: "Alice", Role: UserRoleMember})
	if err != nil {
		t.Fatalf("normalize generated handle: %v", err)
	}
	if got := len(generated.WebAuthnUserID); got != 32 {
		t.Fatalf("generated WebAuthn handle length = %d, want 32", got)
	}
	if generated.Username != "alice" {
		t.Fatalf("normalized username = %q, want alice", generated.Username)
	}

	fixed := make([]byte, 32)
	for i := range fixed {
		fixed[i] = byte(i)
	}
	preserved, err := normalizeCreateUser(CreateUserParams{
		Username: "bob", WebAuthnUserID: fixed,
	})
	if err != nil {
		t.Fatalf("normalize fixed handle: %v", err)
	}
	if string(preserved.WebAuthnUserID) != string(fixed) {
		t.Fatal("provided WebAuthn handle was not preserved")
	}

	_, err = normalizeCreateUser(CreateUserParams{
		Username: "carol", WebAuthnUserID: make([]byte, 31),
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("31-byte WebAuthn handle error = %v, want ErrInvalid", err)
	}
}

func TestNewUUIDVersionAndVariant(t *testing.T) {
	t.Parallel()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("malformed UUID %q", id)
	}
	if id[14] != '4' {
		t.Fatalf("UUID version nibble = %q, want 4", id[14])
	}
	if !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("UUID variant nibble = %q, want 8, 9, a or b", id[19])
	}
}

func TestSafeMetadataRejectsSecretBearingKeys(t *testing.T) {
	t.Parallel()
	if _, err := marshalSafeMetadata(map[string]any{
		"quota": map[string]any{"used": int64(80), "limit": int64(100)},
	}); err != nil {
		t.Fatalf("safe metadata rejected: %v", err)
	}
	for _, key := range []string{"authorization", "refresh_token", "request_body", "source_code"} {
		_, err := marshalSafeMetadata(map[string]any{key: "must-not-be-stored"})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("metadata key %q error = %v, want ErrInvalid", key, err)
		}
	}
}

func TestTotalTokensDoesNotDoubleCountCachedOrReasoning(t *testing.T) {
	t.Parallel()
	request := UsageRequest{
		InputTokens: 100, CachedInputTokens: 80, OutputTokens: 40, ReasoningTokens: 30,
	}
	if got := request.TotalTokens(); got != 140 {
		t.Fatalf("TotalTokens = %d, want 140", got)
	}
}

func TestQuotaExceededError(t *testing.T) {
	t.Parallel()
	if err := enforceQuota("key", "daily_tokens", 100, 80, 20, time.Hour); err != nil {
		t.Fatalf("quota at exact boundary failed: %v", err)
	}
	err := enforceQuota("key", "daily_tokens", 100, 80, 21, time.Hour)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota error = %v, want ErrQuotaExceeded", err)
	}
	var detail *QuotaExceededError
	if !errors.As(err, &detail) || detail.RetryAfter != time.Hour {
		t.Fatalf("quota detail = %#v", detail)
	}
	if err := enforceQuota("key", "daily_tokens", 0, 1_000_000, 1, time.Hour); err != nil {
		t.Fatalf("zero (unlimited) quota failed: %v", err)
	}
}
