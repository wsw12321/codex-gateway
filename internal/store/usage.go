package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const usageRequestColumns = `id, request_id, user_id, device_id, api_key_id, key_prefix,
	project_id, model, requested_model, requested_service_tier, actual_service_tier,
	endpoint, state, http_status, error_code, requested_at,
	first_token_at, completed_at, ttft_ms, duration_ms, input_tokens,
	cached_input_tokens, cache_write_tokens, cache_write_tokens_present,
	output_tokens, reasoning_tokens, request_bytes, response_bytes,
	upstream_request_id, pricing_rule_version, pricing_service_tier,
	context_class, pricing_fallback_reason`

func scanUsageRequest(row rowScanner) (UsageRequest, error) {
	var request UsageRequest
	err := row.Scan(
		&request.ID, &request.RequestID, &request.UserID, &request.DeviceID,
		&request.APIKeyID, &request.KeyPrefix, &request.ProjectID, &request.Model,
		&request.RequestedModel, &request.RequestedServiceTier, &request.ActualServiceTier,
		&request.Endpoint, &request.State, &request.HTTPStatus, &request.ErrorCode,
		&request.RequestedAt, &request.FirstTokenAt, &request.CompletedAt,
		&request.TTFTMillis, &request.DurationMillis, &request.InputTokens,
		&request.CachedInputTokens, &request.CacheWriteTokens,
		&request.CacheWriteTokensPresent, &request.OutputTokens, &request.ReasoningTokens,
		&request.RequestBytes, &request.ResponseBytes, &request.UpstreamRequestID,
		&request.PricingRuleVersion, &request.PricingServiceTier,
		&request.ContextClass, &request.PricingFallbackReason,
	)
	return request, err
}

type BeginUsageRequestParams struct {
	RequestID            string
	UserID               string
	DeviceID             string
	APIKeyID             string
	ProjectID            string
	Model                string
	RequestedServiceTier string
	PricingRuleVersion   int
	Endpoint             string
	RequestedAt          time.Time
	RequestBytes         int64
}

func (s *Store) BeginUsageRequest(ctx context.Context, params BeginUsageRequestParams) (UsageRequest, error) {
	var err error
	params, err = s.normalizeBeginUsageRequest(params)
	if err != nil {
		return UsageRequest{}, err
	}
	return beginUsageRequestTx(ctx, s.db, params)
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) normalizeBeginUsageRequest(params BeginUsageRequestParams) (BeginUsageRequestParams, error) {
	params.Model = strings.TrimSpace(params.Model)
	params.RequestedServiceTier = strings.TrimSpace(params.RequestedServiceTier)
	if params.PricingRuleVersion == 0 {
		params.PricingRuleVersion = 1
	}
	if params.RequestID == "" || params.UserID == "" || params.DeviceID == "" ||
		params.APIKeyID == "" || params.Model == "" || params.RequestBytes < 0 ||
		(params.PricingRuleVersion != 1 && params.PricingRuleVersion != 2) ||
		len(params.RequestedServiceTier) > 32 {
		return params, fmt.Errorf("%w: invalid usage request metadata", ErrInvalid)
	}
	if params.RequestedAt.IsZero() {
		params.RequestedAt = s.now().UTC()
	}
	return params, nil
}

