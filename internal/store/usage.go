package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const usageRequestColumns = `id, request_id, user_id, device_id, api_key_id, key_prefix,
	project_id, model, endpoint, state, http_status, error_code, requested_at,
	first_token_at, completed_at, ttft_ms, duration_ms, input_tokens,
	cached_input_tokens, output_tokens, reasoning_tokens, request_bytes,
	response_bytes, upstream_request_id`

func scanUsageRequest(row rowScanner) (UsageRequest, error) {
	var request UsageRequest
	err := row.Scan(
		&request.ID, &request.RequestID, &request.UserID, &request.DeviceID,
		&request.APIKeyID, &request.KeyPrefix, &request.ProjectID, &request.Model,
		&request.Endpoint, &request.State, &request.HTTPStatus, &request.ErrorCode,
		&request.RequestedAt, &request.FirstTokenAt, &request.CompletedAt,
		&request.TTFTMillis, &request.DurationMillis, &request.InputTokens,
		&request.CachedInputTokens, &request.OutputTokens, &request.ReasoningTokens,
		&request.RequestBytes, &request.ResponseBytes, &request.UpstreamRequestID,
	)
	return request, err
}

type BeginUsageRequestParams struct {
	RequestID    string
	UserID       string
	DeviceID     string
	APIKeyID     string
	ProjectID    string
	Model        string
	Endpoint     string
	RequestedAt  time.Time
	RequestBytes int64
}

func (s *Store) BeginUsageRequest(ctx context.Context, params BeginUsageRequestParams) (UsageRequest, error) {
	params.Model = strings.TrimSpace(params.Model)
	if params.RequestID == "" || params.UserID == "" || params.DeviceID == "" ||
		params.APIKeyID == "" || params.Model == "" || params.RequestBytes < 0 {
		return UsageRequest{}, fmt.Errorf("%w: invalid usage request metadata", ErrInvalid)
	}
	if params.RequestedAt.IsZero() {
		params.RequestedAt = s.now().UTC()
	}
	request, err := scanUsageRequest(s.db.QueryRowContext(ctx, `
		INSERT INTO usage_requests
			(request_id, user_id, device_id, api_key_id, key_prefix, project_id,
			 model, endpoint, requested_at, request_bytes)
		SELECT $1, $2, $3, k.id, k.key_prefix, $5, $6, $7, $8, $9
		FROM api_keys k
		WHERE k.id = $4 AND k.user_id = $2 AND k.device_id = $3
		RETURNING `+usageRequestColumns,
		params.RequestID, params.UserID, params.DeviceID, params.APIKeyID,
		valueOrNil(params.ProjectID), params.Model, params.Endpoint,
		params.RequestedAt, params.RequestBytes,
	))
	return request, mapDBError("begin usage request", err)
}

