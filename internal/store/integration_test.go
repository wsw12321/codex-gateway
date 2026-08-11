//go:build integration

package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgresIntegration is opt-in so ordinary unit tests stay mock-free and
// do not require a local service. CI or operators can set TEST_DATABASE_URL to
// a disposable PostgreSQL 15+ database/schema.
func TestPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("idempotent Migrate: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	user, err := s.CreateUser(ctx, CreateUserParams{
		Username: "integration-owner", DisplayName: "Integration Owner", Role: UserRoleOwner,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if len(user.WebAuthnUserID) != 32 {
		t.Fatalf("WebAuthn user handle length = %d", len(user.WebAuthnUserID))
	}
	if _, err := s.GetUserByWebAuthnID(ctx, user.WebAuthnUserID); err != nil {
		t.Fatalf("GetUserByWebAuthnID: %v", err)
	}

	device, err := s.CreateDevice(ctx, CreateDeviceParams{UserID: user.ID, Name: "integration-device"})
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	project, err := s.CreateProject(ctx, CreateProjectParams{
		UserID: user.ID, Slug: "integration", Name: "Integration",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	emptyKeys, err := s.ListAPIKeys(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys before creation: %v", err)
	}
	encodedKeys, err := json.Marshal(emptyKeys)
	if err != nil {
		t.Fatalf("marshal empty API key list: %v", err)
	}
	if string(encodedKeys) != "[]" {
		t.Fatalf("empty API key list JSON = %s, want []", encodedKeys)
	}
	keyHash := make([]byte, 32)
	keyHash[0] = 1
	key, err := s.CreateAPIKey(ctx, CreateAPIKeyParams{
		PublicID: "integration01", KeyPrefix: "cgk_v1_integ", KeyHash: keyHash,
		UserID: user.ID, DeviceID: device.ID, DefaultProjectID: project.ID,
		Name: "integration-key", ModelAllowlist: []string{"gpt-5-codex"},
		CreatedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if _, err := s.LookupAPIKey(ctx, key.PublicID); err != nil {
		t.Fatalf("LookupAPIKey: %v", err)
	}

	requestID := "0123456789abcdef0123456789abcdef"
	_, err = s.ReserveQuota(ctx, ReserveQuotaParams{
		RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Day: now, Now: now,
		ReservedTokens: 100, LeaseTTL: time.Minute,
		Limits: QuotaLimits{
			KeyRequestsPerMinute: 30, UserRequestsPerMinute: 60,
			KeyConcurrent: 4, UserConcurrent: 8, GlobalConcurrent: 12,
			KeyDailyRequests: 1000, UserDailyRequests: 2000,
			KeyDailyTokens: 20_000_000, UserDailyTokens: 40_000_000,
		},
	})
	if err != nil {
		t.Fatalf("ReserveQuota: %v", err)
	}
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{
		RequestID: requestID, UserID: user.ID, DeviceID: device.ID, APIKeyID: key.ID,
		ProjectID: project.ID, Model: "gpt-5-codex", Endpoint: "responses",
		RequestedAt: now, RequestBytes: 128,
	}); err != nil {
		t.Fatalf("BeginUsageRequest: %v", err)
	}
	firstToken := now.Add(100 * time.Millisecond)
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: requestID, State: "completed", HTTPStatus: 200,
		FirstTokenAt: &firstToken, CompletedAt: now.Add(time.Second),
		InputTokens: 60, CachedInputTokens: 20, OutputTokens: 20,
		ReasoningTokens: 10, RequestBytes: 128, ResponseBytes: 256,
	}); err != nil {
		t.Fatalf("CompleteUsageRequest: %v", err)
	}
	if err := s.SettleQuota(ctx, requestID, 80, now.Add(time.Second)); err != nil {
		t.Fatalf("SettleQuota: %v", err)
	}
	if err := s.AggregateUsageDay(ctx, now, "UTC"); err != nil {
		t.Fatalf("AggregateUsageDay: %v", err)
	}
	daily, err := s.ListDailyUsage(ctx, now, now.AddDate(0, 0, 1), user.ID)
	if err != nil {
		t.Fatalf("ListDailyUsage: %v", err)
	}
	if len(daily) != 1 || daily[0].RequestCount != 1 || daily[0].InputTokens != 60 {
		t.Fatalf("unexpected daily usage: %#v", daily)
	}
	if err := s.AggregateUsageMonth(ctx, now, "UTC"); err != nil {
		t.Fatalf("AggregateUsageMonth: %v", err)
	}
	monthly, err := s.ListMonthlyUsage(ctx, now, now.AddDate(0, 1, 0), user.ID)
	if err != nil {
		t.Fatalf("ListMonthlyUsage: %v", err)
	}
	if len(monthly) != 1 || monthly[0].RequestCount != 1 || monthly[0].OutputTokens != 20 {
		t.Fatalf("unexpected monthly usage: %#v", monthly)
	}
	summary, err := s.SummarizeUsageRequests(ctx, UsageFilter{UserID: user.ID})
	if err != nil {
		t.Fatalf("SummarizeUsageRequests: %v", err)
	}
	if summary.RequestCount != 1 || summary.InputTokens != 60 || summary.P95TTFTMillis != 100 {
		t.Fatalf("unexpected usage summary: %#v", summary)
	}

	if _, err := s.AppendAuditEvent(ctx, AppendAuditEventParams{
		OccurredAt: now, ActorUserID: user.ID, EventType: "integration.completed",
		Severity: "info", Success: true, Metadata: map[string]any{"status": "ok"},
	}); err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}
	if _, err := s.CreateAlert(ctx, CreateAlertParams{
		OccurredAt: now, Type: "quota.threshold", Severity: "warning",
		UserID: user.ID, DedupeKey: "integration-quota", Title: "Quota threshold",
		Details: map[string]any{"percent": 80},
	}); err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
}