func beginUsageRequestTx(ctx context.Context, queryer queryRower, params BeginUsageRequestParams) (UsageRequest, error) {
	request, err := scanUsageRequest(queryer.QueryRowContext(ctx, `
		INSERT INTO usage_requests
			(request_id, user_id, device_id, api_key_id, key_prefix, project_id,
			 model, requested_model, requested_service_tier, pricing_rule_version,
			 endpoint, requested_at, request_bytes)
		SELECT $1, $2, $3, k.id, k.key_prefix, $5, $6, $6, $7, $8, $9, $10, $11
		FROM api_key_history k
		WHERE k.id = $4 AND k.user_id = $2 AND k.device_id = $3
		RETURNING `+usageRequestColumns,
		params.RequestID, params.UserID, params.DeviceID, params.APIKeyID,
		valueOrNil(params.ProjectID), params.Model, valueOrNil(params.RequestedServiceTier),
		params.PricingRuleVersion, params.Endpoint, params.RequestedAt, params.RequestBytes,
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
	RequestID               string
	State                   string
	HTTPStatus              int
	ErrorCode               string
	FirstTokenAt            *time.Time
	CompletedAt             time.Time
	InputTokens             int64
	CachedInputTokens       int64
	CacheWriteTokens        int64
	CacheWriteTokensPresent bool
	OutputTokens            int64
	ReasoningTokens         int64
	RequestBytes            int64
	ResponseBytes           int64
	UpstreamRequestID       string
	ActualModel             string
	ActualServiceTier       string
}

func (s *Store) CompleteUsageRequest(ctx context.Context, params CompleteUsageRequestParams) (UsageRequest, error) {
	if params.RequestID == "" ||
		(params.State != "completed" && params.State != "failed" && params.State != "cancelled") {
		return UsageRequest{}, fmt.Errorf("%w: invalid terminal usage state", ErrInvalid)
	}
	if params.HTTPStatus < 100 || params.HTTPStatus > 599 || params.InputTokens < 0 ||
		params.CachedInputTokens < 0 || params.CacheWriteTokens < 0 ||
		params.OutputTokens < 0 || params.ReasoningTokens < 0 ||
		params.RequestBytes < 0 || params.ResponseBytes < 0 ||
		params.CachedInputTokens > params.InputTokens ||
		params.CacheWriteTokens > params.InputTokens-params.CachedInputTokens {
		return UsageRequest{}, fmt.Errorf("%w: invalid usage metrics", ErrInvalid)
	}
	if params.CompletedAt.IsZero() {
		params.CompletedAt = s.now().UTC()
	}
	params.ActualModel = strings.TrimSpace(params.ActualModel)
	params.ActualServiceTier = strings.TrimSpace(params.ActualServiceTier)
	if len(params.ActualServiceTier) > 32 {
		return UsageRequest{}, fmt.Errorf("%w: invalid actual service tier", ErrInvalid)
	}
	args := []any{
		params.RequestID, params.State, params.HTTPStatus, valueOrNil(params.ErrorCode),
		timeOrNil(params.FirstTokenAt), params.CompletedAt, params.InputTokens,
		params.CachedInputTokens, params.CacheWriteTokens, params.CacheWriteTokensPresent,
		params.OutputTokens, params.ReasoningTokens, params.RequestBytes, params.ResponseBytes,
		valueOrNil(params.UpstreamRequestID), params.ActualModel,
		valueOrNil(params.ActualServiceTier),
	}
	request, err := scanUsageRequest(s.db.QueryRowContext(ctx, `
		UPDATE usage_requests
		SET state = $2, http_status = $3, error_code = $4,
			first_token_at = COALESCE(first_token_at, $5), completed_at = $6,
			ttft_ms = CASE
				WHEN COALESCE(first_token_at, $5) IS NULL THEN NULL
				ELSE ROUND(EXTRACT(EPOCH FROM (COALESCE(first_token_at, $5) - requested_at)) * 1000)::bigint
			END,
			duration_ms = ROUND(EXTRACT(EPOCH FROM ($6 - requested_at)) * 1000)::bigint,
			input_tokens = $7, cached_input_tokens = $8, cache_write_tokens = $9,
			cache_write_tokens_present = $10, output_tokens = $11,
			reasoning_tokens = $12, request_bytes = $13, response_bytes = $14,
			upstream_request_id = $15, model = COALESCE(NULLIF($16, ''), model),
			actual_service_tier = $17
		WHERE request_id = $1 AND state = 'in_progress'
		  AND $6 >= requested_at
		  AND ($5::timestamptz IS NULL OR ($5 >= requested_at AND $5 <= $6))
		RETURNING `+usageRequestColumns,
		args...,
	))
	mapped := mapDBError("complete usage request", err)
	if mapped == nil {
		return request, nil
	}
	if !errors.Is(mapped, ErrNotFound) {
		return UsageRequest{}, mapped
	}

	// Completion may have committed even if the caller observed a transient
	// connection error. Accept an exact terminal replay so the server can retry
	// metadata persistence without risking a conflicting second completion.
	request, err = scanUsageRequest(s.db.QueryRowContext(ctx, `
		SELECT `+usageRequestColumns+` FROM usage_requests
		WHERE request_id = $1 AND state = $2 AND http_status = $3
		  AND error_code IS NOT DISTINCT FROM $4::text
		  AND completed_at = $6::timestamptz
		  AND input_tokens = $7 AND cached_input_tokens = $8
		  AND cache_write_tokens = $9 AND cache_write_tokens_present = $10
		  AND output_tokens = $11 AND reasoning_tokens = $12
		  AND request_bytes = $13 AND response_bytes = $14
		  AND upstream_request_id IS NOT DISTINCT FROM $15::text
		  AND first_token_at IS NOT DISTINCT FROM $5::timestamptz
		  AND ($16::text = '' OR model = $16)
		  AND actual_service_tier IS NOT DISTINCT FROM $17::text`, args...,
	))
	if err == nil {
		return request, nil
	}
	replayErr := mapDBError("replay usage completion", err)
	if !errors.Is(replayErr, ErrNotFound) {
		return UsageRequest{}, replayErr
	}
	var existingState string
	if err := s.db.QueryRowContext(ctx,
		`SELECT state FROM usage_requests WHERE request_id = $1`, params.RequestID,
	).Scan(&existingState); err == nil && existingState != "in_progress" {
		return UsageRequest{}, fmt.Errorf("complete usage request differently: %w", ErrConflict)
	}
	return UsageRequest{}, mapped
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

func usageFilterWhere(filter UsageFilter, alias string) ([]string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 12)
	column := func(name string) string {
		if alias == "" {
			return name
		}
		return alias + "." + name
	}
	add := func(condition string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(condition, len(args)))
	}
	if filter.From != nil {
		add(column("requested_at")+" >= $%d", *filter.From)
	}
	if filter.Until != nil {
		add(column("requested_at")+" < $%d", *filter.Until)
	}
	if filter.UserID != "" {
		add(column("user_id")+" = $%d", filter.UserID)
	}
	if filter.DeviceID != "" {
		add(column("device_id")+" = $%d", filter.DeviceID)
	}
	if filter.APIKeyID != "" {
		add(column("api_key_id")+" = $%d", filter.APIKeyID)
	}
	if filter.ProjectID != "" {
		add(column("project_id")+" = $%d", filter.ProjectID)
	}
	if filter.Model != "" {
		add(column("model")+" = $%d", filter.Model)
	}
	if filter.State != "" {
		add(column("state")+" = $%d", filter.State)
	}
	if filter.HTTPStatus != 0 {
		add(column("http_status")+" = $%d", filter.HTTPStatus)
	}
	if filter.StatusClass != 0 {
		add(column("http_status")+" / 100 = $%d", filter.StatusClass)
	}
	return where, args
}

func (s *Store) ListUsageRequests(ctx context.Context, filter UsageFilter) ([]UsageRequest, error) {
	where, args := usageFilterWhere(filter, "u")
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	args = append(args, filter.Limit, filter.Offset)
	query := `SELECT ` + usageRequestColumns + ` FROM usage_requests u WHERE ` +
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
	where, args := usageFilterWhere(filter, "u")
	query := `SELECT
		count(*)::bigint,
		count(*) FILTER (WHERE u.state <> 'completed' OR u.http_status >= 400)::bigint,
		COALESCE(sum(u.input_tokens), 0)::bigint,
		COALESCE(sum(u.cached_input_tokens), 0)::bigint,
		COALESCE(sum(u.cache_write_tokens), 0)::bigint,
		COALESCE(sum(u.output_tokens), 0)::bigint,
		COALESCE(sum(u.reasoning_tokens), 0)::bigint,
		COALESCE(sum(u.request_bytes), 0)::bigint,
		COALESCE(sum(u.response_bytes), 0)::bigint,
		COALESCE(ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY u.ttft_ms)
			FILTER (WHERE u.ttft_ms IS NOT NULL)), 0)::bigint,
		COALESCE(ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY u.duration_ms)
			FILTER (WHERE u.duration_ms IS NOT NULL)), 0)::bigint,
		COALESCE(sum(l.charged_usd), 0::numeric)::text
		FROM usage_requests u
		LEFT JOIN billing_ledger_entries l
			ON l.request_id = u.request_id AND l.entry_type = 'usage_charge'
		WHERE ` + strings.Join(where, " AND ")
	var summary UsageSummary
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&summary.RequestCount, &summary.ErrorCount, &summary.InputTokens,
		&summary.CachedInputTokens, &summary.CacheWriteTokens,
		&summary.OutputTokens, &summary.ReasoningTokens,
		&summary.RequestBytes, &summary.ResponseBytes,
		&summary.P95TTFTMillis, &summary.P95DurationMillis, &summary.ChargedUSD,
	)
	return summary, mapDBError("summarize usage requests", err)
}

