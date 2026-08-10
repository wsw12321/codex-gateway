package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type AppendAuditEventParams struct {
	OccurredAt     time.Time
	ActorUserID    string
	ActorSessionID string
	ActorAPIKeyID  string
	EventType      string
	Severity       string
	Success        bool
	SourceIP       string
	SubjectType    string
	SubjectID      string
	RequestID      string
	Metadata       map[string]any
}

const auditColumns = `id, occurred_at, actor_user_id, actor_session_id, actor_api_key_id,
	event_type, severity, success, host(source_ip), subject_type, subject_id,
	request_id, metadata::text`

func scanAuditEvent(row rowScanner) (AuditEvent, error) {
	var event AuditEvent
	var metadata []byte
	err := row.Scan(
		&event.ID, &event.OccurredAt, &event.ActorUserID, &event.ActorSessionID,
		&event.ActorAPIKeyID, &event.EventType, &event.Severity, &event.Success,
		&event.SourceIP, &event.SubjectType, &event.SubjectID, &event.RequestID, &metadata,
	)
	if err == nil {
		event.Metadata, err = decodeMetadata(metadata)
	}
	return event, err
}

func (s *Store) AppendAuditEvent(ctx context.Context, params AppendAuditEventParams) (AuditEvent, error) {
	params.EventType = strings.TrimSpace(params.EventType)
	if params.EventType == "" {
		return AuditEvent{}, fmt.Errorf("%w: audit event type is empty", ErrInvalid)
	}
	if params.Severity == "" {
		params.Severity = "info"
	}
	if params.OccurredAt.IsZero() {
		params.OccurredAt = s.now().UTC()
	}
	metadata, err := marshalSafeMetadata(params.Metadata)
	if err != nil {
		return AuditEvent{}, err
	}
	event, err := scanAuditEvent(s.db.QueryRowContext(ctx, `
		INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, actor_api_key_id,
			 event_type, severity, success, source_ip, subject_type, subject_id,
			 request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::inet, $9, $10, $11, $12::jsonb)
		RETURNING `+auditColumns,
		params.OccurredAt, valueOrNil(params.ActorUserID), valueOrNil(params.ActorSessionID),
		valueOrNil(params.ActorAPIKeyID), params.EventType, params.Severity, params.Success,
		valueOrNil(params.SourceIP), params.SubjectType, params.SubjectID,
		valueOrNil(params.RequestID), metadata,
	))
	return event, mapDBError("append audit event", err)
}

type AuditFilter struct {
	From        *time.Time
	Until       *time.Time
	ActorUserID string
	EventType   string
	Severity    string
	Success     *bool
	Limit       int
	Offset      int
}

