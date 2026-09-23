package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MonitoringRequest is the intentionally small, owner-facing view of a
// request used by the request monitor.  Unlike UsageRequest it does not carry
// API-key, device, token, or pricing metadata: the monitor only needs enough
// information to identify a request and its current outcome.
//
// UpstreamAccountID is nullable for requests that have not received an
// upstream account attribution yet.  UpstreamMaskedEmail is joined from the
// durable upstream_accounts table and is likewise nullable when an account is
// unknown or its metadata has not been synchronized.
type MonitoringRequest struct {
	RequestID           string
	ConversationHash    *string
	RequestedAt         time.Time
	CompletedAt         *time.Time
	UserID              string
	Username            string
	DisplayName         string
	RequestedModel      *string
	Model               string
	State               string
	HTTPStatus          *int
	ErrorCode           *string
	UpstreamAccountID   *string
	UpstreamMaskedEmail *string
}

// MonitoringSnapshot contains the three independent windows exposed by the
// owner request monitor.  The sampled timestamp is obtained from the same
// database statement that computes all window boundaries, so a refresh cannot
// mix rows selected against different notions of "now".
type MonitoringSnapshot struct {
	SampledAt  time.Time
	InProgress []MonitoringRequest
	Recent     []MonitoringRequest
	Failures   []MonitoringRequest
}

// Monitoring returns the current owner request-monitor snapshot.  It is kept
// as a short alias so callers that do not need to supply a deterministic clock
// (for example the HTTP handler) can use the concise form.
func (s *Store) Monitoring(ctx context.Context) (MonitoringSnapshot, error) {
	return s.ListMonitoringRequests(ctx)
}