func (s *Store) MarkUsageFirstToken(ctx context.Context, requestID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE usage_requests
		SET first_token_at = $2,
			ttft_ms = ROUND(EXTRACT(EPOCH FROM ($2 - requested_at)) * 1000)::bigint
		WHERE request_id = $1 AND state = 'in_progress'
		  AND first_token_at IS NULL AND $2 >= requested_at`, requestID, at,
	)
	if err != nil {
		return mapDBError("mark first token", err)
	}
	return requireAffected("mark first token", result)
}

type CompleteUsageRequestParams struct {
	RequestID         string
	State             string
	HTTPStatus        int
	ErrorCode         string
	FirstTokenAt      *time.Time
	CompletedAt       time.Time
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	ReasoningTokens   int64
	RequestBytes      int64
	ResponseBytes     int64
	UpstreamRequestID string
	ActualModel       string
}

func (s *Store) CompleteUsageRequest(ctx context.Context, params CompleteUsageRequestParams) (UsageRequest, error) {
	if params.State != "completed" && params.State != "failed" && params.State != "cancelled" {
		return UsageRequest{}, fmt.Errorf("%w: invalid terminal usage state", ErrInvalid)
	}
	if params.HTTPStatus < 100 || params.HTTPStatus > 599 || params.InputTokens < 0 ||
		params.CachedInputTokens < 0 || params.OutputTokens < 0 || params.ReasoningTokens < 0 ||
		params.RequestBytes < 0 || params.ResponseBytes < 0 ||
		params.CachedInputTokens > params.InputTokens {
		return UsageRequest{}, fmt.Errorf("%w: invalid usage metrics", ErrInvalid)
	}
	if params.CompletedAt.IsZero() {
		params.CompletedAt = s.now().UTC()
	}
	params.ActualModel = strings.TrimSpace(params.ActualModel)
	request, err := scanUsageRequest(s.db.QueryRowContext(ctx, `
		UPDATE usage_requests
		SET state = $2, http_status = $3, error_code = $4,
			first_token_at = COALESCE(first_token_at, $5), completed_at = $6,
			ttft_ms = CASE
				WHEN COALESCE(first_token_at, $5) IS NULL THEN NULL
				ELSE ROUND(EXTRACT(EPOCH FROM (COALESCE(first_token_at, $5) - requested_at)) * 1000)::bigint
			END,
			duration_ms = ROUND(EXTRACT(EPOCH FROM ($6 - requested_at)) * 1000)::bigint,
			input_tokens = $7, cached_input_tokens = $8, output_tokens = $9,
			reasoning_tokens = $10, request_bytes = $11, response_bytes = $12,
			upstream_request_id = $13, model = COALESCE(NULLIF($14, ''), model)
		WHERE request_id = $1 AND state = 'in_progress'
		  AND $6 >= requested_at
		  AND ($5::timestamptz IS NULL OR ($5 >= requested_at AND $5 <= $6))
		RETURNING `+usageRequestColumns,
		params.RequestID, params.State, params.HTTPStatus, valueOrNil(params.ErrorCode),
		timeOrNil(params.FirstTokenAt), params.CompletedAt, params.InputTokens,
		params.CachedInputTokens, params.OutputTokens, params.ReasoningTokens,
		params.RequestBytes, params.ResponseBytes, valueOrNil(params.UpstreamRequestID), params.ActualModel,
	))
	return request, mapDBError("complete usage request", err)
}

func (s *Store) GetUsageRequest(ctx context.Context, requestID string) (UsageRequest, error) {
	request, err := scanUsageRequest(s.db.QueryRowContext(ctx,
		`SELECT `+usageRequestColumns+` FROM usage_requests WHERE request_id = $1`, requestID,
	))
	return request, mapDBError("get usage request", err)
}

type UsageFilter struct {
	From        *time.Time
	Until       *time.Time
	UserID      string
	DeviceID    string
	APIKeyID    string
	ProjectID   string
	Model       string
	State       string
	HTTPStatus  int
	StatusClass int
	Limit       int
	Offset      int
}

func usageFilterWhere(filter UsageFilter) ([]string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 12)
	add := func(condition string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(condition, len(args)))
	}
	if filter.From != nil {
		add("requested_at >= $%d", *filter.From)
	}
	if filter.Until != nil {
		add("requested_at < $%d", *filter.Until)
	}
	if filter.UserID != "" {
		add("user_id = $%d", filter.UserID)
	}
	if filter.DeviceID != "" {
		add("device_id = $%d", filter.DeviceID)
	}
	if filter.APIKeyID != "" {
		add("api_key_id = $%d", filter.APIKeyID)
	}
	if filter.ProjectID != "" {
		add("project_id = $%d", filter.ProjectID)
	}
	if filter.Model != "" {
		add("model = $%d", filter.Model)
	}
	if filter.State != "" {
		add("state = $%d", filter.State)
	}
	if filter.HTTPStatus != 0 {
		add("http_status = $%d", filter.HTTPStatus)
	}
	if filter.StatusClass != 0 {
		add("http_status / 100 = $%d", filter.StatusClass)
	}
	return where, args
}

func (s *Store) ListUsageRequests(ctx context.Context, filter UsageFilter) ([]UsageRequest, error) {
	where, args := usageFilterWhere(filter)
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	args = append(args, filter.Limit, filter.Offset)
	query := `SELECT ` + usageRequestColumns + ` FROM usage_requests WHERE ` +
		strings.Join(where, " AND ") + fmt.Sprintf(
		` ORDER BY requested_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args),
	)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError("list usage requests", err)
	}
	defer rows.Close()
	result := make([]UsageRequest, 0)
	for rows.Next() {
		request, err := scanUsageRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scan usage request: %w", err)
		}
		result = append(result, request)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage requests: %w", err)
	}
	return result, nil
}

