package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ModelIdentificationRunning   = "running"
	ModelIdentificationSucceeded = "succeeded"
	ModelIdentificationFailed    = "failed"

	ModelIdentificationValidity = 30 * 24 * time.Hour
	// A live run is capped at ten minutes. The extra thirty seconds allow
	// cancellation and its terminal database update to complete.
	modelIdentificationRecoveryAfter = 10*time.Minute + 30*time.Second
)

var modelIdentificationCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ModelIdentification contains only the latest statistical match and current
// run metadata. No prompt, response, response digest, or usage belongs here.
type ModelIdentification struct {
	AccountID        string     `json:"account_id"`
	RequestedModel   string     `json:"requested_model"`
	Conclusion       string     `json:"conclusion,omitempty"`
	ClosestModel     string     `json:"closest_model,omitempty"`
	MatchLevel       string     `json:"match_level,omitempty"`
	Fit              *float64   `json:"fit,omitempty"`
	Margin           *float64   `json:"margin,omitempty"`
	ReferenceVersion string     `json:"reference_version,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	RunID            string     `json:"run_id"`
	RunStatus        string     `json:"run_status"`
	RunProgress      int        `json:"run_progress"`
	RunStartedAt     *time.Time `json:"run_started_at,omitempty"`
	RunFinishedAt    *time.Time `json:"run_finished_at,omitempty"`
	RunErrorCode     string     `json:"run_error_code,omitempty"`
}

type ModelIdentificationResult struct {
	Conclusion       string
	ClosestModel     string
	MatchLevel       string
	Fit              float64
	Margin           float64
	ReferenceVersion string
}

const modelIdentificationColumns = `account_id, requested_model, conclusion, closest_model,
	match_level, fit, margin, reference_version, completed_at, expires_at,
	run_id, run_status, run_progress, run_started_at, run_finished_at,
	run_error_code`

func scanModelIdentification(row rowScanner) (ModelIdentification, error) {
	var item ModelIdentification
	var conclusion, closest, level, version, errorCode sql.NullString
	var fit, margin sql.NullFloat64
	var completed, expires, started, finished sql.NullTime
	err := row.Scan(&item.AccountID, &item.RequestedModel, &conclusion, &closest,
		&level, &fit, &margin, &version, &completed, &expires,
		&item.RunID, &item.RunStatus, &item.RunProgress, &started, &finished,
		&errorCode)
	if err != nil {
		return ModelIdentification{}, err
	}
	item.Conclusion = conclusion.String
	item.ClosestModel = closest.String
	item.MatchLevel = level.String
	item.ReferenceVersion = version.String
	item.RunErrorCode = errorCode.String
	if fit.Valid {
		item.Fit = &fit.Float64
	}
	if margin.Valid {
		item.Margin = &margin.Float64
	}
	if completed.Valid {
		item.CompletedAt = &completed.Time
	}
	if expires.Valid {
		item.ExpiresAt = &expires.Time
	}
	if started.Valid {
		item.RunStartedAt = &started.Time
	}
	if finished.Valid {
		item.RunFinishedAt = &finished.Time
	}
	return item, nil
}

func validModelIdentificationText(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

// BeginModelIdentificationRun reserves the single global diagnostic slot.
// A database index enforces this across Gateway replicas. An expired result is
// cleared at the same time; an unexpired result remains visible while retesting.
func (s *Store) BeginModelIdentificationRun(ctx context.Context, accountID, model string) (ModelIdentification, error) {
	if !upstreamAccountIDPattern.MatchString(accountID) ||
		!validModelIdentificationText(model, 200) {
		return ModelIdentification{}, fmt.Errorf("%w: invalid account or model", ErrInvalid)
	}
	runID, err := newUUID()
	if err != nil {
		return ModelIdentification{}, err
	}
	if _, err := s.RecoverInterruptedModelIdentificationRuns(ctx); err != nil {
		return ModelIdentification{}, err
	}
	at := s.now().UTC()
	item, err := scanModelIdentification(s.db.QueryRowContext(ctx, `
		INSERT INTO model_identifications
			(account_id, requested_model, run_id, run_status, run_progress,
			 run_started_at)
		SELECT id, $2, $3::uuid, 'running', 0, $4
		FROM upstream_accounts WHERE id = $1 AND status = 'available'
		ON CONFLICT (account_id, requested_model) DO UPDATE SET
			conclusion = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.conclusion ELSE NULL END,
			closest_model = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.closest_model ELSE NULL END,
			match_level = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.match_level ELSE NULL END,
			fit = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.fit ELSE NULL END,
			margin = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.margin ELSE NULL END,
			reference_version = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.reference_version ELSE NULL END,
			completed_at = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.completed_at ELSE NULL END,
			expires_at = CASE WHEN model_identifications.expires_at > $4
				THEN model_identifications.expires_at ELSE NULL END,
			run_id = EXCLUDED.run_id,
			run_status = 'running',
			run_progress = 0,
			run_started_at = EXCLUDED.run_started_at,
			run_finished_at = NULL,
			run_error_code = NULL
		WHERE model_identifications.run_status <> 'running'
		RETURNING `+modelIdentificationColumns,
		accountID, model, runID, at))
	if errors.Is(err, sql.ErrNoRows) {
		var available bool
		if lookupErr := s.db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM upstream_accounts WHERE id = $1 AND status = 'available')`,
			accountID).Scan(&available); lookupErr != nil {
			return ModelIdentification{}, mapDBError("check model identification account", lookupErr)
		}
		if !available {
			return ModelIdentification{}, fmt.Errorf("begin model identification: %w", ErrNotFound)
		}
		return ModelIdentification{}, fmt.Errorf("begin model identification: %w", ErrConflict)
	}
	if err != nil {
		return ModelIdentification{}, mapDBError("begin model identification", err)
	}
	return item, nil
}

