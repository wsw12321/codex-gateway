package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type SetModelAccessDefaultsBatchParams struct {
	ModelAccessWriteParams
	Models  []string
	Enabled bool
}

type SetUserModelAccessBatchParams struct {
	ModelAccessWriteParams
	Models  []string
	Enabled bool
	Scope   string
	UserIDs []string
}

// ModelAccessBatchResult counts model/user pairs, or models for default changes.
type ModelAccessBatchResult struct {
	Results      []ModelAccessChangeResult `json:"results"`
	TargetCount  int64                     `json:"target_count"`
	ChangedCount int64                     `json:"changed_count"`
}

func normalizeModelAccessBatch(models []string) ([]string, error) {
	if len(models) == 0 || len(models) > 1000 {
		return nil, fmt.Errorf("%w: model access batch requires 1-1000 models", ErrInvalid)
	}
	normalized, err := normalizeModelAccessCatalog(models)
	if err != nil {
		return nil, err
	}
	if len(normalized) != len(models) {
		return nil, fmt.Errorf("%w: model access models must be unique", ErrInvalid)
	}
	return normalized, nil
}

// lockModelAccessBatch serializes catalog/default changes and validates every
// target before any write. Do not take FOR UPDATE row locks here: registration
// holds a users table lock and takes FK key-share locks on the model rows, while
// an all-users batch subsequently needs to lock the users table.
func lockModelAccessBatch(ctx context.Context, tx *sql.Tx, models []string) error {
	if _, err := tx.ExecContext(ctx, `LOCK TABLE model_access_defaults IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return mapDBError("lock model access batch catalog", err)
	}
	for _, model := range models {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT catalog_active FROM model_access_defaults
			WHERE model = $1 AND catalog_active`, model).Scan(&active); err != nil {
			return mapDBError("lock model access batch model", err)
		}
	}
	return nil
}

func (s *Store) SetModelAccessDefaultsBatch(ctx context.Context, params SetModelAccessDefaultsBatchParams) (ModelAccessBatchResult, error) {
	models, err := normalizeModelAccessBatch(params.Models)
	if err != nil {
		return ModelAccessBatchResult{}, err
	}
	write, err := normalizeModelAccessWrite(params.ModelAccessWriteParams, s.now)
	if err != nil {
		return ModelAccessBatchResult{}, err
	}
	result := ModelAccessBatchResult{Results: make([]ModelAccessChangeResult, 0, len(models))}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockModelAccessBatch(ctx, tx, models); err != nil {
			return err
		}
		for _, model := range models {
			change := ModelAccessChangeResult{Model: model, Enabled: params.Enabled, Scope: ModelAccessScopeDefault, TargetCount: 1}
			update, err := tx.ExecContext(ctx, `UPDATE model_access_defaults
				SET enabled = $2, updated_at = $3, updated_by_user_id = $4
				WHERE model = $1 AND enabled IS DISTINCT FROM $2`, model, params.Enabled, write.At, write.ActorUserID)
			if err != nil {
				return mapDBError("update model access batch default", err)
			}
			change.ChangedCount, err = update.RowsAffected()
			if err != nil {
				return fmt.Errorf("count changed model access defaults: %w", err)
			}
			if change.ChangedCount > 0 {
				if err := appendModelAccessAuditTx(ctx, tx, write, "model_access.default_changed", change); err != nil {
					return err
				}
			}
			result.Results = append(result.Results, change)
			result.TargetCount += change.TargetCount
			result.ChangedCount += change.ChangedCount
		}
		return nil
	})
	if err != nil {
		return ModelAccessBatchResult{}, err
	}
	return result, nil
}

