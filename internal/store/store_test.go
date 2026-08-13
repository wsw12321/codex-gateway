package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type testSQLStateError string

func (e testSQLStateError) Error() string    { return string(e) }
func (e testSQLStateError) SQLState() string { return string(e) }

func TestEmbeddedMigrationsCoverRequiredSchema(t *testing.T) {
	t.Parallel()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatalf("EmbeddedMigrations: %v", err)
	}
	if len(migrations) != 3 {
		t.Fatalf("migration count = %d, want 3", len(migrations))
	}
	var sql string
	for _, migration := range migrations {
		sql += migration.SQL
	}
	for _, table := range []string{
		"users", "invitations", "webauthn_credentials", "recovery_codes",
		"sessions", "devices", "projects", "api_keys", "usage_requests",
		"usage_daily", "audit_events", "alerts", "quota_locks",
		"usage_monthly",
		"quota_counters", "quota_rate_windows", "quota_reservations",
		"concurrency_leases",
		"billing_settings", "billing_accounts", "billing_operations",
		"billing_subscriptions", "billing_subscription_periods",
		"billing_ledger_entries", "billing_cash_credit_lots",
		"billing_reservations", "billing_charge_allocations",
		"password_credentials",
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
	for _, key := range []string{"authorization", "refresh_token", "request_body", "source_code", "password", "encoded_hash", "phc"} {
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
	if err := enforceQuota("key", "daily_requests", 100, 80, 20, time.Hour); err != nil {
		t.Fatalf("quota at exact boundary failed: %v", err)
	}
	err := enforceQuota("key", "daily_requests", 100, 80, 21, time.Hour)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota error = %v, want ErrQuotaExceeded", err)
	}
	var detail *QuotaExceededError
	if !errors.As(err, &detail) || detail.RetryAfter != time.Hour {
		t.Fatalf("quota detail = %#v", detail)
	}
	if err := enforceQuota("key", "daily_requests", 0, 1_000_000, 1, time.Hour); err != nil {
		t.Fatalf("zero (unlimited) quota failed: %v", err)
	}
}

func TestComparePasswordHashCAS(t *testing.T) {
	t.Parallel()
	if err := ComparePasswordHash("same", "same"); err != nil {
		t.Fatalf("matching hash rejected: %v", err)
	}
	if err := ComparePasswordHash("changed", "old"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed hash error = %v, want ErrConflict", err)
	}
}

func TestMapDBErrorClassifiesNumericOverflowAsInvalid(t *testing.T) {
	t.Parallel()
	if err := mapDBError("add billing balance", testSQLStateError("22003")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("numeric overflow error = %v, want ErrInvalid", err)
	}
}

func TestAdmitRequestRejectsBillingBypassBeforeTransaction(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	base := AdmitRequestParams{
		Quota: ReserveQuotaParams{
			RequestID: "0123456789abcdef", UserID: "user", APIKeyID: "key",
			Now: now, Day: now,
		},
		Usage: BeginUsageRequestParams{
			RequestID: "0123456789abcdef", UserID: "user", DeviceID: "device",
			APIKeyID: "key", Model: "gpt-test", Endpoint: "models", RequestedAt: now,
		},
	}
	billing := BillingReservationParams{
		RequestID: base.Quota.RequestID, UserID: base.Quota.UserID,
		APIKeyID: base.Quota.APIKeyID, Model: base.Usage.Model,
		InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0.1", OutputUSDPerMillion: "10",
	}

	storeWithoutDB := New(nil)
	for _, test := range []struct {
		name   string
		mutate func(*AdmitRequestParams)
	}{
		{
			name: "models cannot create billing reservation",
			mutate: func(params *AdmitRequestParams) {
				params.Billing = &billing
			},
		},
		{
			name: "responses require billing reservation",
			mutate: func(params *AdmitRequestParams) {
				params.Usage.Endpoint = "responses"
			},
		},
		{
			name: "unsupported endpoint",
			mutate: func(params *AdmitRequestParams) {
				params.Usage.Endpoint = "chat.completions"
			},
		},
		{
			name: "admission timestamps must match",
			mutate: func(params *AdmitRequestParams) {
				params.Usage.RequestedAt = now.Add(time.Nanosecond)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := base
			test.mutate(&params)
			if _, err := storeWithoutDB.AdmitRequest(context.Background(), params); !errors.Is(err, ErrInvalid) {
				t.Fatalf("AdmitRequest error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestBillingPeriodDurationsAreFixedRollingWindows(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		tier string
		want time.Duration
	}{
		{tier: BillingTierDay, want: 24 * time.Hour},
		{tier: BillingTierWeek, want: 7 * 24 * time.Hour},
		{tier: BillingTierMonth, want: 31 * 24 * time.Hour},
	} {
		got, err := billingPeriodDuration(test.tier)
		if err != nil {
			t.Fatalf("duration for %s: %v", test.tier, err)
		}
		if got != test.want {
			t.Fatalf("duration for %s = %s, want %s", test.tier, got, test.want)
		}
	}
	if _, err := billingPeriodDuration("calendar-month"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid tier error = %v, want ErrInvalid", err)
	}
}

func TestBillingAllocationArithmeticKeepsTwelveDecimalPlaces(t *testing.T) {
	t.Parallel()

	minimum, err := billingMin("1.234567890123", "2.000000000000")
	if err != nil || minimum != "1.234567890123" {
		t.Fatalf("billingMin = %q, %v", minimum, err)
	}
	remaining, err := billingSubtract("2.000000000000", minimum)
	if err != nil || remaining != "0.765432109877" {
		t.Fatalf("billingSubtract = %q, %v", remaining, err)
	}
	recombined, err := billingAdd(minimum, remaining)
	if err != nil || recombined != "2.000000000000" {
		t.Fatalf("billingAdd = %q, %v", recombined, err)
	}
}

func TestBillingMigrationEnforcesAuditAndAdmissionBindings(t *testing.T) {
	t.Parallel()

	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var billingSQL string
	for _, migration := range migrations {
		if migration.Name == "0002_billing.sql" {
			billingSQL = migration.SQL
		}
	}
	if billingSQL == "" {
		t.Fatal("0002_billing.sql is missing")
	}
	for _, required := range []string{
		"NUMERIC(30,12)",
		"ends_at = starts_at + INTERVAL '24 hours'",
		"ends_at = starts_at + INTERVAL '7 days'",
		"ends_at = starts_at + INTERVAL '31 days'",
		"cash_lot_cutoff BIGINT",
		"request_fingerprint BYTEA NOT NULL",
		"billing_operations_target_consistent",
		"billing_cash_lots_source_user_fk",
		"billing_allocations_period_tier_fk",
		"allocation_order INTEGER",
		"billing_ledger_entries_immutable",
		"AFTER INSERT ON users",
	} {
		if !strings.Contains(billingSQL, required) {
			t.Errorf("billing migration is missing %q", required)
		}
	}
	for _, forbidden := range []string{"request_payload", "response_payload", "raw_payload"} {
		if strings.Contains(strings.ToLower(billingSQL), forbidden) {
			t.Errorf("billing migration stores forbidden payload field %q", forbidden)
		}
	}
}