// ListMonitoringRequests returns the owner request-monitor snapshot.  At most
// 500 rows are returned for each window, ordered newest-first.  An optional
// timestamp is accepted for deterministic tests and callers that already have
// a trusted sample time; when omitted, PostgreSQL computes the timestamp in
// the query itself.  In either form every window uses one shared boundary.
func (s *Store) ListMonitoringRequests(ctx context.Context, sampled ...time.Time) (MonitoringSnapshot, error) {
	if s == nil || s.db == nil {
		return MonitoringSnapshot{}, fmt.Errorf("list monitoring requests: %w", ErrInvalid)
	}

	var boundary any
	if len(sampled) > 0 && !sampled[0].IsZero() {
		boundary = sampled[0].UTC()
	}
	// The CTEs deliberately keep their own ORDER/LIMIT before the UNION.  The
	// outer ordering then restores a deterministic order for each independent
	// bucket while retaining one shared database timestamp from bounds.
	query := `
WITH bounds AS (
		SELECT COALESCE($1::timestamptz, CURRENT_TIMESTAMP) AS sampled_at
	),
	in_progress AS (
		SELECT
			1::smallint AS bucket_order,
			'in_progress'::text AS bucket,
			u.request_id::text,
			u.conversation_hash,
			u.requested_at,
			u.completed_at,
			u.user_id::text,
			usr.username,
			usr.display_name,
			u.requested_model,
			u.model,
			u.state,
			u.http_status,
			u.error_code,
			u.upstream_account_id,
			a.masked_email,
			b.sampled_at,
			u.requested_at AS sort_at,
			u.id::bigint AS request_sort_id
		FROM usage_requests u
		JOIN users usr ON usr.id = u.user_id
		LEFT JOIN upstream_accounts a ON a.id = u.upstream_account_id
		CROSS JOIN bounds b
		WHERE u.state = 'in_progress'
		ORDER BY u.requested_at DESC, u.id DESC
		LIMIT 500
	),
	recent AS (
		SELECT
			2::smallint AS bucket_order,
			'recent'::text AS bucket,
			u.request_id::text,
			u.conversation_hash,
			u.requested_at,
			u.completed_at,
			u.user_id::text,
			usr.username,
			usr.display_name,
			u.requested_model,
			u.model,
			u.state,
			u.http_status,
			u.error_code,
			u.upstream_account_id,
			a.masked_email,
			b.sampled_at,
			u.requested_at AS sort_at,
			u.id::bigint AS request_sort_id
		FROM usage_requests u
		JOIN users usr ON usr.id = u.user_id
		LEFT JOIN upstream_accounts a ON a.id = u.upstream_account_id
		CROSS JOIN bounds b
		WHERE u.requested_at >= b.sampled_at - INTERVAL '2 minutes'
		  AND u.requested_at <= b.sampled_at
		ORDER BY u.requested_at DESC, u.id DESC
		LIMIT 500
	),
	failures AS (
		SELECT
			3::smallint AS bucket_order,
			'failures'::text AS bucket,
			u.request_id::text,
			u.conversation_hash,
			u.requested_at,
			u.completed_at,
			u.user_id::text,
			usr.username,
			usr.display_name,
			u.requested_model,
			u.model,
			u.state,
			u.http_status,
			u.error_code,
			u.upstream_account_id,
			a.masked_email,
			b.sampled_at,
			u.completed_at AS sort_at,
			u.id::bigint AS request_sort_id
		FROM usage_requests u
		JOIN users usr ON usr.id = u.user_id
		LEFT JOIN upstream_accounts a ON a.id = u.upstream_account_id
		CROSS JOIN bounds b
		WHERE u.state = 'failed'
		  AND u.completed_at >= b.sampled_at - INTERVAL '1 hour'
		  AND u.completed_at <= b.sampled_at
		ORDER BY u.completed_at DESC, u.id DESC
		LIMIT 500
	),
	bucket_rows AS (
		SELECT * FROM in_progress
		UNION ALL SELECT * FROM recent
		UNION ALL SELECT * FROM failures
	),
	marker AS (
		SELECT
			0::smallint AS bucket_order,
			'__sampled__'::text AS bucket,
			NULL::text AS request_id,
			NULL::text AS conversation_hash,
			NULL::timestamptz AS requested_at,
			NULL::timestamptz AS completed_at,
			NULL::text AS user_id,
			NULL::text AS username,
			NULL::text AS display_name,
			NULL::text AS requested_model,
			NULL::text AS model,
			NULL::text AS state,
			NULL::smallint AS http_status,
			NULL::text AS error_code,
			NULL::text AS upstream_account_id,
			NULL::text AS masked_email,
			b.sampled_at,
			NULL::timestamptz AS sort_at,
			NULL::bigint AS request_sort_id
		FROM bounds b
	)
	SELECT bucket_order, bucket, request_id, conversation_hash, requested_at, completed_at,
	       user_id, username, display_name, requested_model, model, state, http_status,
	       error_code, upstream_account_id, masked_email, sampled_at, sort_at,
	       request_sort_id
FROM (
	SELECT * FROM marker
	UNION ALL
	SELECT * FROM bucket_rows
) all_rows
	ORDER BY bucket_order, sort_at DESC NULLS LAST, request_sort_id DESC NULLS LAST`

	rows, err := s.db.QueryContext(ctx, query, boundary)
	if err != nil {
		return MonitoringSnapshot{}, mapDBError("list monitoring requests", err)
	}
	defer rows.Close()

	result := MonitoringSnapshot{
		InProgress: make([]MonitoringRequest, 0),
		Recent:     make([]MonitoringRequest, 0),
		Failures:   make([]MonitoringRequest, 0),
	}
	for rows.Next() {
		var (
			bucketOrder       int16
			bucket            string
			requestID         sql.NullString
			conversationHash  sql.NullString
			requestedAt       sql.NullTime
			completedAt       sql.NullTime
			userID            sql.NullString
			username          sql.NullString
			displayName       sql.NullString
			requestedModel    sql.NullString
			model             sql.NullString
			state             sql.NullString
			httpStatus        sql.NullInt64
			errorCode         sql.NullString
			upstreamAccountID sql.NullString
			maskedEmail       sql.NullString
			sampledAt         time.Time
			sortAt            sql.NullTime
			requestSortID     sql.NullInt64
		)
		if err := rows.Scan(
			&bucketOrder, &bucket, &requestID, &conversationHash, &requestedAt, &completedAt,
			&userID, &username, &displayName, &requestedModel, &model, &state, &httpStatus,
			&errorCode, &upstreamAccountID, &maskedEmail, &sampledAt, &sortAt, &requestSortID,
		); err != nil {
			return MonitoringSnapshot{}, fmt.Errorf("scan monitoring request: %w", err)
		}
		if bucket == "__sampled__" {
			result.SampledAt = sampledAt.UTC()
			continue
		}
		if !requestID.Valid || !requestedAt.Valid || !userID.Valid || !username.Valid ||
			!displayName.Valid || !model.Valid || !state.Valid {
			return MonitoringSnapshot{}, fmt.Errorf("scan monitoring request: %w", ErrInvalid)
		}
		request := MonitoringRequest{
			RequestID: requestID.String, RequestedAt: requestedAt.Time.UTC(), UserID: userID.String,
			Username: username.String, DisplayName: displayName.String,
			RequestedModel: nullableString(requestedModel), Model: model.String,
			State: state.String,
		}
		if conversationHash.Valid && strings.TrimSpace(conversationHash.String) != "" {
			value := conversationHash.String
			request.ConversationHash = &value
		}
		if completedAt.Valid {
			value := completedAt.Time.UTC()
			request.CompletedAt = &value
		}
		if httpStatus.Valid {
			value := int(httpStatus.Int64)
			request.HTTPStatus = &value
		}
		if errorCode.Valid {
			value := errorCode.String
			request.ErrorCode = &value
		}
		if upstreamAccountID.Valid && strings.TrimSpace(upstreamAccountID.String) != "" {
			value := upstreamAccountID.String
			request.UpstreamAccountID = &value
		}
		if maskedEmail.Valid && strings.TrimSpace(maskedEmail.String) != "" {
			value := maskedEmail.String
			request.UpstreamMaskedEmail = &value
		}
		switch bucket {
		case "in_progress":
			result.InProgress = append(result.InProgress, request)
		case "recent":
			result.Recent = append(result.Recent, request)
		case "failures":
			result.Failures = append(result.Failures, request)
		default:
			return MonitoringSnapshot{}, fmt.Errorf("scan monitoring request: %w", ErrInvalid)
		}
	}
	if err := rows.Err(); err != nil {
		return MonitoringSnapshot{}, fmt.Errorf("iterate monitoring requests: %w", err)
	}
	if result.SampledAt.IsZero() {
		// The marker is part of the fixed query above.  Keep a defensive fallback
		// for unusual database drivers that suppress an all-null UNION branch.
		if s.now != nil {
			result.SampledAt = s.now().UTC()
		} else {
			result.SampledAt = time.Now().UTC()
		}
	}
	return result, nil
}