func (s *Store) SetUserModelAccessBatch(ctx context.Context, params SetUserModelAccessBatchParams) (ModelAccessBatchResult, error) {
	models, err := normalizeModelAccessBatch(params.Models)
	if err != nil {
		return ModelAccessBatchResult{}, err
	}
	write, err := normalizeModelAccessWrite(params.ModelAccessWriteParams, s.now)
	if err != nil {
		return ModelAccessBatchResult{}, err
	}
	params.Scope = strings.TrimSpace(params.Scope)
	encodedUserIDs := "[]"
	switch params.Scope {
	case ModelAccessScopeAll:
		if len(params.UserIDs) != 0 {
			return ModelAccessBatchResult{}, fmt.Errorf("%w: all model access scope cannot include user ids", ErrInvalid)
		}
	case ModelAccessScopeSelected:
		params.UserIDs, err = normalizeSelectedModelAccessUsers(params.UserIDs)
		if err != nil {
			return ModelAccessBatchResult{}, err
		}
		encodedUserIDs, err = marshalStringArray(params.UserIDs)
		if err != nil {
			return ModelAccessBatchResult{}, err
		}
	default:
		return ModelAccessBatchResult{}, fmt.Errorf("%w: invalid model access scope", ErrInvalid)
	}
	result := ModelAccessBatchResult{Results: make([]ModelAccessChangeResult, 0, len(models))}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockModelAccessBatch(ctx, tx, models); err != nil {
			return err
		}
		var users int64
		if params.Scope == ModelAccessScopeAll {
			if _, err := tx.ExecContext(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
				return mapDBError("lock users for model access batch", err)
			}
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
				return mapDBError("count model access batch users", err)
			}
		} else {
			rows, err := tx.QueryContext(ctx, `SELECT id FROM users WHERE id IN
				(SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)) ORDER BY id FOR UPDATE`, encodedUserIDs)
			if err != nil {
				return mapDBError("lock selected model access batch users", err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan model access batch user: %w", err)
				}
				users++
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close model access batch users: %w", err)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate model access batch users: %w", err)
			}
			if users != int64(len(params.UserIDs)) {
				return fmt.Errorf("%w: selected model access user does not exist", ErrInvalid)
			}
		}
		for _, model := range models {
			var accessCount int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM user_model_access WHERE model = $1
				AND ($2 = 'all' OR user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($3::jsonb)))`,
				model, params.Scope, encodedUserIDs).Scan(&accessCount); err != nil {
				return mapDBError("count model access batch rows", err)
			}
			if accessCount != users {
				return fmt.Errorf("model access rows missing for %q: %w", model, ErrModelAccessUnavailable)
			}
			change := ModelAccessChangeResult{Model: model, Enabled: params.Enabled, Scope: params.Scope, TargetCount: users}
			update, err := tx.ExecContext(ctx, `UPDATE user_model_access
				SET enabled = $2, updated_at = $3, updated_by_user_id = $4
				WHERE model = $1 AND enabled IS DISTINCT FROM $2
				AND ($5 = 'all' OR user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($6::jsonb)))`,
				model, params.Enabled, write.At, write.ActorUserID, params.Scope, encodedUserIDs)
			if err != nil {
				return mapDBError("update model access batch users", err)
			}
			change.ChangedCount, err = update.RowsAffected()
			if err != nil {
				return fmt.Errorf("count changed model access batch users: %w", err)
			}
			if change.ChangedCount > 0 {
				if err := appendModelAccessAuditTx(ctx, tx, write, "model_access.users_changed", change); err != nil {
					return err
				}
			}
			result.Results = append(result.Results, change)
			result.TargetCount += users
			result.ChangedCount += change.ChangedCount
		}
		return nil
	})
	if err != nil {
		return ModelAccessBatchResult{}, err
	}
	return result, nil
}

// ListModelAccessUsersBatch reads the complete selection in one snapshot.
func (s *Store) ListModelAccessUsersBatch(ctx context.Context, requested []string) ([]ModelAccessUser, error) {
	models, err := normalizeModelAccessBatch(requested)
	if err != nil {
		return nil, err
	}
	encoded, err := marshalStringArray(models)
	if err != nil {
		return nil, err
	}
	result := make([]ModelAccessUser, 0)
	err = s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM model_access_defaults WHERE catalog_active
			AND model IN (SELECT value FROM jsonb_array_elements_text($1::jsonb))`, encoded).Scan(&count); err != nil {
			return mapDBError("validate model access batch query", err)
		}
		if count != len(models) {
			return fmt.Errorf("model access batch model not found: %w", ErrNotFound)
		}
		rows, err := tx.QueryContext(ctx, `SELECT d.model, u.id, u.username, u.display_name, u.role, u.status,
			a.enabled, a.updated_at, a.updated_by_user_id
			FROM users u CROSS JOIN model_access_defaults d
			LEFT JOIN user_model_access a ON a.user_id = u.id AND a.model = d.model
			WHERE d.model IN (SELECT value FROM jsonb_array_elements_text($1::jsonb))
			ORDER BY lower(u.username), u.id, d.model`, encoded)
		if err != nil {
			return mapDBError("list model access batch users", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value ModelAccessUser
			var enabled sql.NullBool
			var updatedAt sql.NullTime
			var updatedBy sql.NullString
			if err := rows.Scan(&value.Model, &value.UserID, &value.Username, &value.DisplayName,
				&value.Role, &value.Status, &enabled, &updatedAt, &updatedBy); err != nil {
				return fmt.Errorf("scan model access batch row: %w", err)
			}
			if !enabled.Valid || !updatedAt.Valid {
				return fmt.Errorf("model access batch row missing: %w", ErrModelAccessUnavailable)
			}
			value.Enabled, value.UpdatedAt, value.UpdatedByUserID = enabled.Bool, updatedAt.Time, nullableString(updatedBy)
			result = append(result, value)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
