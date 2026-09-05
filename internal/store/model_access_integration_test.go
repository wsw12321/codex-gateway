//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestModelAccessPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	modelA := "access-a-" + suffix
	modelB := "access-b-" + suffix
	actor := modelAccessIntegrationUser(t, ctx, repository, "access-actor-"+suffix, UserRoleMember)
	existing := modelAccessIntegrationUser(t, ctx, repository, "access-existing-"+suffix, UserRoleMember)
	disabled := modelAccessIntegrationUser(t, ctx, repository, "access-disabled-"+suffix, UserRoleMember)
	if err := repository.DisableUser(ctx, disabled.ID, time.Now().UTC()); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	ownerID := modelAccessIntegrationOwner(t, ctx, repository, suffix)

	if err := repository.SyncModelAccessCatalog(ctx, []string{modelB, modelA, modelA}); err != nil {
		t.Fatalf("SyncModelAccessCatalog: %v", err)
	}
	for _, userID := range []string{actor.ID, existing.ID, disabled.ID, ownerID} {
		if err := repository.RequireModelAccess(ctx, userID, modelA); err != nil {
			t.Fatalf("RequireModelAccess(backfilled %s): %v", userID, err)
		}
		if err := repository.RequireModelAccess(ctx, userID, modelB); err != nil {
			t.Fatalf("RequireModelAccess(backfilled %s, model B): %v", userID, err)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	defaultResult, err := repository.SetModelAccessDefault(ctx, SetModelAccessDefaultParams{
		ModelAccessWriteParams: ModelAccessWriteParams{
			ActorUserID: actor.ID, Reason: "disable model B for future registrations", At: now,
		},
		Model: modelB, Enabled: false,
	})
	if err != nil {
		t.Fatalf("SetModelAccessDefault: %v", err)
	}
	if defaultResult.Scope != ModelAccessScopeDefault || defaultResult.TargetCount != 1 || defaultResult.ChangedCount != 1 {
		t.Fatalf("default result = %+v", defaultResult)
	}
	if err := repository.RequireModelAccess(ctx, existing.ID, modelB); err != nil {
		t.Fatalf("default change retroactively changed existing user: %v", err)
	}
	newUser := modelAccessIntegrationUser(t, ctx, repository, "access-new-"+suffix, UserRoleMember)
	if err := repository.RequireModelAccess(ctx, newUser.ID, modelA); err != nil {
		t.Fatalf("new user model A snapshot: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, newUser.ID, modelB); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("new user model B snapshot error = %v, want ErrModelNotAllowed", err)
	}

	selectedIDs := []string{existing.ID, disabled.ID, ownerID}
	selectedResult, err := repository.SetUserModelAccess(ctx, SetUserModelAccessParams{
		ModelAccessWriteParams: ModelAccessWriteParams{
			ActorUserID: actor.ID, Reason: "selected integration disable", At: now.Add(time.Second),
		},
		Model: modelA, Enabled: false, Scope: ModelAccessScopeSelected, UserIDs: selectedIDs,
	})
	if err != nil {
		t.Fatalf("SetUserModelAccess(selected): %v", err)
	}
	if selectedResult.TargetCount != int64(len(selectedIDs)) || selectedResult.ChangedCount != int64(len(selectedIDs)) {
		t.Fatalf("selected result = %+v", selectedResult)
	}
	users, err := repository.ListModelAccessUsers(ctx, modelA)
	if err != nil {
		t.Fatalf("ListModelAccessUsers: %v", err)
	}
	assertModelAccessIntegrationUser(t, users, disabled.ID, StatusDisabled, false)
	assertModelAccessIntegrationUser(t, users, ownerID, "", false)

	device, err := repository.CreateDevice(ctx, CreateDeviceParams{UserID: existing.ID, Name: "model-access-test"})
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	keyHash := sha256.Sum256([]byte("model-access-key-" + suffix))
	key, err := repository.CreateAPIKey(ctx, CreateAPIKeyParams{
		PublicID: "makey_" + suffix, KeyPrefix: "cgk_model_" + suffix,
		KeyHash: keyHash[:], SecretCiphertext: []byte{1}, UserID: existing.ID,
		DeviceID: device.ID, Name: "model access admission", CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	deniedRequestID := "model-access-denied-" + suffix
	before := modelAccessIntegrationAdmissionArtifacts(t, ctx, repository, existing.ID, key.ID, deniedRequestID)
	_, err = repository.AdmitRequest(ctx, modelAccessIntegrationAdmission(existing.ID, device.ID, key.ID, modelA, deniedRequestID, now.Add(2*time.Second)))
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("AdmitRequest(disabled) error = %v, want ErrModelNotAllowed", err)
	}
	after := modelAccessIntegrationAdmissionArtifacts(t, ctx, repository, existing.ID, key.ID, deniedRequestID)
	if after != before {
		t.Fatalf("disabled admission artifacts changed from %d to %d", before, after)
	}

	missingID, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.SetUserModelAccess(ctx, SetUserModelAccessParams{
		ModelAccessWriteParams: ModelAccessWriteParams{
			ActorUserID: actor.ID, Reason: "invalid batch must roll back", At: now.Add(3 * time.Second),
		},
		Model: modelA, Enabled: true, Scope: ModelAccessScopeSelected,
		UserIDs: []string{existing.ID, missingID},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("SetUserModelAccess(invalid selected) error = %v, want ErrInvalid", err)
	}
	if err := repository.RequireModelAccess(ctx, existing.ID, modelA); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("invalid selected batch did not roll back: %v", err)
	}

	allResult, err := repository.SetUserModelAccess(ctx, SetUserModelAccessParams{
		ModelAccessWriteParams: ModelAccessWriteParams{
			ActorUserID: actor.ID, Reason: "all existing integration disable", At: now.Add(4 * time.Second),
		},
		Model: modelA, Enabled: false, Scope: ModelAccessScopeAll,
	})
	if err != nil {
		t.Fatalf("SetUserModelAccess(all): %v", err)
	}
	if allResult.TargetCount < int64(len(selectedIDs)+2) || allResult.ChangedCount == 0 {
		t.Fatalf("all result = %+v", allResult)
	}
	postBatch := modelAccessIntegrationUser(t, ctx, repository, "access-post-batch-"+suffix, UserRoleMember)
	if err := repository.RequireModelAccess(ctx, postBatch.ID, modelA); err != nil {
		t.Fatalf("all-existing batch changed future default: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, postBatch.ID, modelB); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("post-batch model B snapshot error = %v, want ErrModelNotAllowed", err)
	}

	var auditCount int
	if err := repository.DB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events
		WHERE actor_user_id = $1 AND subject_type = 'model' AND subject_id IN ($2,$3)
		AND event_type IN ('model_access.default_changed','model_access.users_changed')
		AND metadata ? 'reason'`, actor.ID, modelA, modelB).Scan(&auditCount); err != nil {
		t.Fatalf("count model access audits: %v", err)
	}
	if auditCount != 3 {
		t.Fatalf("model access audit count = %d, want 3", auditCount)
	}

	if err := repository.SyncModelAccessCatalog(ctx, []string{modelA}); err != nil {
		t.Fatalf("remove model B from catalog: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, actor.ID, modelB); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("removed model access error = %v, want ErrModelAccessUnavailable", err)
	}
	models, err := repository.ListModelAccessModels(ctx)
	if err != nil {
		t.Fatalf("ListModelAccessModels(removed): %v", err)
	}
	for _, model := range models {
		if model.Model == modelB {
			t.Fatalf("removed model %q remains in active list", modelB)
		}
	}
	if err := repository.SyncModelAccessCatalog(ctx, []string{modelA, modelB}); err != nil {
		t.Fatalf("reactivate model B: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, actor.ID, modelB); err != nil {
		t.Fatalf("reactivation overwrote existing enabled access: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, postBatch.ID, modelB); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("reactivation overwrote existing disabled access: %v", err)
	}

	if _, err := repository.DB().ExecContext(ctx, `DELETE FROM user_model_access
		WHERE user_id = $1 AND model = $2`, existing.ID, modelA); err != nil {
		t.Fatalf("delete model access row for fail-closed test: %v", err)
	}
	missingRequestID := "model-access-missing-" + suffix
	before = modelAccessIntegrationAdmissionArtifacts(t, ctx, repository, existing.ID, key.ID, missingRequestID)
	_, err = repository.AdmitRequest(ctx, modelAccessIntegrationAdmission(existing.ID, device.ID, key.ID, modelA, missingRequestID, now.Add(5*time.Second)))
	if !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("AdmitRequest(missing access) error = %v, want ErrModelAccessUnavailable", err)
	}
	after = modelAccessIntegrationAdmissionArtifacts(t, ctx, repository, existing.ID, key.ID, missingRequestID)
	if after != before {
		t.Fatalf("missing-access admission artifacts changed from %d to %d", before, after)
	}
	if _, err := repository.ListEnabledModelsForUser(ctx, existing.ID); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("ListEnabledModelsForUser(missing row) error = %v, want ErrModelAccessUnavailable", err)
	}
	if err := repository.SyncModelAccessCatalog(ctx, []string{modelA, modelB}); err != nil {
		t.Fatalf("repair missing model access row: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, existing.ID, modelA); err != nil {
		t.Fatalf("catalog sync did not repair missing row from default: %v", err)
	}
}

func modelAccessIntegrationUser(t *testing.T, ctx context.Context, repository *Store, username, role string) User {
	t.Helper()
	user, err := repository.CreateUser(ctx, CreateUserParams{Username: username, DisplayName: username, Role: role})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	return user
}

func modelAccessIntegrationOwner(t *testing.T, ctx context.Context, repository *Store, suffix string) string {
	t.Helper()
	var ownerID string
	err := repository.DB().QueryRowContext(ctx, `SELECT id FROM users
		WHERE role = 'owner' AND status = 'active' ORDER BY created_at LIMIT 1`).Scan(&ownerID)
	if err == nil {
		return ownerID
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("find existing Owner: %v", err)
	}
	owner := modelAccessIntegrationUser(t, ctx, repository, "access-owner-"+suffix, UserRoleOwner)
	if err := repository.DisableUser(ctx, owner.ID, time.Now().UTC()); err != nil {
		t.Fatalf("disable temporary Owner: %v", err)
	}
	return owner.ID
}

func assertModelAccessIntegrationUser(t *testing.T, users []ModelAccessUser, userID, status string, enabled bool) {
	t.Helper()
	for _, user := range users {
		if user.UserID != userID {
			continue
		}
		if status != "" && user.Status != status {
			t.Fatalf("model access user %s status = %q, want %q", userID, user.Status, status)
		}
		if user.Enabled != enabled {
			t.Fatalf("model access user %s enabled = %t, want %t", userID, user.Enabled, enabled)
		}
		return
	}
	t.Fatalf("model access user %s not listed", userID)
}

func modelAccessIntegrationAdmission(userID, deviceID, keyID, model, requestID string, at time.Time) AdmitRequestParams {
	return AdmitRequestParams{
		Quota: ReserveQuotaParams{
			RequestID: requestID, UserID: userID, APIKeyID: keyID, Now: at,
			ReservedTokens: 1, LeaseTTL: time.Minute,
			Limits: QuotaLimits{
				KeyRequestsPerMinute: 100, UserRequestsPerMinute: 100,
				KeyConcurrent: 10, UserConcurrent: 10, GlobalConcurrent: 100,
				KeyDailyRequests: 1000, UserDailyRequests: 1000,
			},
		},
		Usage: BeginUsageRequestParams{
			RequestID: requestID, UserID: userID, DeviceID: deviceID, APIKeyID: keyID,
			Model: model, Endpoint: "models", RequestedAt: at,
		},
		RequireModelAccess: true,
	}
}

func modelAccessIntegrationAdmissionArtifacts(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	userID, keyID, requestID string,
) int {
	t.Helper()
	var count int
	if err := repository.DB().QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM quota_reservations WHERE request_id = $3) +
			(SELECT count(*) FROM concurrency_leases WHERE request_id = $3) +
			(SELECT count(*) FROM usage_requests WHERE request_id = $3) +
			(SELECT count(*) FROM billing_reservations WHERE request_id = $3) +
			(SELECT count(*) FROM quota_rate_windows
			 WHERE (scope_type = 'user' AND scope_id = $1)
			    OR (scope_type = 'key' AND scope_id = $2)) +
			(SELECT count(*) FROM quota_counters
			 WHERE (scope_type = 'user' AND scope_id = $1)
			    OR (scope_type = 'key' AND scope_id = $2))`,
		userID, keyID, requestID).Scan(&count); err != nil {
		t.Fatalf("count admission artifacts: %v", err)
	}
	return count
}