func (s *Store) ListAuditEvents(ctx context.Context, filter AuditFilter) ([]AuditEvent, error) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 8)
	add := func(condition string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(condition, len(args)))
	}
	if filter.From != nil {
		add("occurred_at >= $%d", *filter.From)
	}
	if filter.Until != nil {
		add("occurred_at < $%d", *filter.Until)
	}
	if filter.ActorUserID != "" {
		add("actor_user_id = $%d", filter.ActorUserID)
	}
	if filter.EventType != "" {
		add("event_type = $%d", filter.EventType)
	}
	if filter.Severity != "" {
		add("severity = $%d", filter.Severity)
	}
	if filter.Success != nil {
		add("success = $%d", *filter.Success)
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	args = append(args, filter.Limit, filter.Offset)
	query := `SELECT ` + auditColumns + ` FROM audit_events WHERE ` +
		strings.Join(where, " AND ") + fmt.Sprintf(
		` ORDER BY occurred_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args),
	)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError("list audit events", err)
	}
	defer rows.Close()
	var result []AuditEvent
	for rows.Next() {
		event, err := scanAuditEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit events: %w", err)
	}
	return result, nil
}

type CreateAlertParams struct {
	OccurredAt time.Time
	Type       string
	Severity   string
	UserID     string
	RequestID  string
	DedupeKey  string
	Title      string
	Details    map[string]any
}

const alertColumns = `id, created_at, updated_at, alert_type, severity, status, user_id,
	request_id, dedupe_key, title, details::text, occurrence_count, last_occurred_at,
	acknowledged_at, acknowledged_by, resolved_at`

func scanAlert(row rowScanner) (Alert, error) {
	var alert Alert
	var details []byte
	err := row.Scan(
		&alert.ID, &alert.CreatedAt, &alert.UpdatedAt, &alert.Type, &alert.Severity,
		&alert.Status, &alert.UserID, &alert.RequestID, &alert.DedupeKey, &alert.Title,
		&details, &alert.OccurrenceCount, &alert.LastOccurredAt, &alert.AcknowledgedAt,
		&alert.AcknowledgedBy, &alert.ResolvedAt,
	)
	if err == nil {
		alert.Details, err = decodeMetadata(details)
	}
	return alert, err
}

// CreateAlert coalesces an unresolved dedupe key and reopens an acknowledged
// alert when the condition happens again.
func (s *Store) CreateAlert(ctx context.Context, params CreateAlertParams) (Alert, error) {
	params.Type = strings.TrimSpace(params.Type)
	params.Title = strings.TrimSpace(params.Title)
	if params.Type == "" || params.Title == "" || params.Severity == "" {
		return Alert{}, fmt.Errorf("%w: alert type, severity and title are required", ErrInvalid)
	}
	if params.OccurredAt.IsZero() {
		params.OccurredAt = s.now().UTC()
	}
	details, err := marshalSafeMetadata(params.Details)
	if err != nil {
		return Alert{}, err
	}
	alert, err := scanAlert(s.db.QueryRowContext(ctx, `
		INSERT INTO alerts
			(created_at, updated_at, alert_type, severity, user_id, request_id,
			 dedupe_key, title, details, last_occurred_at)
		VALUES ($1, $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $1)
		ON CONFLICT (dedupe_key)
			WHERE status <> 'resolved' AND dedupe_key IS NOT NULL
		DO UPDATE SET
			severity = CASE
				WHEN alerts.severity = 'critical' OR EXCLUDED.severity = 'critical' THEN 'critical'
				WHEN alerts.severity = 'warning' OR EXCLUDED.severity = 'warning' THEN 'warning'
				ELSE 'info' END,
			status = 'open', acknowledged_at = NULL, acknowledged_by = NULL,
			title = EXCLUDED.title, details = EXCLUDED.details,
			occurrence_count = alerts.occurrence_count + 1,
			last_occurred_at = EXCLUDED.last_occurred_at
		RETURNING `+alertColumns,
		params.OccurredAt, params.Type, params.Severity, valueOrNil(params.UserID),
		valueOrNil(params.RequestID), valueOrNil(params.DedupeKey), params.Title, details,
	))
	return alert, mapDBError("create alert", err)
}

func (s *Store) ListAlerts(ctx context.Context, status string, limit, offset int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	query := `SELECT ` + alertColumns + ` FROM alerts WHERE ($1 = '' OR status = $1)
		ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
			last_occurred_at DESC LIMIT $2 OFFSET $3`
	rows, err := s.db.QueryContext(ctx, query, status, limit, offset)
	if err != nil {
		return nil, mapDBError("list alerts", err)
	}
	defer rows.Close()
	var result []Alert
	for rows.Next() {
		alert, err := scanAlert(rows)
		if err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		result = append(result, alert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate alerts: %w", err)
	}
	return result, nil
}

func (s *Store) AcknowledgeAlert(ctx context.Context, alertID int64, userID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE alerts
		SET status = 'acknowledged', acknowledged_at = $3, acknowledged_by = $2
		WHERE id = $1 AND status = 'open'`, alertID, userID, at,
	)
	if err != nil {
		return mapDBError("acknowledge alert", err)
	}
	return requireAffected("acknowledge alert", result)
}

func (s *Store) ResolveAlert(ctx context.Context, alertID int64, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE alerts SET status = 'resolved', resolved_at = $2
		WHERE id = $1 AND status <> 'resolved'`, alertID, at,
	)
	if err != nil {
		return mapDBError("resolve alert", err)
	}
	return requireAffected("resolve alert", result)
}

func (s *Store) DeleteAuditEventsBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 100_000 {
		limit = 10_000
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM audit_events WHERE id IN (
			SELECT id FROM audit_events WHERE occurred_at < $1
			ORDER BY occurred_at LIMIT $2
		)`, before, limit,
	)
	if err != nil {
		return 0, mapDBError("delete old audit events", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete old audit events rows affected: %w", err)
	}
	return n, nil
}