// AdvanceModelIdentificationRun records how many of the three probes have
// completed. Progress can only move forward for the current running ID.
func (s *Store) AdvanceModelIdentificationRun(ctx context.Context, runID string, progress int) (ModelIdentification, error) {
	if !validModelIdentificationRunID(runID) || progress < 1 || progress > 3 {
		return ModelIdentification{}, fmt.Errorf("%w: invalid run or progress", ErrInvalid)
	}
	item, err := scanModelIdentification(s.db.QueryRowContext(ctx, `
		UPDATE model_identifications SET run_progress = $2
		WHERE run_id = $1::uuid AND run_status = 'running'
			AND run_progress < $2
		RETURNING `+modelIdentificationColumns, runID, progress))
	return item, s.modelIdentificationMutationError(ctx, runID, "advance model identification", err)
}

// CompleteModelIdentificationRun replaces the previous verdict, including a
// weak or indeterminate match. The expiration starts at successful completion.
func (s *Store) CompleteModelIdentificationRun(ctx context.Context, runID string, result ModelIdentificationResult) (ModelIdentification, error) {
	if !validModelIdentificationRunID(runID) ||
		!validModelIdentificationText(result.Conclusion, 200) ||
		!validModelIdentificationText(result.ClosestModel, 200) ||
		!modelIdentificationCodePattern.MatchString(result.MatchLevel) ||
		!validModelIdentificationText(result.ReferenceVersion, 128) ||
		math.IsNaN(result.Fit) || math.IsInf(result.Fit, 0) ||
		math.IsNaN(result.Margin) || math.IsInf(result.Margin, 0) {
		return ModelIdentification{}, fmt.Errorf("%w: invalid model identification result", ErrInvalid)
	}
	at := s.now().UTC()
	item, err := scanModelIdentification(s.db.QueryRowContext(ctx, `
		UPDATE model_identifications SET
			conclusion = $2, closest_model = $3, match_level = $4, fit = $5, margin = $6,
			reference_version = $7, completed_at = $8, expires_at = $9,
			run_status = 'succeeded', run_progress = 3,
			run_finished_at = $8, run_error_code = NULL
		WHERE run_id = $1::uuid AND run_status = 'running'
		RETURNING `+modelIdentificationColumns,
		runID, result.Conclusion, result.ClosestModel, result.MatchLevel, result.Fit, result.Margin,
		result.ReferenceVersion, at, at.Add(ModelIdentificationValidity)))
	return item, s.modelIdentificationMutationError(ctx, runID, "complete model identification", err)
}

