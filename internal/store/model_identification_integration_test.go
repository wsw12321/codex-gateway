//go:build integration

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func TestModelIdentificationLifecyclePostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	firstID := upstreamIntegrationAccountID("identification-first-" + t.Name())
	secondID := upstreamIntegrationAccountID("identification-second-" + t.Name())
	for _, id := range []string{firstID, secondID} {
		if err := s.EnsureUpstreamAccount(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts
			SET status = 'available' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = s.db.ExecContext(cleanupCtx,
			`DELETE FROM model_identifications WHERE account_id IN ($1,$2)`, firstID, secondID)
	})

	// Diagnostics do not inherit the account's ordinary disabled/quota status.
	if _, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET status = 'unavailable' WHERE id = $1`, firstID); err != nil {
		t.Fatal(err)
	}
	actorID := "00000000-0000-0000-0000-000000000007"
	first, err := s.BeginModelIdentificationRun(ctx, firstID, "gpt-test", actorID)
	if err != nil || first.RunStatus != ModelIdentificationRunning || first.RunProgress != 0 || first.RunActorID != actorID || first.RunStage != "preflight" || first.RunUpdatedAt == nil {
		t.Fatalf("first run=%+v err=%v", first, err)
	}
	if _, err := s.BeginModelIdentificationRun(ctx, secondID, "gpt-test"); !errors.Is(err, ErrConflict) {
		t.Fatalf("global second run error=%v; want conflict", err)
	}
	if _, err := s.BeginModelIdentificationRun(ctx, firstID, "gpt-test"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same-key second run error=%v; want conflict", err)
	}
	if _, err := s.AdvanceModelIdentificationRun(ctx, first.RunID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceModelIdentificationRun(ctx, first.RunID, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeated progress error=%v; want conflict", err)
	}
	if _, err := s.UpdateModelIdentificationRun(ctx, first.RunID, "probing", 2, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateModelIdentificationRun(ctx, first.RunID, "validating", 2, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateModelIdentificationRun(ctx, first.RunID, "probing", 2, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("phase regressed: %v", err)
	}
	if _, err := s.UpdateModelIdentificationRun(ctx, first.RunID, "probing", 1, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("probe regressed: %v", err)
	}
	if _, err := s.UpdateModelIdentificationRun(ctx, first.RunID, "saving", 3, 3); err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteModelIdentificationRun(ctx, first.RunID, ModelIdentificationResult{
		Conclusion: "无法可靠判定", ClosestModel: "reference-x", MatchLevel: "unreliable",
		Fit: -0.01, Margin: -0.003813972697052005, ReferenceVersion: "v1",
	})
	if err != nil || completed.RunStatus != ModelIdentificationSucceeded ||
		completed.Conclusion != "无法可靠判定" || completed.ClosestModel != "reference-x" ||
		completed.ExpiresAt == nil || completed.CompletedAt == nil ||
		completed.Fit == nil || *completed.Fit != -0.01 ||
		completed.Margin == nil || *completed.Margin != -0.003813972697052005 ||
		completed.ExpiresAt.Sub(*completed.CompletedAt) != ModelIdentificationValidity {
		t.Fatalf("completed run=%+v err=%v", completed, err)
	}
	if _, err := s.UpdateModelIdentificationRun(ctx, first.RunID, "saving", 3, 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal phase mutated: %v", err)
	}
	if _, err := s.FailModelIdentificationRun(ctx, first.RunID, "probe_failed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal run mutation error=%v; want conflict", err)
	}

	retest, err := s.BeginModelIdentificationRun(ctx, firstID, "gpt-test")
	if err != nil || retest.Conclusion != completed.Conclusion {
		t.Fatalf("retest=%+v err=%v; valid result lost", retest, err)
	}
	if _, err := s.GetModelIdentificationRun(ctx, first.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old run resolved after replacement: %v", err)
	}
	if _, err := s.FailModelIdentificationRun(ctx, first.RunID, "probe_failed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old run could mutate replacement: %v", err)
	}
	failed, err := s.FailModelIdentificationRun(ctx, retest.RunID, "model_identification_rate_limited", ModelIdentificationFailure{Source: "upstream", UpstreamStatus: 429, RetryAfter: 12})
	if err != nil || failed.RunStatus != ModelIdentificationFailed ||
		failed.RunErrorCode != "model_identification_rate_limited" || failed.RunErrorSource != "upstream" || failed.RunUpstreamStatus != 429 || failed.RunRetryAfter != 12 || failed.Conclusion != completed.Conclusion ||
		failed.ClosestModel != completed.ClosestModel {
		t.Fatalf("failed retest=%+v err=%v; old result lost", failed, err)
	}

	second, err := s.BeginModelIdentificationRun(ctx, secondID, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.RecoverInterruptedModelIdentificationRuns(ctx); err != nil || recovered != 0 {
		t.Fatalf("recovered fresh live run=%d err=%v", recovered, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE model_identifications
		SET run_started_at = $2 WHERE run_id = $1::uuid`, second.RunID,
		time.Now().Add(-11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.RecoverInterruptedModelIdentificationRuns(ctx); err != nil || recovered != 1 {
		t.Fatalf("recovered stale run=%d err=%v", recovered, err)
	}
	if _, err := s.CompleteModelIdentificationRun(ctx, second.RunID, ModelIdentificationResult{
		Conclusion: "reference-y", ClosestModel: "reference-y", MatchLevel: "strong",
		Fit: 0.9, Margin: 0.2, ReferenceVersion: "v1",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("interrupted run completion error=%v; want conflict", err)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE model_identifications
		SET completed_at = $2, expires_at = $3 WHERE account_id = $1`,
		firstID, time.Now().Add(-31*24*time.Hour), time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListModelIdentifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.AccountID == firstID && (item.Conclusion != "" || item.ClosestModel != "" ||
			item.Fit != nil || item.ExpiresAt != nil || item.RunStatus != ModelIdentificationFailed) {
			t.Fatalf("expired verdict not purged while run status retained: %+v", item)
		}
		if item.AccountID == secondID &&
			(item.RunStatus != ModelIdentificationFailed || item.RunErrorCode != "model_identification_interrupted") {
			t.Fatalf("interrupted status not shown: %+v", item)
		}
	}

	// GET/list hide expired verdicts without mutating database state. Maintenance
	// alone removes them, while preserving safe task metadata.
	var persisted sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT conclusion FROM model_identifications WHERE account_id=$1`, firstID).Scan(&persisted); err != nil || !persisted.Valid {
		t.Fatalf("read purged durable verdict: %v %+v", err, persisted)
	}
	read, err := s.GetModelIdentificationRun(ctx, retest.RunID)
	if err != nil || read.Conclusion != "" || read.RunErrorSource != "upstream" {
		t.Fatalf("read expired result: %+v %v", read, err)
	}
	if _, err := s.PurgeExpiredModelIdentifications(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT conclusion FROM model_identifications WHERE account_id=$1`, firstID).Scan(&persisted); err != nil || persisted.Valid {
		t.Fatalf("maintenance failed to purge: %v %+v", err, persisted)
	}

	strongRun, err := s.BeginModelIdentificationRun(ctx, firstID, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	strong, err := s.CompleteModelIdentificationRun(ctx, strongRun.RunID, ModelIdentificationResult{
		Conclusion: "reference-y", ClosestModel: "reference-y", MatchLevel: "match",
		Fit: 0.9, Margin: 0.2, ReferenceVersion: "v2",
	})
	if err != nil || strong.Conclusion != "reference-y" || strong.ReferenceVersion != "v2" {
		t.Fatalf("successful retest did not replace old result: %+v err=%v", strong, err)
	}
	weakRun, err := s.BeginModelIdentificationRun(ctx, firstID, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	weak, err := s.CompleteModelIdentificationRun(ctx, weakRun.RunID, ModelIdentificationResult{
		Conclusion: "无法可靠判定", ClosestModel: "reference-x", MatchLevel: "insufficient",
		Fit: 0.03, Margin: -0.01, ReferenceVersion: "v2",
	})
	if err != nil || weak.Conclusion != "无法可靠判定" || weak.ClosestModel != "reference-x" ||
		weak.MatchLevel != "insufficient" || weak.RunStatus != ModelIdentificationSucceeded {
		t.Fatalf("weak retest did not replace strong result: %+v err=%v", weak, err)
	}
}

// A rolling deployment or rollback can leave an old gateway using only the
// original columns. New metadata must not break its reservation or completion.
func TestModelIdentificationLegacyWritesPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	accountID := upstreamIntegrationAccountID("identification-legacy-" + t.Name())
	if err := s.EnsureUpstreamAccount(ctx, accountID, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = s.db.ExecContext(cleanup, `DELETE FROM model_identifications WHERE account_id=$1`, accountID)
	})
	for _, outcome := range []string{"failed", "succeeded"} {
		t.Run(outcome, func(t *testing.T) {
			runID, err := newUUID()
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			if _, err := s.db.ExecContext(ctx, `INSERT INTO model_identifications
				(account_id, requested_model, run_id, run_status, run_progress, run_started_at)
				VALUES ($1,$2,$3::uuid,'running',0,$4)`, accountID, "legacy-"+outcome, runID, at); err != nil {
				t.Fatalf("old reservation rejected: %v", err)
			}
			run, err := s.GetModelIdentificationRun(ctx, runID)
			if err != nil || run.RunUpdatedAt == nil || run.RunStage != "preflight" || run.RunActorID != "" {
				t.Fatalf("metadata defaults=%+v err=%v", run, err)
			}
			if outcome == "failed" {
				_, err = s.db.ExecContext(ctx, `UPDATE model_identifications SET
					run_status='failed', run_finished_at=$2, run_error_code='probe_failed'
					WHERE run_id=$1::uuid`, runID, at)
			} else {
				_, err = s.db.ExecContext(ctx, `UPDATE model_identifications SET
					conclusion='reference', closest_model='reference', match_level='match',
					fit=0.9, margin=0.2, reference_version='legacy', completed_at=$2, expires_at=$3,
					run_status='succeeded', run_progress=3, run_finished_at=$2, run_error_code=NULL
					WHERE run_id=$1::uuid`, runID, at, at.Add(ModelIdentificationValidity))
			}
			if err != nil {
				t.Fatalf("old terminal update rejected: %v", err)
			}
		})
	}
}