// SummarizeUsageRequests computes exact totals over the entire filtered
// interval. Detail pagination therefore never makes dashboard totals drift.
func (s *Store) SummarizeUsageRequests(ctx context.Context, filter UsageFilter) (UsageSummary, error) {
	where, args := usageFilterWhere(filter)
	query := `SELECT
		count(*)::bigint,
		count(*) FILTER (WHERE state <> 'completed' OR http_status >= 400)::bigint,
		COALESCE(sum(input_tokens), 0)::bigint,
		COALESCE(sum(cached_input_tokens), 0)::bigint,
		COALESCE(sum(output_tokens), 0)::bigint,
		COALESCE(sum(reasoning_tokens), 0)::bigint,
		COALESCE(sum(request_bytes), 0)::bigint,
		COALESCE(sum(response_bytes), 0)::bigint,
		COALESCE(ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms)
			FILTER (WHERE ttft_ms IS NOT NULL)), 0)::bigint,
		COALESCE(ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
			FILTER (WHERE duration_ms IS NOT NULL)), 0)::bigint
		FROM usage_requests WHERE ` + strings.Join(where, " AND ")
	var summary UsageSummary
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&summary.RequestCount, &summary.ErrorCount, &summary.InputTokens,
		&summary.CachedInputTokens, &summary.OutputTokens, &summary.ReasoningTokens,
		&summary.RequestBytes, &summary.ResponseBytes,
		&summary.P95TTFTMillis, &summary.P95DurationMillis,
	)
	return summary, mapDBError("summarize usage requests", err)
}

// GlobalUsage returns terminal usage grouped at the minimum granularity needed
// for exact model pricing. In all-history mode, completed monthly aggregates
// before liveFrom are combined with request metadata from liveFrom onward.
func (s *Store) GlobalUsage(ctx context.Context, from, until time.Time, model string, all bool, liveFrom time.Time) ([]GlobalUsageRow, error) {
	model = strings.TrimSpace(model)
	var source string
	var args []any
	if all {
		if liveFrom.IsZero() || until.IsZero() || !liveFrom.Before(until) {
			return nil, fmt.Errorf("%w: invalid all-history usage interval", ErrInvalid)
		}
		source = `
			SELECT user_id, model, request_count, input_tokens, cached_input_tokens, output_tokens, reasoning_tokens
			FROM usage_monthly WHERE usage_month < $1::date
			UNION ALL
			SELECT user_id, model, 1::bigint, input_tokens, cached_input_tokens, output_tokens, reasoning_tokens
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND requested_at >= $2 AND requested_at < $3`
		args = []any{liveFrom.Format("2006-01-02"), liveFrom, until}
	} else {
		if from.IsZero() || until.IsZero() || !from.Before(until) {
			return nil, fmt.Errorf("%w: invalid bounded usage interval", ErrInvalid)
		}
		source = `
			SELECT user_id, model, 1::bigint AS request_count, input_tokens,
				cached_input_tokens, output_tokens, reasoning_tokens
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND requested_at >= $1 AND requested_at < $2`
		args = []any{from, until}
	}
	modelClause := ""
	if model != "" {
		args = append(args, model)
		modelClause = fmt.Sprintf(" WHERE source.model = $%d", len(args))
	}
	query := `WITH source AS (` + source + `), totals AS (
		SELECT user_id, model, sum(request_count)::bigint request_count,
			sum(input_tokens)::bigint input_tokens, sum(cached_input_tokens)::bigint cached_input_tokens,
			sum(output_tokens)::bigint output_tokens, sum(reasoning_tokens)::bigint reasoning_tokens
		FROM source` + modelClause + ` GROUP BY user_id, model)
		SELECT u.id, u.username, u.display_name, COALESCE(t.model, ''),
			COALESCE(t.request_count, 0), COALESCE(t.input_tokens, 0), COALESCE(t.cached_input_tokens, 0),
			COALESCE(t.output_tokens, 0), COALESCE(t.reasoning_tokens, 0)
		FROM users u LEFT JOIN totals t ON t.user_id = u.id
		ORDER BY u.username, t.model`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError("global usage", err)
	}
	defer rows.Close()
	result := make([]GlobalUsageRow, 0)
	for rows.Next() {
		var value GlobalUsageRow
		if err := rows.Scan(
			&value.UserID, &value.Username, &value.DisplayName, &value.Model,
			&value.RequestCount, &value.InputTokens, &value.CachedInputTokens,
			&value.OutputTokens, &value.ReasoningTokens,
		); err != nil {
			return nil, fmt.Errorf("scan global usage: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate global usage: %w", err)
	}
	return result, nil
}

