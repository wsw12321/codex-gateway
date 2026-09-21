//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestModelAccessBatchPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	models := []string{"batch-a-" + suffix, "batch-b-" + suffix}
	actor := modelAccessIntegrationUser(t, ctx, repository, "batch-actor-"+suffix, UserRoleMember)
	u1 := modelAccessIntegrationUser(t, ctx, repository, "batch-one-"+suffix, UserRoleMember)
	u2 := modelAccessIntegrationUser(t, ctx, repository, "batch-two-"+suffix, UserRoleMember)
	if err := repository.SyncModelAccessCatalog(ctx, models); err != nil {
		t.Fatal(err)
	}
	write := ModelAccessWriteParams{ActorUserID: actor.ID, Reason: "batch regression"}
	params := SetUserModelAccessBatchParams{ModelAccessWriteParams: write, Models: models, Enabled: false, Scope: ModelAccessScopeSelected, UserIDs: []string{u1.ID, u2.ID}}
	result, err := repository.SetUserModelAccessBatch(ctx, params)
	if err != nil || result.TargetCount != 4 || result.ChangedCount != 4 || len(result.Results) != 2 {
		t.Fatalf("selected result %+v: %v", result, err)
	}
	for _, model := range models {
		for _, user := range []string{u1.ID, u2.ID} {
			if err := repository.RequireModelAccess(ctx, user, model); !errors.Is(err, ErrModelNotAllowed) {
				t.Fatalf("%s/%s: %v", user, model, err)
			}
		}
		if err := repository.RequireModelAccess(ctx, actor.ID, model); err != nil {
			t.Fatalf("unselected actor changed: %v", err)
		}
	}
	rows, err := repository.ListModelAccessUsersBatch(ctx, models)
	if err != nil {
		t.Fatal(err)
	}
	pairs := 0
	for _, row := range rows {
		if row.UserID == u1.ID || row.UserID == u2.ID {
			pairs++
			if row.Enabled {
				t.Fatal("batch read lost disabled state")
			}
		}
	}
	if pairs != 4 {
		t.Fatalf("queried %d target pairs", pairs)
	}

	result, err = repository.SetUserModelAccessBatch(ctx, params)
	if err != nil || result.ChangedCount != 0 {
		t.Fatalf("idempotent batch %+v: %v", result, err)
	}
	var audits int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND event_type='model_access.users_changed'`, actor.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit count %d: %v", audits, err)
	}

	// Validate all models and users before changing any of the Cartesian product.
	params.Enabled = true
	params.Models = []string{models[0], "missing-" + suffix}
	if _, err := repository.SetUserModelAccessBatch(ctx, params); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing model error: %v", err)
	}
	params.Models = models
	missing, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	params.UserIDs = []string{u1.ID, missing}
	if _, err := repository.SetUserModelAccessBatch(ctx, params); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing user error: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, u1.ID, models[0]); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("invalid batch modified permissions: %v", err)
	}

	// A missing snapshot discovered after the first model write rolls back
	// both that write and its audit entry.
	params.UserIDs = []string{u1.ID, u2.ID}
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM user_model_access WHERE user_id=$1 AND model=$2`, u2.ID, models[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUserModelAccessBatch(ctx, params); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("corrupted batch error: %v", err)
	}
	if err := repository.RequireModelAccess(ctx, u1.ID, models[0]); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("partial batch leaked: %v", err)
	}
	if _, err := repository.ListModelAccessUsersBatch(ctx, models); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("batch query did not fail closed: %v", err)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND event_type='model_access.users_changed'`, actor.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("rolled-back audit count %d: %v", audits, err)
	}
	if err := repository.SyncModelAccessCatalog(ctx, models); err != nil {
		t.Fatal(err)
	}

	// Defaults apply only to registrations made after the atomic batch.
	defaults, err := repository.SetModelAccessDefaultsBatch(ctx, SetModelAccessDefaultsBatchParams{ModelAccessWriteParams: write, Models: models, Enabled: false})
	if err != nil || defaults.TargetCount != 2 || defaults.ChangedCount != 2 {
		t.Fatalf("defaults %+v: %v", defaults, err)
	}
	future := modelAccessIntegrationUser(t, ctx, repository, "batch-future-"+suffix, UserRoleMember)
	for _, model := range models {
		if err := repository.RequireModelAccess(ctx, future.ID, model); !errors.Is(err, ErrModelNotAllowed) {
			t.Fatalf("future defaults: %v", err)
		}
		if err := repository.RequireModelAccess(ctx, actor.ID, model); err != nil {
			t.Fatalf("defaults changed existing user: %v", err)
		}
	}
	params.Scope, params.UserIDs = ModelAccessScopeAll, nil
	result, err = repository.SetUserModelAccessBatch(ctx, params)
	if err != nil || result.TargetCount < 8 || len(result.Results) != 2 {
		t.Fatalf("all batch %+v: %v", result, err)
	}
	for _, model := range models {
		if err := repository.RequireModelAccess(ctx, future.ID, model); err != nil {
			t.Fatalf("all users not enabled: %v", err)
		}
	}
	afterAll := modelAccessIntegrationUser(t, ctx, repository, "batch-after-all-"+suffix, UserRoleMember)
	for _, model := range models {
		if err := repository.RequireModelAccess(ctx, afterAll.ID, model); !errors.Is(err, ErrModelNotAllowed) {
			t.Fatalf("all users changed default: %v", err)
		}
	}
}