// FailModelIdentificationRun preserves an unexpired earlier verdict while
// recording the latest run failure with a normalized, non-sensitive code.
func (s *Store) FailModelIdentificationRun(ctx context.Context, runID, errorCode string) (ModelIdentification, error) {
	if !validModelIdentificationRunID(runID) ||
		!modelIdentificationCodePattern.MatchString(errorCode) {
		return ModelIdentification{}, fmt.Errorf("%w: invalid run or error code", ErrInvalid)
	}
	at := s.now().UTC()
	item, err := scanModelIdentification(s.db.QueryRowContext(ctx, `
		UPDATE model_identifications SET
			conclusion = CASE WHEN expires_at > $3 THEN conclusion ELSE NULL END,
			closest_model = CASE WHEN expires_at > $3 THEN closest_model ELSE NULL END,
			match_level = CASE WHEN expires_at > $3 THEN match_level ELSE NULL END,
			fit = CASE WHEN expires_at > $3 THEN fit ELSE NULL END,
			margin = CASE WHEN expires_at > $3 THEN margin ELSE NULL END,
			reference_version = CASE WHEN expires_at > $3 THEN reference_version ELSE NULL END,
			completed_at = CASE WHEN expires_at > $3 THEN completed_at ELSE NULL END,
			expires_at = CASE WHEN expires_at > $3 THEN expires_at ELSE NULL END,
			run_status = 'failed', run_finished_at = $3, run_error_code = $2
		WHERE run_id = $1::uuid AND run_status = 'running'
		RETURNING `+modelIdentificationColumns, runID, errorCode, at))
	return item, s.modelIdentificationMutationError(ctx, runID, "fail model identification", err)
}

// RecoverInterruptedModelIdentificationRuns closes runs older than the maximum
// runtime. A new replica must not mark another live replica's run as failed.
// Begin also calls this method so a stale slot is eventually reclaimed.
func (s *Store) RecoverInterruptedModelIdentificationRuns(ctx context.Context) (int64, error) {
	at := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE model_identifications SET run_status = 'failed',
			run_finished_at = $1, run_error_code = 'interrupted',
			conclusion = CASE WHEN expires_at > $1 THEN conclusion ELSE NULL END,
			closest_model = CASE WHEN expires_at > $1 THEN closest_model ELSE NULL END,
			match_level = CASE WHEN expires_at > $1 THEN match_level ELSE NULL END,
			fit = CASE WHEN expires_at > $1 THEN fit ELSE NULL END,
			margin = CASE WHEN expires_at > $1 THEN margin ELSE NULL END,
			reference_version = CASE WHEN expires_at > $1 THEN reference_version ELSE NULL END,
			completed_at = CASE WHEN expires_at > $1 THEN completed_at ELSE NULL END,
			expires_at = CASE WHEN expires_at > $1 THEN expires_at ELSE NULL END
		WHERE run_status = 'running' AND run_started_at < $2`,
		at, at.Add(-modelIdentificationRecoveryAfter))
	if err != nil {
		return 0, mapDBError("recover interrupted model identifications", err)
	}
	return result.RowsAffected()
}

// PurgeExpiredModelIdentifications removes verdict data after thirty days,
// retaining the last run status for the administrator's recent-run display.
func (s *Store) PurgeExpiredModelIdentifications(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE model_identifications SET conclusion = NULL, closest_model = NULL, match_level = NULL,
			fit = NULL, margin = NULL, reference_version = NULL,
			completed_at = NULL, expires_at = NULL
		WHERE expires_at <= $1`, s.now().UTC())
	if err != nil {
		return 0, mapDBError("purge expired model identifications", err)
	}
	return result.RowsAffected()
}

func (s *Store) ListModelIdentifications(ctx context.Context) ([]ModelIdentification, error) {
	if _, err := s.PurgeExpiredModelIdentifications(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+modelIdentificationColumns+`
		FROM model_identifications ORDER BY run_started_at DESC, account_id, requested_model`)
	if err != nil {
		return nil, mapDBError("list model identifications", err)
	}
	defer rows.Close()
	items := make([]ModelIdentification, 0)
	for rows.Next() {
		item, err := scanModelIdentification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan model identification: %w", err)
		}
		if item.ExpiresAt != nil && !s.now().UTC().Before(*item.ExpiresAt) {
			item.Conclusion = ""
			item.ClosestModel = ""
			item.MatchLevel = ""
			item.Fit = nil
			item.Margin = nil
			item.ReferenceVersion = ""
			item.CompletedAt = nil
			item.ExpiresAt = nil
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model identifications: %w", err)
	}
	return items, nil
}

func validModelIdentificationRunID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
				return false
			}
		}
	}
	return true
}

func (s *Store) modelIdentificationMutationError(ctx context.Context, runID, operation string, err error) error {
	if !errors.Is(err, sql.ErrNoRows) {
		return mapDBError(operation, err)
	}
	var exists bool
	if lookupErr := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM model_identifications WHERE run_id = $1::uuid)`, runID).Scan(&exists); lookupErr != nil {
		return mapDBError(operation, lookupErr)
	}
	if exists {
		return fmt.Errorf("%s: %w", operation, ErrConflict)
	}
	return fmt.Errorf("%s: %w", operation, ErrNotFound)
}
