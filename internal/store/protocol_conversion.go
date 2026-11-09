package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ProtocolConversionSettings is the Owner switch for protocol conversion the
// gateway exposes. ClaudeResponsesEnabled lets claude-* models use OpenAI
// Responses, which CPA translates to Claude Messages.
type ProtocolConversionSettings struct {
	ClaudeResponsesEnabled bool      `json:"claude_responses_enabled"`
	Revision               int64     `json:"revision"`
	UpdatedAt              time.Time `json:"updated_at"`
}

type SetProtocolConversionSettingsParams struct {
	ClaudeResponsesEnabled bool
	// ExpectedRevision must match the stored revision, so an Owner acting on a
	// stale page cannot silently undo another Owner's change.
	ExpectedRevision int64
	ActorUserID      string
	ActorSessionID   string
	RequestID        string
	SourceIP         string
	At               time.Time
}

func (s *Store) ProtocolConversionSettings(ctx context.Context) (ProtocolConversionSettings, error) {
	var settings ProtocolConversionSettings
	err := s.db.QueryRowContext(ctx, `SELECT claude_responses_enabled, revision, updated_at
		FROM protocol_conversion_settings WHERE singleton`).Scan(
		&settings.ClaudeResponsesEnabled, &settings.Revision, &settings.UpdatedAt)
	return settings, mapDBError("read protocol conversion settings", err)
}

func (s *Store) SetProtocolConversionSettings(ctx context.Context, params SetProtocolConversionSettingsParams) (ProtocolConversionSettings, error) {
	if params.ExpectedRevision < 1 || strings.TrimSpace(params.ActorUserID) == "" {
		return ProtocolConversionSettings{}, fmt.Errorf("%w: invalid protocol conversion settings", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	var settings ProtocolConversionSettings
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var previous bool
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT claude_responses_enabled, revision
			FROM protocol_conversion_settings WHERE singleton FOR UPDATE`).Scan(&previous, &revision); err != nil {
			return mapDBError("lock protocol conversion settings", err)
		}
		if revision != params.ExpectedRevision {
			return fmt.Errorf("%w: protocol conversion settings revision changed", ErrConflict)
		}
		if err := tx.QueryRowContext(ctx, `UPDATE protocol_conversion_settings
			SET claude_responses_enabled = $1, revision = revision + 1, updated_at = $2 WHERE singleton
			RETURNING claude_responses_enabled, revision, updated_at`, params.ClaudeResponsesEnabled, params.At).Scan(
			&settings.ClaudeResponsesEnabled, &settings.Revision, &settings.UpdatedAt); err != nil {
			return mapDBError("update protocol conversion settings", err)
		}
		metadata, err := marshalSafeMetadata(map[string]any{
			"previous_claude_responses_enabled": previous,
			"claude_responses_enabled":          settings.ClaudeResponsesEnabled,
			"revision":                          settings.Revision,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
				success, source_ip, subject_type, subject_id, request_id, metadata)
			VALUES ($1,$2,$3,'gateway.protocol_conversion_changed','warning',true,
				$4::inet,'gateway_setting','protocol_conversion',$5,$6::jsonb)`,
			params.At, params.ActorUserID, valueOrNil(params.ActorSessionID),
			valueOrNil(params.SourceIP), valueOrNil(params.RequestID), metadata)
		return mapDBError("audit protocol conversion settings", err)
	})
	if err != nil {
		return ProtocolConversionSettings{}, err
	}
	return settings, nil
}
