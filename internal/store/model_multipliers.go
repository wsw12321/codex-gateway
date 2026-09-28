package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	decimal "github.com/wsw/codex-gateway/internal/billing"
	"github.com/wsw/codex-gateway/internal/config"
)

type ModelMultiplier struct {
	Model      string     `json:"model"`
	Multiplier string     `json:"multiplier"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

type SetModelMultiplierParams struct {
	BillingWriteParams
	Model      string
	Multiplier string
}

// ListModelMultipliers accepts the caller's current pricing catalog. Stored
// settings for models removed from that catalog remain available for readding.
func (s *Store) ListModelMultipliers(ctx context.Context, models []string) ([]ModelMultiplier, error) {
	values := make([]ModelMultiplier, 0, len(models))
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.model, CASE WHEN m.model = $2 THEN '1'
			ELSE COALESCE(s.multiplier::text, '1') END, s.updated_at
		FROM unnest($1::text[]) WITH ORDINALITY AS m(model, position)
		LEFT JOIN billing_model_multipliers s ON s.model = m.model
		ORDER BY m.position`, models, config.InternalGovernanceModel)
	if err != nil {
		return nil, mapDBError("list model multipliers", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value ModelMultiplier
		if err := rows.Scan(&value.Model, &value.Multiplier, &value.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model multiplier: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError("iterate model multipliers", err)
	}
	return values, nil
}

// Shared admission locks and exclusive setting locks also serialize the first
// setting for a model, when no settings row exists yet. The lock is held until
// the reservation snapshot commits, defining one ordering with admin saves.
func snapshotModelMultiplierTx(ctx context.Context, tx *sql.Tx, model string) (string, error) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(
		hashtextextended('billing.model_multiplier.' || $1, 0))`, model); err != nil {
		return "", mapDBError("lock model multiplier snapshot", err)
	}
	return readModelMultiplierTx(ctx, tx, model)
}

func readModelMultiplierTx(ctx context.Context, tx *sql.Tx, model string) (string, error) {
	var multiplier string
	err := tx.QueryRowContext(ctx, `SELECT multiplier::text FROM billing_model_multipliers
		WHERE model = $1`, model).Scan(&multiplier)
	if errors.Is(err, sql.ErrNoRows) {
		return "1", nil
	}
	if err != nil {
		return "", mapDBError("read model multiplier", err)
	}
	return multiplier, nil
}

func (s *Store) SetModelMultiplier(ctx context.Context, params SetModelMultiplierParams) (ModelMultiplier, error) {
	var setting ModelMultiplier
	if err := validateBillingWrite(params.BillingWriteParams); err != nil {
		return setting, err
	}
	params.Model = strings.TrimSpace(params.Model)
	if params.Model == "" || len([]rune(params.Model)) > 128 || params.Model == config.InternalGovernanceModel {
		return setting, fmt.Errorf("%w: invalid editable pricing model", ErrInvalid)
	}
	multiplier, err := decimal.ParseMultiplier(params.Multiplier)
	if err != nil {
		return setting, fmt.Errorf("%w: invalid model multiplier", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	fingerprint := billingOperationFingerprint("model_multiplier", params.ActorUserID,
		params.Reason, params.Model, multiplier)
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		created, replayID, err := claimBillingOperationTx(ctx, tx, params.BillingWriteParams,
			"model_multiplier", "", fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err := replayBillingLedgerTx(ctx, tx, *replayID)
			if err != nil {
				return err
			}
			if entry.Model == nil || *entry.Model != params.Model {
				return fmt.Errorf("replayed multiplier operation has no matching model: %w", ErrConflict)
			}
			setting = ModelMultiplier{Model: *entry.Model, Multiplier: entry.PricingMultiplier,
				UpdatedAt: &entry.CreatedAt}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(
			hashtextextended('billing.model_multiplier.' || $1, 0))`, params.Model); err != nil {
			return mapDBError("lock model multiplier setting", err)
		}
		previous, err := readModelMultiplierTx(ctx, tx, params.Model)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO billing_model_multipliers (model, multiplier, updated_at, updated_by_user_id)
			VALUES ($1, $2::numeric, $3, $4)
			ON CONFLICT (model) DO UPDATE SET multiplier = EXCLUDED.multiplier,
				updated_at = EXCLUDED.updated_at, updated_by_user_id = EXCLUDED.updated_by_user_id
			RETURNING model, multiplier::text, updated_at`,
			params.Model, multiplier, params.At, params.ActorUserID,
		).Scan(&setting.Model, &setting.Multiplier, &setting.UpdatedAt); err != nil {
			return mapDBError("set model multiplier", err)
		}
		entry, err := scanBillingLedgerEntry(tx.QueryRowContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id, operation_id, entry_type, model, pricing_multiplier, reason, actor_user_id, created_at)
			VALUES ($1, $2, 'model_multiplier', $3, $4::numeric, $5, $1, $6)
			RETURNING `+billingLedgerColumns,
			params.ActorUserID, params.OperationID, params.Model, multiplier, params.Reason, params.At))
		if err != nil {
			return mapDBError("record model multiplier ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, entry.ID); err != nil {
			return err
		}
		return appendBillingAuditTx(ctx, tx, params.BillingWriteParams,
			"billing.model_multiplier_updated", "billing_model_multiplier", params.Model, map[string]any{
				"operation_id": params.OperationID, "model": params.Model,
				"previous_multiplier": previous, "multiplier": multiplier, "reason": params.Reason,
			})
	})
	return setting, err
}