// AggregateUsageDay recomputes one complete local calendar day. Recomputing
// rather than incrementally adding makes retries idempotent and keeps p95
// values exact. timezone must be an IANA/PostgreSQL timezone (for example
// "Asia/Shanghai").
func (s *Store) AggregateUsageDay(ctx context.Context, day time.Time, timezone string) error {
	if strings.TrimSpace(timezone) == "" {
		return fmt.Errorf("%w: aggregation timezone is empty", ErrInvalid)
	}
	dayText := day.Format("2006-01-02")
	return s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM usage_daily WHERE usage_day = $1::date`, dayText,
		); err != nil {
			return mapDBError("clear daily usage", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO usage_daily (
				usage_day, user_id, device_id, api_key_id, project_id, model, endpoint,
				status_class, error_code, request_count, error_count, input_tokens,
				cached_input_tokens, output_tokens, reasoning_tokens, request_bytes,
				response_bytes, ttft_count, ttft_sum_ms, p95_ttft_ms,
				duration_count, duration_sum_ms, p95_duration_ms, updated_at
			)
			SELECT $1::date, user_id, device_id, api_key_id, project_id, model, endpoint,
				COALESCE(http_status / 100, 0)::smallint, error_code,
				count(*)::bigint,
				count(*) FILTER (WHERE http_status >= 400 OR error_code IS NOT NULL)::bigint,
				sum(input_tokens)::bigint, sum(cached_input_tokens)::bigint,
				sum(output_tokens)::bigint, sum(reasoning_tokens)::bigint,
				sum(request_bytes)::bigint, sum(response_bytes)::bigint,
				count(ttft_ms)::bigint, COALESCE(sum(ttft_ms), 0)::numeric,
				ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms)
					FILTER (WHERE ttft_ms IS NOT NULL))::bigint,
				count(duration_ms)::bigint, COALESCE(sum(duration_ms), 0)::numeric,
				ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
					FILTER (WHERE duration_ms IS NOT NULL))::bigint,
				now()
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND (requested_at AT TIME ZONE $2)::date = $1::date
			GROUP BY user_id, device_id, api_key_id, project_id, model, endpoint,
				COALESCE(http_status / 100, 0)::smallint, error_code`, dayText, timezone,
		)
		return mapDBError("aggregate daily usage", err)
	})
}

const dailyUsageColumns = `usage_day, user_id, device_id, api_key_id, project_id,
	model, endpoint, status_class, error_code, request_count, error_count,
	input_tokens, cached_input_tokens, output_tokens, reasoning_tokens,
	request_bytes, response_bytes, ttft_count, ttft_sum_ms::bigint, p95_ttft_ms,
	duration_count, duration_sum_ms::bigint, p95_duration_ms, updated_at`

func scanDailyUsage(row rowScanner) (DailyUsage, error) {
	var usage DailyUsage
	err := row.Scan(
		&usage.Day, &usage.UserID, &usage.DeviceID, &usage.APIKeyID, &usage.ProjectID,
		&usage.Model, &usage.Endpoint, &usage.StatusClass, &usage.ErrorCode,
		&usage.RequestCount, &usage.ErrorCount, &usage.InputTokens,
		&usage.CachedInputTokens, &usage.OutputTokens, &usage.ReasoningTokens,
		&usage.RequestBytes, &usage.ResponseBytes, &usage.TTFTCount,
		&usage.TTFTSumMillis, &usage.P95TTFTMillis, &usage.DurationCount,
		&usage.DurationSumMillis, &usage.P95DurationMillis, &usage.UpdatedAt,
	)
	return usage, err
}