// GlobalUsage combines long-lived token aggregates with immutable usage-charge
// ledger amounts. Changing the process pricing JSON can therefore never
// rewrite historical USD totals. In all-history mode, completed monthly
// aggregates before liveFrom are combined with request metadata from liveFrom
// onward, while the ledger remains independently queryable after detail
// retention.
func (s *Store) GlobalUsage(ctx context.Context, from, until time.Time, model string, all bool, liveFrom time.Time) ([]GlobalUsageRow, error) {
	model = strings.TrimSpace(model)
	var usageSource, ledgerTimeClause string
	var args []any
	if all {
		if liveFrom.IsZero() || until.IsZero() || !liveFrom.Before(until) {
			return nil, fmt.Errorf("%w: invalid all-history usage interval", ErrInvalid)
		}
		usageSource = `
			SELECT user_id, model, request_count, input_tokens, cached_input_tokens,
				cache_write_tokens, output_tokens, reasoning_tokens
			FROM usage_monthly WHERE usage_month < $1::date
			UNION ALL
			SELECT user_id, model, 1::bigint, input_tokens, cached_input_tokens,
				cache_write_tokens, output_tokens, reasoning_tokens
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND requested_at >= $2 AND requested_at < $3`
		args = []any{liveFrom.Format("2006-01-02"), liveFrom, until}
		ledgerTimeClause = `COALESCE(usage_requested_at, created_at) < $3`
	} else {
		if from.IsZero() || until.IsZero() || !from.Before(until) {
			return nil, fmt.Errorf("%w: invalid bounded usage interval", ErrInvalid)
		}
		usageSource = `
			SELECT user_id, model, 1::bigint AS request_count, input_tokens,
				cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND requested_at >= $1 AND requested_at < $2`
		args = []any{from, until}
		ledgerTimeClause = `COALESCE(usage_requested_at, created_at) >= $1
			AND COALESCE(usage_requested_at, created_at) < $2`
	}
	modelClause := ""
	ledgerModelClause := ""
	if model != "" {
		args = append(args, model)
		modelClause = fmt.Sprintf(" WHERE usage_source.model = $%d", len(args))
		ledgerModelClause = fmt.Sprintf(" WHERE ledger_source.model = $%d", len(args))
	}
	query := `WITH usage_source AS (` + usageSource + `), usage_totals AS (
		SELECT user_id, model, sum(request_count)::bigint request_count,
			sum(input_tokens)::bigint input_tokens, sum(cached_input_tokens)::bigint cached_input_tokens,
			sum(cache_write_tokens)::bigint cache_write_tokens,
			sum(output_tokens)::bigint output_tokens, sum(reasoning_tokens)::bigint reasoning_tokens
		FROM usage_source` + modelClause + ` GROUP BY user_id, model
	), ledger_source AS (
		SELECT user_id, COALESCE(actual_model, model, '') AS model,
			COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0) AS ledger_tokens,
			actual_cost_usd, charged_usd, uncovered_usd
		FROM billing_ledger_entries
		WHERE entry_type = 'usage_charge' AND ` + ledgerTimeClause + `
	), ledger_totals AS (
		SELECT user_id, model, sum(ledger_tokens)::bigint ledger_tokens,
			COALESCE(sum(actual_cost_usd), 0)::text actual_cost_usd,
			COALESCE(sum(charged_usd), 0)::text charged_usd,
			COALESCE(sum(uncovered_usd), 0)::text uncovered_usd
		FROM ledger_source` + ledgerModelClause + ` GROUP BY user_id, model
	), dimensions AS (
		SELECT user_id, model FROM usage_totals
		UNION
		SELECT user_id, model FROM ledger_totals
	), combined AS (
		SELECT d.user_id, d.model,
			COALESCE(u.request_count, 0)::bigint request_count,
			COALESCE(u.input_tokens, 0)::bigint input_tokens,
			COALESCE(u.cached_input_tokens, 0)::bigint cached_input_tokens,
			COALESCE(u.cache_write_tokens, 0)::bigint cache_write_tokens,
			COALESCE(u.output_tokens, 0)::bigint output_tokens,
			COALESCE(u.reasoning_tokens, 0)::bigint reasoning_tokens,
			COALESCE(l.ledger_tokens, 0)::bigint ledger_tokens,
			COALESCE(l.actual_cost_usd, '0') actual_cost_usd,
			COALESCE(l.charged_usd, '0') charged_usd,
			COALESCE(l.uncovered_usd, '0') uncovered_usd
		FROM dimensions d
		LEFT JOIN usage_totals u USING (user_id, model)
		LEFT JOIN ledger_totals l USING (user_id, model)
	)
	SELECT u.id, u.username, u.display_name, COALESCE(c.model, ''),
		COALESCE(c.request_count, 0), COALESCE(c.input_tokens, 0),
		COALESCE(c.cached_input_tokens, 0), COALESCE(c.cache_write_tokens, 0),
		COALESCE(c.output_tokens, 0), COALESCE(c.reasoning_tokens, 0),
		COALESCE(c.actual_cost_usd, '0'), COALESCE(c.charged_usd, '0'),
		COALESCE(c.uncovered_usd, '0'), COALESCE(c.ledger_tokens, 0)
	FROM users u LEFT JOIN combined c ON c.user_id = u.id
	ORDER BY u.username, c.model`
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
			&value.CacheWriteTokens, &value.OutputTokens, &value.ReasoningTokens,
			&value.ActualCostUSD, &value.ChargedUSD, &value.UncoveredUSD,
			&value.LedgerTokens,
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

// GlobalPricingBreakdown exposes v2 settlement dimensions directly from the
// immutable ledger. Fallback reasons are split into individual counters even
// when one request needed more than one conservative rule.
func (s *Store) GlobalPricingBreakdown(ctx context.Context, from, until time.Time, model string, all bool) ([]GlobalPricingBreakdownRow, error) {
	model = strings.TrimSpace(model)
	var timeClause string
	var args []any
	if all {
		if until.IsZero() {
			return nil, fmt.Errorf("%w: invalid all-history pricing interval", ErrInvalid)
		}
		args = []any{until}
		timeClause = `COALESCE(usage_requested_at, created_at) < $1`
	} else {
		if from.IsZero() || until.IsZero() || !from.Before(until) {
			return nil, fmt.Errorf("%w: invalid bounded pricing interval", ErrInvalid)
		}
		args = []any{from, until}
		timeClause = `COALESCE(usage_requested_at, created_at) >= $1
			AND COALESCE(usage_requested_at, created_at) < $2`
	}
	modelClause := ""
	if model != "" {
		args = append(args, model)
		modelClause = fmt.Sprintf(" AND COALESCE(actual_model, model, '') = $%d", len(args))
	}
	base := `entry_type = 'usage_charge' AND pricing_rule_version = 2 AND ` + timeClause + modelClause
	query := globalPricingBreakdownSQL(base)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError("global pricing breakdown", err)
	}
	defer rows.Close()
	result := make([]GlobalPricingBreakdownRow, 0)
	for rows.Next() {
		var value GlobalPricingBreakdownRow
		if err := rows.Scan(&value.Dimension, &value.Value, &value.RequestCount,
			&value.CacheWriteTokens, &value.ActualCostUSD); err != nil {
			return nil, fmt.Errorf("scan global pricing breakdown: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate global pricing breakdown: %w", err)
	}
	return result, nil
}

func globalPricingBreakdownSQL(base string) string {
	return `WITH dimensions AS (
		SELECT 'service_tier'::text dimension, pricing_service_tier::text value,
			count(*)::bigint request_count,
			COALESCE(sum(cache_write_tokens), 0)::bigint cache_write_tokens,
			COALESCE(sum(actual_cost_usd), 0)::text actual_cost_usd
		FROM billing_ledger_entries WHERE ` + base + `
		GROUP BY pricing_service_tier
		UNION ALL
		SELECT 'context_class'::text, context_class::text, count(*)::bigint,
			COALESCE(sum(cache_write_tokens), 0)::bigint,
			COALESCE(sum(actual_cost_usd), 0)::text
		FROM billing_ledger_entries WHERE ` + base + `
		GROUP BY context_class
		UNION ALL
		SELECT 'fallback'::text, fallback.value::text, count(*)::bigint,
			COALESCE(sum(cache_write_tokens), 0)::bigint,
			COALESCE(sum(actual_cost_usd), 0)::text
		FROM billing_ledger_entries
		CROSS JOIN LATERAL regexp_split_to_table(pricing_fallback_reason, ',') AS fallback(value)
		WHERE ` + base + ` AND pricing_fallback_reason IS NOT NULL
		GROUP BY fallback.value
	)
	SELECT dimension, value, request_count, cache_write_tokens, actual_cost_usd
	FROM dimensions ORDER BY dimension, value`
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
				cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens, request_bytes,
				response_bytes, ttft_count, ttft_sum_ms, p95_ttft_ms,
				duration_count, duration_sum_ms, p95_duration_ms, updated_at
			)
			SELECT $1::date, user_id, device_id, api_key_id, project_id, model, endpoint,
				COALESCE(http_status / 100, 0)::smallint, error_code,
				count(*)::bigint,
				count(*) FILTER (WHERE http_status >= 400 OR error_code IS NOT NULL)::bigint,
				sum(input_tokens)::bigint, sum(cached_input_tokens)::bigint,
				sum(cache_write_tokens)::bigint, sum(output_tokens)::bigint, sum(reasoning_tokens)::bigint,
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
	input_tokens, cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
	request_bytes, response_bytes, ttft_count, ttft_sum_ms::bigint, p95_ttft_ms,
	duration_count, duration_sum_ms::bigint, p95_duration_ms, updated_at`

func scanDailyUsage(row rowScanner) (DailyUsage, error) {
	var usage DailyUsage
	err := row.Scan(
		&usage.Day, &usage.UserID, &usage.DeviceID, &usage.APIKeyID, &usage.ProjectID,
		&usage.Model, &usage.Endpoint, &usage.StatusClass, &usage.ErrorCode,
		&usage.RequestCount, &usage.ErrorCount, &usage.InputTokens,
		&usage.CachedInputTokens, &usage.CacheWriteTokens,
		&usage.OutputTokens, &usage.ReasoningTokens,
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
				cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens, request_bytes,
				response_bytes, p95_ttft_ms, p95_duration_ms, updated_at
			)
			SELECT $1::date, user_id, device_id, api_key_id, project_id, model, endpoint,
				COALESCE(http_status / 100, 0)::smallint, error_code,
				count(*)::bigint,
				count(*) FILTER (WHERE http_status >= 400 OR error_code IS NOT NULL)::bigint,
				sum(input_tokens)::bigint, sum(cached_input_tokens)::bigint,
				sum(cache_write_tokens)::bigint, sum(output_tokens)::bigint, sum(reasoning_tokens)::bigint,
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
	input_tokens, cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
	request_bytes, response_bytes, p95_ttft_ms, p95_duration_ms, updated_at`

func scanMonthlyUsage(row rowScanner) (MonthlyUsage, error) {
	var usage MonthlyUsage
	err := row.Scan(
		&usage.Month, &usage.UserID, &usage.DeviceID, &usage.APIKeyID, &usage.ProjectID,
		&usage.Model, &usage.Endpoint, &usage.StatusClass, &usage.ErrorCode,
		&usage.RequestCount, &usage.ErrorCount, &usage.InputTokens,
		&usage.CachedInputTokens, &usage.CacheWriteTokens,
		&usage.OutputTokens, &usage.ReasoningTokens,
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
			SELECT u.id FROM usage_requests u
			WHERE u.completed_at < $1
			  AND NOT EXISTS (
				SELECT 1 FROM quota_reservations q
				WHERE q.request_id = u.request_id AND q.state = 'reserved'
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM billing_reservations b
				WHERE b.request_id = u.request_id AND b.state = 'reserved'
			  )
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
