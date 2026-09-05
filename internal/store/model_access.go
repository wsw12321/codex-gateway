package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ModelAccessScopeDefault  = "default"
	ModelAccessScopeAll      = "all"
	ModelAccessScopeSelected = "selected"
)

// ModelAccessModel is the Owner-facing summary for one currently manageable
// model. Counts include active, disabled, member, and Owner accounts.
type ModelAccessModel struct {
	Model             string    `json:"model"`
	DefaultEnabled    bool      `json:"default_enabled"`
	EnabledUserCount  int64     `json:"enabled_user_count"`
	DisabledUserCount int64     `json:"disabled_user_count"`
	UpdatedAt         time.Time `json:"updated_at"`
	UpdatedByUserID   *string   `json:"updated_by_user_id,omitempty"`
}

// ModelAccessUser is one user's authorization state for a manageable model.
type ModelAccessUser struct {
	Model           string    `json:"model"`
	UserID          string    `json:"user_id"`
	Username        string    `json:"username"`
	DisplayName     string    `json:"display_name"`
	Role            string    `json:"role"`
	Status          string    `json:"status"`
	Enabled         bool      `json:"enabled"`
	UpdatedAt       time.Time `json:"updated_at"`
	UpdatedByUserID *string   `json:"updated_by_user_id,omitempty"`
}

// ModelAccessWriteParams carries the audit attribution required for Owner
// changes. Authorization of the actor remains an HTTP/service-layer concern.
type ModelAccessWriteParams struct {
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	Reason         string
	At             time.Time
}

type SetModelAccessDefaultParams struct {
	ModelAccessWriteParams
	Model   string
	Enabled bool
}

type SetUserModelAccessParams struct {
	ModelAccessWriteParams
	Model   string
	Enabled bool
	Scope   string
	UserIDs []string
}

type ModelAccessChangeResult struct {
	Model        string `json:"model"`
	Enabled      bool   `json:"enabled"`
	Scope        string `json:"scope"`
	TargetCount  int64  `json:"target_count"`
	ChangedCount int64  `json:"changed_count"`
}