func (s *Store) ListDailyUsage(ctx context.Context, from, until time.Time, userID string) ([]DailyUsage, error) {
	query := `SELECT ` + dailyUsageColumns + ` FROM usage_daily
		WHERE usage_day >= $1::date AND usage_day < $2::date`
	args := []any{from.Format("2006-01-02"), until.Format("2006-01-02")}
	if userID != "" {
		query += ` AND user_id = $3`
		args = append(args, userID)
	}
	query += ` ORDER BY usage_day DESC, user_id, device_id, api_key_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError("list daily usage", err)
	}
	defer rows.Close()
	var result []DailyUsage
	for rows.Next() {
		usage, err := scanDailyUsage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan daily usage: %w", err)
		}
		result = append(result, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily usage: %w", err)
	}
	return result, nil
}

// AggregateUsageMonth recomputes one calendar month directly from request
// metadata while detail rows are still within retention, preserving exact p95
// values instead of averaging daily percentiles.
func (s *Store) AggregateUsageMonth(ctx context.Context, month time.Time, timezone string) error {
	if strings.TrimSpace(timezone) == "" {
		return fmt.Errorf("%w: aggregation timezone is empty", ErrInvalid)
	}
	monthText := monthBucket(month)
	return s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM usage_monthly WHERE usage_month = $1::date`, monthText,
		); err != nil {
			return mapDBError("clear monthly usage", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO usage_monthly (
				usage_month, user_id, device_id, api_key_id, project_id, model, endpoint,
				status_class, error_code, request_count, error_count, input_tokens,
				cached_input_tokens, output_tokens, reasoning_tokens, request_bytes,
				response_bytes, p95_ttft_ms, p95_duration_ms, updated_at
			)
			SELECT $1::date, user_id, device_id, api_key_id, project_id, model, endpoint,
				COALESCE(http_status / 100, 0)::smallint, error_code,
				count(*)::bigint,
				count(*) FILTER (WHERE http_status >= 400 OR error_code IS NOT NULL)::bigint,
				sum(input_tokens)::bigint, sum(cached_input_tokens)::bigint,
				sum(output_tokens)::bigint, sum(reasoning_tokens)::bigint,
				sum(request_bytes)::bigint, sum(response_bytes)::bigint,
				ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms)
					FILTER (WHERE ttft_ms IS NOT NULL))::bigint,
				ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
					FILTER (WHERE duration_ms IS NOT NULL))::bigint,
				now()
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND date_trunc('month', requested_at AT TIME ZONE $2)::date = $1::date
			GROUP BY user_id, device_id, api_key_id, project_id, model, endpoint,
				COALESCE(http_status / 100, 0)::smallint, error_code`, monthText, timezone,
		)
		return mapDBError("aggregate monthly usage", err)
	})
}

const monthlyUsageColumns = `usage_month, user_id, device_id, api_key_id, project_id,
	model, endpoint, status_class, error_code, request_count, error_count,
	input_tokens, cached_input_tokens, output_tokens, reasoning_tokens,
	request_bytes, response_bytes, p95_ttft_ms, p95_duration_ms, updated_at`

func scanMonthlyUsage(row rowScanner) (MonthlyUsage, error) {
	var usage MonthlyUsage
	err := row.Scan(
		&usage.Month, &usage.UserID, &usage.DeviceID, &usage.APIKeyID, &usage.ProjectID,
		&usage.Model, &usage.Endpoint, &usage.StatusClass, &usage.ErrorCode,
		&usage.RequestCount, &usage.ErrorCount, &usage.InputTokens,
		&usage.CachedInputTokens, &usage.OutputTokens, &usage.ReasoningTokens,
		&usage.RequestBytes, &usage.ResponseBytes, &usage.P95TTFTMillis,
		&usage.P95DurationMillis, &usage.UpdatedAt,
	)
	return usage, err
}

func (s *Store) ListMonthlyUsage(ctx context.Context, from, until time.Time, userID string) ([]MonthlyUsage, error) {
	query := `SELECT ` + monthlyUsageColumns + ` FROM usage_monthly
		WHERE usage_month >= $1::date AND usage_month < $2::date`
	args := []any{monthBucket(from), monthBucket(until)}
	if userID != "" {
		query += ` AND user_id = $3`
		args = append(args, userID)
	}
	query += ` ORDER BY usage_month DESC, user_id, device_id, api_key_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError("list monthly usage", err)
	}
	defer rows.Close()
	var result []MonthlyUsage
	for rows.Next() {
		usage, err := scanMonthlyUsage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan monthly usage: %w", err)
		}
		result = append(result, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate monthly usage: %w", err)
	}
	return result, nil
}

func monthBucket(value time.Time) string {
	return fmt.Sprintf("%04d-%02d-01", value.Year(), int(value.Month()))
}

func (s *Store) DeleteUsageRequestsBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 100_000 {
		limit = 10_000
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM usage_requests
		WHERE id IN (
			SELECT id FROM usage_requests
			WHERE completed_at < $1
			ORDER BY completed_at LIMIT $2
		)`, before, limit,
	)
	if err != nil {
		return 0, mapDBError("delete old usage requests", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete old usage requests rows affected: %w", err)
	}
	return n, nil
}
