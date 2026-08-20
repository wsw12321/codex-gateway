//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestAPIKeyLifecyclePostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user, err := repository.CreateUser(ctx, CreateUserParams{
		Username: "key-life-" + suffix, DisplayName: "API Key Lifecycle", Role: UserRoleMember,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	device, err := repository.CreateDevice(ctx, CreateDeviceParams{
		UserID: user.ID, Name: "key-life-" + suffix,
	})
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	project, err := repository.CreateProject(ctx, CreateProjectParams{
		UserID: user.ID, Slug: "key-life-" + suffix, Name: "API Key Lifecycle",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	failedID, _ := newUUID()
	missingProjectID, _ := newUUID()
	failedHash := sha256.Sum256([]byte("failed-" + suffix))
	if _, err := repository.CreateAPIKey(ctx, CreateAPIKeyParams{
		ID: failedID, PublicID: "failedkey" + suffix, KeyPrefix: "cgk_fail_" + suffix,
		KeyHash: failedHash[:], SecretCiphertext: []byte{1}, UserID: user.ID,
		DeviceID: device.ID, DefaultProjectID: missingProjectID, Name: "must roll back",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("CreateAPIKey with missing project error = %v, want ErrInvalid", err)
	}
	var failedHistory int
	if err := repository.db.QueryRowContext(ctx,
		`SELECT count(*) FROM api_key_history WHERE id=$1`, failedID,
	).Scan(&failedHistory); err != nil {
		t.Fatalf("count rolled-back API key history: %v", err)
	}
	if failedHistory != 0 {
		t.Fatalf("failed API key creation left %d history rows", failedHistory)
	}

	digest := sha256.Sum256([]byte("active-" + suffix))
	ciphertext := []byte{1, 2, 3, 4}
	key, err := repository.CreateAPIKey(ctx, CreateAPIKeyParams{
		PublicID: "lifecycle" + suffix, KeyPrefix: "cgk_lc_" + suffix,
		KeyHash: digest[:], SecretCiphertext: ciphertext,
		UserID: user.ID, DeviceID: device.ID, DefaultProjectID: project.ID,
		Name: "lifecycle key", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if !key.SecretAvailable {
		t.Fatal("new API key did not report secret availability")
	}
	secret, err := repository.GetAPIKeySecret(ctx, user.ID, key.ID)
	if err != nil {
		t.Fatalf("GetAPIKeySecret: %v", err)
	}
	if secret.ID != key.ID || secret.PublicID != key.PublicID ||
		string(secret.KeyHash) != string(digest[:]) ||
		string(secret.SecretCiphertext) != string(ciphertext) {
		t.Fatalf("unexpected API key secret material: id=%s public=%s hash=%d cipher=%d",
			secret.ID, secret.PublicID, len(secret.KeyHash), len(secret.SecretCiphertext))
	}

	disabled, changed, err := repository.SetAPIKeyStatus(
		ctx, user.ID, key.ID, StatusDisabled, now.Add(time.Second),
	)
	if err != nil || !changed || disabled.Status != StatusDisabled {
		t.Fatalf("disable API key = key %+v changed %v err %v", disabled, changed, err)
	}
	_, changed, err = repository.SetAPIKeyStatus(
		ctx, user.ID, key.ID, StatusDisabled, now.Add(2*time.Second),
	)
	if err != nil || changed {
		t.Fatalf("idempotent disable changed=%v err=%v", changed, err)
	}
	if _, _, err := repository.SetAPIKeyStatus(
		ctx, user.ID, key.ID, StatusActive, key.ExpiresAt,
	); !errors.Is(err, ErrAPIKeyExpired) {
		t.Fatalf("enable expired API key error = %v, want ErrAPIKeyExpired", err)
	}
	key, changed, err = repository.SetAPIKeyStatus(
		ctx, user.ID, key.ID, StatusActive, now.Add(3*time.Second),
	)
	if err != nil || !changed || key.Status != StatusActive {
		t.Fatalf("enable API key = key %+v changed %v err %v", key, changed, err)
	}

	legacyID, _ := newUUID()
	legacyHash := sha256.Sum256([]byte("legacy-" + suffix))
	legacyPrefix := "cgk_old_" + suffix
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO api_key_history (id,user_id,device_id,key_prefix,created_at)
		VALUES ($1,$2,$3,$4,$5)`, legacyID, user.ID, device.ID, legacyPrefix, now); err != nil {
		t.Fatalf("insert legacy API key history: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO api_keys
			(id,public_id,key_prefix,key_hash,user_id,device_id,name,created_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,'legacy key',$7,$8)`, legacyID,
		"legacykey"+suffix, legacyPrefix, legacyHash[:], user.ID, device.ID,
		now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("insert legacy API key: %v", err)
	}
	legacy, err := repository.GetAPIKey(ctx, user.ID, legacyID)
	if err != nil || legacy.SecretAvailable {
		t.Fatalf("legacy API key availability = %v, err %v", legacy.SecretAvailable, err)
	}
	legacySecret, err := repository.GetAPIKeySecret(ctx, user.ID, legacyID)
	if err != nil || len(legacySecret.SecretCiphertext) != 0 {
		t.Fatalf("legacy API key secret = %d bytes, err %v", len(legacySecret.SecretCiphertext), err)
	}
	if _, err := repository.DeleteAPIKey(ctx, user.ID, legacyID); err != nil {
		t.Fatalf("DeleteAPIKey(legacy): %v", err)
	}

	operationID, _ := newUUID()
	if _, err := repository.RechargeUser(ctx, RechargeUserParams{
		BillingWriteParams: BillingWriteParams{
			OperationID: operationID, Reason: "fund lifecycle settlement",
			ActorUserID: user.ID, At: now,
		},
		UserID: user.ID, CNYAmount: "5",
	}); err != nil {
		t.Fatalf("RechargeUser: %v", err)
	}

	requestID := "key-lifecycle-request-" + suffix
	requestedAt := now.Add(4 * time.Second)
	if _, err := repository.AdmitRequest(ctx, AdmitRequestParams{
		Quota: ReserveQuotaParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID,
			Day: requestedAt, Now: requestedAt, LeaseTTL: time.Minute,
			Limits: QuotaLimits{
				KeyRequestsPerMinute: 10, UserRequestsPerMinute: 20,
				KeyConcurrent: 2, UserConcurrent: 4, GlobalConcurrent: 8,
				KeyDailyRequests: 100, UserDailyRequests: 200,
			},
		},
		Usage: BeginUsageRequestParams{
			RequestID: requestID, UserID: user.ID, DeviceID: device.ID,
			APIKeyID: key.ID, ProjectID: project.ID, Model: "lifecycle-model",
			Endpoint: "responses", RequestedAt: requestedAt,
		},
		Billing: &BillingReservationParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID,
			Model: "lifecycle-model", InputUSDPerMillion: "1",
			CachedInputUSDPerMillion: "0.1", OutputUSDPerMillion: "2",
			Now: requestedAt,
		},
	}); err != nil {
		t.Fatalf("AdmitRequest: %v", err)
	}

	deleted, err := repository.DeleteAPIKey(ctx, user.ID, key.ID)
	if err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if deleted.ID != key.ID || deleted.UserID != user.ID ||
		deleted.DeviceID != device.ID || deleted.KeyPrefix != key.KeyPrefix {
		t.Fatalf("deleted API key history = %+v", deleted)
	}
	if _, err := repository.DeleteAPIKey(ctx, user.ID, key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeated DeleteAPIKey error = %v, want ErrNotFound", err)
	}
	if _, err := repository.LookupAPIKey(ctx, key.PublicID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupAPIKey after delete error = %v, want ErrNotFound", err)
	}
	if _, err := repository.GetAPIKeySecret(ctx, user.ID, key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAPIKeySecret after delete error = %v, want ErrNotFound", err)
	}

	if _, err := repository.AppendAuditEvent(ctx, AppendAuditEventParams{
		OccurredAt: requestedAt, ActorUserID: user.ID, ActorAPIKeyID: key.ID,
		EventType: "api_key.lifecycle_test", Success: true,
	}); err != nil {
		t.Fatalf("AppendAuditEvent with deleted actor key: %v", err)
	}
	completedAt := requestedAt.Add(time.Second)
	if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: requestID, State: "completed", HTTPStatus: 200,
		CompletedAt: completedAt, InputTokens: 1_000_000,
	}); err != nil {
		t.Fatalf("CompleteUsageRequest after key deletion: %v", err)
	}
	if err := repository.SettleRequest(ctx, requestID, completedAt); err != nil {
		t.Fatalf("SettleRequest after key deletion: %v", err)
	}
	lastUsedAt := completedAt.Add(time.Second)
	if err := repository.RecordAPIKeyUse(ctx, key.ID, device.ID, lastUsedAt); err != nil {
		t.Fatalf("RecordAPIKeyUse after key deletion: %v", err)
	}
	if err := repository.AggregateUsageDay(ctx, requestedAt, "UTC"); err != nil {
		t.Fatalf("AggregateUsageDay after key deletion: %v", err)
	}
	if err := repository.AggregateUsageMonth(ctx, requestedAt, "UTC"); err != nil {
		t.Fatalf("AggregateUsageMonth after key deletion: %v", err)
	}

	from, until := requestedAt.Add(-time.Second), completedAt.Add(time.Second)
	summary, err := repository.SummarizeUsageRequests(ctx, UsageFilter{
		From: &from, Until: &until, UserID: user.ID, DeviceID: device.ID,
		APIKeyID: key.ID, ProjectID: project.ID, Model: "lifecycle-model",
		State: "completed", HTTPStatus: 200,
	})
	if err != nil {
		t.Fatalf("SummarizeUsageRequests: %v", err)
	}
	if summary.RequestCount != 1 || summary.InputTokens != 1_000_000 ||
		summary.OutputTokens != 0 || summary.ChargedUSD != "1.000000000000" {
		t.Fatalf("usage summary after key deletion = %+v", summary)
	}

	var activeCount, historyCount, dailyCount, monthlyCount, ledgerCount, auditCount int
	var quotaState, billingState, historyPrefix string
	var deviceLastSeen time.Time
	if err := repository.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM api_keys WHERE id=$1),
		(SELECT count(*) FROM api_key_history WHERE id=$1),
		(SELECT count(*) FROM usage_daily WHERE api_key_id=$1),
		(SELECT count(*) FROM usage_monthly WHERE api_key_id=$1),
		(SELECT count(*) FROM billing_ledger_entries
		 WHERE request_id=$2 AND entry_type='usage_charge'),
		(SELECT count(*) FROM audit_events WHERE actor_api_key_id=$1),
		(SELECT state FROM quota_reservations WHERE request_id=$2),
		(SELECT state FROM billing_reservations WHERE request_id=$2),
		(SELECT key_prefix FROM api_key_history WHERE id=$1),
		(SELECT last_seen_at FROM devices WHERE id=$3)`,
		key.ID, requestID, device.ID,
	).Scan(
		&activeCount, &historyCount, &dailyCount, &monthlyCount, &ledgerCount,
		&auditCount, &quotaState, &billingState, &historyPrefix, &deviceLastSeen,
	); err != nil {
		t.Fatalf("read retained API key accounting: %v", err)
	}
	if activeCount != 0 || historyCount != 1 || dailyCount != 1 || monthlyCount != 1 ||
		ledgerCount != 1 || auditCount != 1 || quotaState != "settled" ||
		billingState != "settled" || historyPrefix != key.KeyPrefix ||
		!deviceLastSeen.Equal(lastUsedAt) {
		t.Fatalf("retained API key accounting = active/history %d/%d daily/monthly %d/%d ledger/audit %d/%d quota/billing %s/%s prefix %q last_seen %v",
			activeCount, historyCount, dailyCount, monthlyCount, ledgerCount, auditCount,
			quotaState, billingState, historyPrefix, deviceLastSeen)
	}
}