func validModelAccessModel(model string) bool {
	if len(model) == 0 || len(model) > 128 {
		return false
	}
	for index := 0; index < len(model); index++ {
		value := model[index]
		if !asciiAlphaNumeric(value) && value != '.' && value != '_' && value != ':' && value != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func normalizeModelAccessModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if !validModelAccessModel(model) {
		return "", fmt.Errorf("%w: invalid model access model", ErrInvalid)
	}
	return model, nil
}

func normalizeModelAccessCatalog(models []string) ([]string, error) {
	unique := make(map[string]struct{}, len(models))
	for _, model := range models {
		normalized, err := normalizeModelAccessModel(model)
		if err != nil {
			return nil, err
		}
		unique[normalized] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for model := range unique {
		result = append(result, model)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeModelAccessWrite(params ModelAccessWriteParams, now func() time.Time) (ModelAccessWriteParams, error) {
	params.ActorUserID = strings.TrimSpace(params.ActorUserID)
	params.ActorSessionID = strings.TrimSpace(params.ActorSessionID)
	params.RequestID = strings.TrimSpace(params.RequestID)
	params.SourceIP = strings.TrimSpace(params.SourceIP)
	params.Reason = strings.TrimSpace(params.Reason)
	if params.ActorUserID == "" || params.Reason == "" || len([]rune(params.Reason)) > 500 {
		return params, fmt.Errorf("%w: model access actor and 1-500 character reason are required", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	return params, nil
}

// SyncModelAccessCatalog makes models the authoritative manageable catalog.
// Removed models are retained as inactive history. Existing defaults and user
// choices are never overwritten; only missing snapshots are filled. Locking
// users closes the race with the registration trigger.
func (s *Store) SyncModelAccessCatalog(ctx context.Context, models []string) error {
	models, err := normalizeModelAccessCatalog(models)
	if err != nil {
		return err
	}
	encoded, err := marshalStringArray(models)
	if err != nil {
		return err
	}
	at := s.now().UTC()
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE model_access_defaults IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock model access catalog", err)
		}
		if _, err := tx.ExecContext(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock users for model access catalog", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE model_access_defaults d SET catalog_active = false
			WHERE d.catalog_active AND NOT EXISTS (
				SELECT 1 FROM jsonb_array_elements_text($1::jsonb) requested(model)
				WHERE requested.model = d.model
			)`, encoded); err != nil {
			return mapDBError("deactivate removed model access defaults", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_access_defaults
				(model, enabled, catalog_active, created_at, updated_at)
			SELECT requested.model, true, true, $2, $2
			FROM jsonb_array_elements_text($1::jsonb) requested(model)
			ON CONFLICT (model) DO UPDATE SET catalog_active = true`, encoded, at); err != nil {
			return mapDBError("synchronize model access defaults", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_model_access
				(user_id, model, enabled, created_at, updated_at)
			SELECT u.id, d.model, d.enabled, $1, $1
			FROM users u
			CROSS JOIN model_access_defaults d
			WHERE d.catalog_active
			ON CONFLICT (user_id, model) DO NOTHING`, at); err != nil {
			return mapDBError("backfill user model access", err)
		}
		return nil
	})
}

// ListModelAccessModels returns only models in the current synchronized
// catalog. A missing per-user row is corruption and fails closed.
func (s *Store) ListModelAccessModels(ctx context.Context) ([]ModelAccessModel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.model, d.enabled, d.updated_at, d.updated_by_user_id,
			count(a.user_id) FILTER (WHERE a.enabled),
			count(a.user_id) FILTER (WHERE NOT a.enabled),
			(SELECT count(*) FROM users)
		FROM model_access_defaults d
		LEFT JOIN user_model_access a ON a.model = d.model
		WHERE d.catalog_active
		GROUP BY d.model, d.enabled, d.updated_at, d.updated_by_user_id
		ORDER BY d.model`)
	if err != nil {
		return nil, mapDBError("list model access models", err)
	}
	defer rows.Close()
	result := make([]ModelAccessModel, 0)
	for rows.Next() {
		var value ModelAccessModel
		var updatedBy sql.NullString
		var totalUsers int64
		if err := rows.Scan(&value.Model, &value.DefaultEnabled, &value.UpdatedAt, &updatedBy,
			&value.EnabledUserCount, &value.DisabledUserCount, &totalUsers); err != nil {
			return nil, fmt.Errorf("scan model access model: %w", err)
		}
		if value.EnabledUserCount+value.DisabledUserCount != totalUsers {
			return nil, fmt.Errorf("model access rows missing for %q: %w", value.Model, ErrModelAccessUnavailable)
		}
		value.UpdatedByUserID = nullableString(updatedBy)
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model access models: %w", err)
	}
	return result, nil
}

// ListModelAccessUsers returns all current users, including disabled accounts,
// for one active catalog model.
func (s *Store) ListModelAccessUsers(ctx context.Context, model string) ([]ModelAccessUser, error) {
	model, err := normalizeModelAccessModel(model)
	if err != nil {
		return nil, err
	}
	result := make([]ModelAccessUser, 0)
	err = s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM model_access_defaults WHERE model = $1 AND catalog_active
		)`, model).Scan(&exists); err != nil {
			return mapDBError("find model access model", err)
		}
		if !exists {
			return fmt.Errorf("list model access users: %w", ErrNotFound)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT u.id, u.username, u.display_name, u.role, u.status,
				a.enabled, a.updated_at, a.updated_by_user_id
			FROM users u
			LEFT JOIN user_model_access a ON a.user_id = u.id AND a.model = $1
			ORDER BY lower(u.username), u.id`, model)
		if err != nil {
			return mapDBError("list model access users", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value ModelAccessUser
			var enabled sql.NullBool
			var updatedAt sql.NullTime
			var updatedBy sql.NullString
			if err := rows.Scan(&value.UserID, &value.Username, &value.DisplayName,
				&value.Role, &value.Status, &enabled, &updatedAt, &updatedBy); err != nil {
				return fmt.Errorf("scan model access user: %w", err)
			}
			if !enabled.Valid || !updatedAt.Valid {
				return fmt.Errorf("model access row missing for user %q and model %q: %w",
					value.UserID, model, ErrModelAccessUnavailable)
			}
			value.Model = model
			value.Enabled = enabled.Bool
			value.UpdatedAt = updatedAt.Time
			value.UpdatedByUserID = nullableString(updatedBy)
			result = append(result, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate model access users: %w", err)
		}
		return nil
	})
	return result, err
}

// ListEnabledModelsForUser returns the effective user side of the model
// intersection used by /v1/models. Any missing active-catalog row is an error,
// rather than silently hiding potentially corrupted authorization state.
func (s *Store) ListEnabledModelsForUser(ctx context.Context, userID string) ([]string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("%w: model access user is required", ErrInvalid)
	}
	result := make([]string, 0)
	err := s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			return mapDBError("find model access user", err)
		}
		if !exists {
			return fmt.Errorf("list enabled models for user: %w", ErrNotFound)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT d.model, a.enabled
			FROM model_access_defaults d
			LEFT JOIN user_model_access a ON a.model = d.model AND a.user_id = $1
			WHERE d.catalog_active
			ORDER BY d.model`, userID)
		if err != nil {
			return mapDBError("list enabled models for user", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			var enabled sql.NullBool
			if err := rows.Scan(&model, &enabled); err != nil {
				return fmt.Errorf("scan enabled model for user: %w", err)
			}
			if !enabled.Valid {
				return fmt.Errorf("model access row missing for user %q and model %q: %w",
					userID, model, ErrModelAccessUnavailable)
			}
			if enabled.Bool {
				result = append(result, model)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate enabled models for user: %w", err)
		}
		return nil
	})
	return result, err
}

// RequireModelAccess reports ErrModelNotAllowed only for an explicit disabled
// authorization. Missing catalog/default/user state is distinguished as
// ErrModelAccessUnavailable so callers fail closed with an internal error.
func (s *Store) RequireModelAccess(ctx context.Context, userID, model string) error {
	model, err := normalizeModelAccessModel(model)
	if err != nil {
		return err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("%w: model access user is required", ErrInvalid)
	}
	return requireModelAccess(ctx, s.db, userID, model)
}

func requireModelAccess(ctx context.Context, queryer queryRower, userID, model string) error {
	var enabled sql.NullBool
	err := queryer.QueryRowContext(ctx, `
		SELECT a.enabled
		FROM model_access_defaults d
		LEFT JOIN user_model_access a ON a.model = d.model AND a.user_id = $1
		WHERE d.model = $2 AND d.catalog_active`, userID, model).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !enabled.Valid {
		return fmt.Errorf("model access state missing for user %q and model %q: %w",
			userID, model, ErrModelAccessUnavailable)
	}
	if err != nil {
		return fmt.Errorf("read model access for user %q and model %q: %w", userID, model, err)
	}
	if !enabled.Bool {
		return fmt.Errorf("user %q cannot use model %q: %w", userID, model, ErrModelNotAllowed)
	}
	return nil
}

func (s *Store) SetModelAccessDefault(ctx context.Context, params SetModelAccessDefaultParams) (ModelAccessChangeResult, error) {
	model, err := normalizeModelAccessModel(params.Model)
	if err != nil {
		return ModelAccessChangeResult{}, err
	}
	write, err := normalizeModelAccessWrite(params.ModelAccessWriteParams, s.now)
	if err != nil {
		return ModelAccessChangeResult{}, err
	}
	result := ModelAccessChangeResult{
		Model: model, Enabled: params.Enabled, Scope: ModelAccessScopeDefault, TargetCount: 1,
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var current bool
		if err := tx.QueryRowContext(ctx, `SELECT enabled FROM model_access_defaults
			WHERE model = $1 AND catalog_active FOR UPDATE`, model).Scan(&current); err != nil {
			return mapDBError("lock model access default", err)
		}
		if current == params.Enabled {
			return nil
		}
		update, err := tx.ExecContext(ctx, `UPDATE model_access_defaults
			SET enabled = $2, updated_at = $3, updated_by_user_id = $4
			WHERE model = $1 AND catalog_active`, model, params.Enabled, write.At, write.ActorUserID)
		if err != nil {
			return mapDBError("update model access default", err)
		}
		if err := requireAffected("update model access default", update); err != nil {
			return err
		}
		result.ChangedCount = 1
		return appendModelAccessAuditTx(ctx, tx, write, "model_access.default_changed", result)
	})
	return result, err
}

func normalizeSelectedModelAccessUsers(userIDs []string) ([]string, error) {
	if len(userIDs) == 0 || len(userIDs) > 5000 {
		return nil, fmt.Errorf("%w: selected model access scope requires 1-5000 users", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(userIDs))
	result := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		userID = strings.TrimSpace(userID)
		parsed, err := uuid.Parse(userID)
		if err != nil || parsed.String() != userID {
			return nil, fmt.Errorf("%w: selected model access user id is invalid", ErrInvalid)
		}
		if _, duplicate := seen[userID]; duplicate {
			return nil, fmt.Errorf("%w: selected model access users must be unique", ErrInvalid)
		}
		seen[userID] = struct{}{}
		result = append(result, userID)
	}
	return result, nil
}

func (s *Store) SetUserModelAccess(ctx context.Context, params SetUserModelAccessParams) (ModelAccessChangeResult, error) {
	model, err := normalizeModelAccessModel(params.Model)
	if err != nil {
		return ModelAccessChangeResult{}, err
	}
	write, err := normalizeModelAccessWrite(params.ModelAccessWriteParams, s.now)
	if err != nil {
		return ModelAccessChangeResult{}, err
	}
	params.Scope = strings.TrimSpace(params.Scope)
	var encodedUserIDs string
	switch params.Scope {
	case ModelAccessScopeAll:
		if len(params.UserIDs) != 0 {
			return ModelAccessChangeResult{}, fmt.Errorf("%w: all model access scope cannot include user ids", ErrInvalid)
		}
	case ModelAccessScopeSelected:
		params.UserIDs, err = normalizeSelectedModelAccessUsers(params.UserIDs)
		if err != nil {
			return ModelAccessChangeResult{}, err
		}
		encodedUserIDs, err = marshalStringArray(params.UserIDs)
		if err != nil {
			return ModelAccessChangeResult{}, err
		}
	default:
		return ModelAccessChangeResult{}, fmt.Errorf("%w: invalid model access scope", ErrInvalid)
	}
	result := ModelAccessChangeResult{Model: model, Enabled: params.Enabled, Scope: params.Scope}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT catalog_active FROM model_access_defaults
			WHERE model = $1 AND catalog_active FOR UPDATE`, model).Scan(&active); err != nil {
			return mapDBError("lock model access model", err)
		}
		if params.Scope == ModelAccessScopeAll {
			if _, err := tx.ExecContext(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
				return mapDBError("lock users for model access update", err)
			}
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&result.TargetCount); err != nil {
				return mapDBError("count model access users", err)
			}
			var accessCount int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM user_model_access WHERE model = $1`, model).Scan(&accessCount); err != nil {
				return mapDBError("count user model access rows", err)
			}
			if accessCount != result.TargetCount {
				return fmt.Errorf("model access rows missing for %q: %w", model, ErrModelAccessUnavailable)
			}
			update, err := tx.ExecContext(ctx, `UPDATE user_model_access
				SET enabled = $2, updated_at = $3, updated_by_user_id = $4
				WHERE model = $1 AND enabled IS DISTINCT FROM $2`,
				model, params.Enabled, write.At, write.ActorUserID)
			if err != nil {
				return mapDBError("update all user model access", err)
			}
			result.ChangedCount, err = update.RowsAffected()
			if err != nil {
				return fmt.Errorf("update all user model access rows affected: %w", err)
			}
		} else {
			rows, err := tx.QueryContext(ctx, `
				SELECT u.id FROM users u
				WHERE u.id IN (
					SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)
				) ORDER BY u.id FOR UPDATE`, encodedUserIDs)
			if err != nil {
				return mapDBError("lock selected model access users", err)
			}
			for rows.Next() {
				var userID string
				if err := rows.Scan(&userID); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan selected model access user: %w", err)
				}
				result.TargetCount++
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close selected model access users: %w", err)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate selected model access users: %w", err)
			}
			if result.TargetCount != int64(len(params.UserIDs)) {
				return fmt.Errorf("selected model access user does not exist: %w", ErrInvalid)
			}
			var accessCount int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM user_model_access
				WHERE model = $1 AND user_id IN (
					SELECT value::uuid FROM jsonb_array_elements_text($2::jsonb)
				)`, model, encodedUserIDs).Scan(&accessCount); err != nil {
				return mapDBError("count selected user model access rows", err)
			}
			if accessCount != result.TargetCount {
				return fmt.Errorf("selected model access row is missing: %w", ErrModelAccessUnavailable)
			}
			update, err := tx.ExecContext(ctx, `UPDATE user_model_access
				SET enabled = $2, updated_at = $3, updated_by_user_id = $4
				WHERE model = $1 AND enabled IS DISTINCT FROM $2
				AND user_id IN (
					SELECT value::uuid FROM jsonb_array_elements_text($5::jsonb)
				)`, model, params.Enabled, write.At, write.ActorUserID, encodedUserIDs)
			if err != nil {
				return mapDBError("update selected user model access", err)
			}
			result.ChangedCount, err = update.RowsAffected()
			if err != nil {
				return fmt.Errorf("update selected user model access rows affected: %w", err)
			}
		}
		if result.ChangedCount == 0 {
			return nil
		}
		return appendModelAccessAuditTx(ctx, tx, write, "model_access.users_changed", result)
	})
	return result, err
}

func appendModelAccessAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	write ModelAccessWriteParams,
	eventType string,
	result ModelAccessChangeResult,
) error {
	metadata, err := marshalSafeMetadata(map[string]any{
		"reason": write.Reason, "enabled": result.Enabled, "scope": result.Scope,
		"target_count": result.TargetCount, "changed_count": result.ChangedCount,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
			 success, source_ip, subject_type, subject_id, request_id, metadata)
		VALUES ($1,$2,$3,$4,'info',true,$5::inet,'model',$6,$7,$8::jsonb)`,
		write.At, write.ActorUserID, valueOrNil(write.ActorSessionID), eventType,
		valueOrNil(write.SourceIP), result.Model, valueOrNil(write.RequestID), metadata)
	return mapDBError("append model access audit event", err)
}
