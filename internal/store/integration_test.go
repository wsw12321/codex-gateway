//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
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
		SecretCiphertext: []byte{1},
		UserID:           user.ID, DeviceID: device.ID, DefaultProjectID: project.ID,
		Name: "integration-key", ModelAllowlist: []string{"gpt-5-codex"},
		CreatedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if _, err := s.LookupAPIKey(ctx, key.PublicID); err != nil {
		t.Fatalf("LookupAPIKey: %v", err)
	}
	admissionUser, admissionDevice, admissionKey := billingIntegrationPrincipal(
		t, ctx, s, "flow-"+user.ID[:8],
	)

	// Billing failure happens after the quota locks in AdmitRequest, but the
	// shared transaction must roll every admission artifact back.
	failedAdmissionID := "billingatomicfailure0123456789abcdef"
	_, err = s.AdmitRequest(ctx, AdmitRequestParams{
		Quota: ReserveQuotaParams{
			RequestID: failedAdmissionID, UserID: admissionUser.ID, APIKeyID: admissionKey.ID,
			Limits: QuotaLimits{
				KeyRequestsPerMinute: 30, UserRequestsPerMinute: 60,
				KeyConcurrent: 4, UserConcurrent: 8, GlobalConcurrent: 12,
				KeyDailyRequests: 1000, UserDailyRequests: 2000,
			},
		},
		Usage: BeginUsageRequestParams{
			RequestID: failedAdmissionID, UserID: admissionUser.ID, DeviceID: admissionDevice.ID,
			APIKeyID: admissionKey.ID, Model: "gpt-5-codex",
			Endpoint: "responses", RequestBytes: 16,
		},
		Billing: &BillingReservationParams{
			RequestID: failedAdmissionID, UserID: admissionUser.ID, APIKeyID: admissionKey.ID,
			Model: "gpt-5-codex", InputUSDPerMillion: "1",
			CachedInputUSDPerMillion: "0.1", OutputUSDPerMillion: "10",
		},
	})
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("AdmitRequest without funds error = %v, want InsufficientFundsError", err)
	}
	var failedArtifacts int
	if err := s.DB().QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM quota_reservations WHERE request_id = $1)
		     + (SELECT count(*) FROM usage_requests WHERE request_id = $1)
		     + (SELECT count(*) FROM billing_reservations WHERE request_id = $1)`,
		failedAdmissionID,
	).Scan(&failedArtifacts); err != nil {
		t.Fatalf("count failed admission artifacts: %v", err)
	}
	if failedArtifacts != 0 {
		t.Fatalf("failed admission left %d durable artifacts", failedArtifacts)
	}

	// The model catalog endpoint still participates in traffic quotas but does
	// not require or create monetary billing state, even at a zero balance.
	modelsRequestID := "modelswithoutbalance0123456789abcdef"
	modelsAt := now.Add(10 * time.Second)
	if _, err := s.AdmitRequest(ctx, AdmitRequestParams{
		Quota: ReserveQuotaParams{
			RequestID: modelsRequestID, UserID: admissionUser.ID, APIKeyID: admissionKey.ID,
			Now: modelsAt, Day: modelsAt, LeaseTTL: time.Minute,
			Limits: QuotaLimits{
				KeyRequestsPerMinute: 30, UserRequestsPerMinute: 60,
				KeyConcurrent: 4, UserConcurrent: 8, GlobalConcurrent: 12,
				KeyDailyRequests: 1000, UserDailyRequests: 2000,
			},
		},
		Usage: BeginUsageRequestParams{
			RequestID: modelsRequestID, UserID: admissionUser.ID, DeviceID: admissionDevice.ID,
			APIKeyID: admissionKey.ID, Model: "catalog",
			Endpoint: "models", RequestedAt: modelsAt,
		},
	}); err != nil {
		t.Fatalf("AdmitRequest for models at zero balance: %v", err)
	}
	modelsCompletedAt := modelsAt.Add(time.Second)
	completion := CompleteUsageRequestParams{
		RequestID: modelsRequestID, State: "completed", HTTPStatus: 200,
		CompletedAt: modelsCompletedAt,
	}
	if _, err := s.CompleteUsageRequest(ctx, completion); err != nil {
		t.Fatalf("CompleteUsageRequest for models: %v", err)
	}
	if _, err := s.CompleteUsageRequest(ctx, completion); err != nil {
		t.Fatalf("idempotent CompleteUsageRequest replay: %v", err)
	}
	differentFirstToken := modelsAt.Add(500 * time.Millisecond)
	conflictingCompletion := completion
	conflictingCompletion.FirstTokenAt = &differentFirstToken
	if _, err := s.CompleteUsageRequest(ctx, conflictingCompletion); !errors.Is(err, ErrConflict) {
		t.Fatalf("completion replay with a different first-token time error = %v, want ErrConflict", err)
	}
	if settled, err := s.RetryUnsettledRequests(ctx, 100); err != nil {
		t.Fatalf("RetryUnsettledRequests: %v", err)
	} else if settled != 1 {
		t.Fatalf("settled terminal requests = %d, want 1", settled)
	}
	var quotaState string
	var billingArtifacts int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT state FROM quota_reservations WHERE request_id = $1`, modelsRequestID,
	).Scan(&quotaState); err != nil {
		t.Fatalf("read models quota state: %v", err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM billing_reservations WHERE request_id = $1`, modelsRequestID,
	).Scan(&billingArtifacts); err != nil {
		t.Fatalf("count models billing artifacts: %v", err)
	}
	if quotaState != "settled" || billingArtifacts != 0 {
		t.Fatalf("models settlement = quota %q, billing rows %d", quotaState, billingArtifacts)
	}

	// Stale cleanup may release only an in-progress request. It atomically
	// terminalizes the metadata, so a late completion cannot overwrite it.
	staleRequestID := "stalemodelsrequest0123456789abcdef"
	staleAt := now.Add(-2 * time.Hour)
	if _, err := s.AdmitRequest(ctx, AdmitRequestParams{
		Quota: ReserveQuotaParams{
			RequestID: staleRequestID, UserID: admissionUser.ID, APIKeyID: admissionKey.ID,
			Now: staleAt, Day: staleAt, LeaseTTL: time.Minute,
			Limits: QuotaLimits{
				KeyRequestsPerMinute: 30, UserRequestsPerMinute: 60,
				KeyConcurrent: 4, UserConcurrent: 8, GlobalConcurrent: 12,
				KeyDailyRequests: 1000, UserDailyRequests: 2000,
			},
		},
		Usage: BeginUsageRequestParams{
			RequestID: staleRequestID, UserID: admissionUser.ID, DeviceID: admissionDevice.ID,
			APIKeyID: admissionKey.ID, Model: "catalog",
			Endpoint: "models", RequestedAt: staleAt,
		},
	}); err != nil {
		t.Fatalf("AdmitRequest for stale cleanup: %v", err)
	}
	if released, err := s.ReleaseStaleQuotaReservations(ctx, now.Add(-time.Hour), 100); err != nil {
		t.Fatalf("ReleaseStaleQuotaReservations: %v", err)
	} else if released != 1 {
		t.Fatalf("released stale requests = %d, want 1", released)
	}
	staleUsage, err := s.GetUsageRequest(ctx, staleRequestID)
	if err != nil {
		t.Fatalf("GetUsageRequest after stale cleanup: %v", err)
	}
	if staleUsage.State != "cancelled" || staleUsage.ErrorCode == nil || *staleUsage.ErrorCode != "reservation_released" {
		t.Fatalf("stale usage was not terminalized: %#v", staleUsage)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: staleRequestID, State: "completed", HTTPStatus: 200,
		CompletedAt: now,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("late stale completion error = %v, want ErrConflict", err)
	}

	requestID := "0123456789abcdef0123456789abcdef"
	_, err = s.ReserveQuota(ctx, ReserveQuotaParams{
		RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Day: now, Now: now,
		ReservedTokens: 100, LeaseTTL: time.Minute,
		Limits: QuotaLimits{
			KeyRequestsPerMinute: 30, UserRequestsPerMinute: 60,
			KeyConcurrent: 4, UserConcurrent: 8, GlobalConcurrent: 12,
			KeyDailyRequests: 1000, UserDailyRequests: 2000,
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
	completed, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: requestID, State: "completed", HTTPStatus: 200,
		FirstTokenAt: &firstToken, CompletedAt: now.Add(time.Second),
		InputTokens: 60, CachedInputTokens: 20, OutputTokens: 20,
		ReasoningTokens: 10, RequestBytes: 128, ResponseBytes: 256,
		ActualModel: "gpt-5-codex-upstream",
	})
	if err != nil {
		t.Fatalf("CompleteUsageRequest: %v", err)
	}
	if completed.Model != "gpt-5-codex-upstream" {
		t.Fatalf("completed model = %q, want explicit upstream model", completed.Model)
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
	if len(daily) != 1 || daily[0].RequestCount != 1 || daily[0].InputTokens != 60 ||
		daily[0].Model != "gpt-5-codex-upstream" {
		t.Fatalf("unexpected daily usage: %#v", daily)
	}
	if err := s.AggregateUsageMonth(ctx, now, "UTC"); err != nil {
		t.Fatalf("AggregateUsageMonth: %v", err)
	}
	monthly, err := s.ListMonthlyUsage(ctx, now, now.AddDate(0, 1, 0), user.ID)
	if err != nil {
		t.Fatalf("ListMonthlyUsage: %v", err)
	}
	if len(monthly) != 1 || monthly[0].RequestCount != 1 || monthly[0].OutputTokens != 20 ||
		monthly[0].Model != "gpt-5-codex-upstream" {
		t.Fatalf("unexpected monthly usage: %#v", monthly)
	}
	summary, err := s.SummarizeUsageRequests(ctx, UsageFilter{UserID: user.ID})
	if err != nil {
		t.Fatalf("SummarizeUsageRequests: %v", err)
	}
	if summary.RequestCount != 1 || summary.InputTokens != 60 || summary.P95TTFTMillis != 100 {
		t.Fatalf("unexpected usage summary: %#v", summary)
	}

	fallbackRequestID := "abcdef0123456789abcdef0123456789"
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{
		RequestID: fallbackRequestID, UserID: user.ID, DeviceID: device.ID, APIKeyID: key.ID,
		ProjectID: project.ID, Model: "gpt-5-codex-requested", Endpoint: "responses",
		RequestedAt: now.Add(2 * time.Second), RequestBytes: 2,
	}); err != nil {
		t.Fatalf("BeginUsageRequest for model fallback: %v", err)
	}
	fallback, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: fallbackRequestID, State: "completed", HTTPStatus: 200,
		CompletedAt: now.Add(3 * time.Second), RequestBytes: 2, ResponseBytes: 2,
		ActualModel: "   ",
	})
	if err != nil {
		t.Fatalf("CompleteUsageRequest for model fallback: %v", err)
	}
	if fallback.Model != "gpt-5-codex-requested" {
		t.Fatalf("fallback model = %q, want requested model", fallback.Model)
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
