package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type QuotaLimits struct {
	KeyRequestsPerMinute  int64
	UserRequestsPerMinute int64
	KeyConcurrent         int64
	UserConcurrent        int64
	GlobalConcurrent      int64
	KeyDailyRequests      int64
	UserDailyRequests     int64
}

type ReserveQuotaParams struct {
	RequestID      string
	UserID         string
	APIKeyID       string
	Day            time.Time
	Now            time.Time
	ReservedTokens int64
	LeaseTTL       time.Duration
	Limits         QuotaLimits
}

type QuotaReservation struct {
	RequestID      string
	Day            time.Time
	UserID         string
	APIKeyID       string
	ReservedTokens int64
	ActualTokens   *int64
	State          string
	CreatedAt      time.Time
	SettledAt      *time.Time
	LeaseExpiresAt *time.Time
}

// AdmitRequestParams combines every durable mutation needed before a request
// may be forwarded. Billing is nil only for endpoints such as GET /v1/models
// that consume traffic quota but no monetary credit.
type AdmitRequestParams struct {
	Quota   ReserveQuotaParams
	Usage   BeginUsageRequestParams
	Billing *BillingReservationParams
}

type RequestAdmission struct {
	Quota   QuotaReservation
	Usage   UsageRequest
	Billing *BillingReservation
}

// AdmitRequest makes quota admission, optional billing-source binding, and
// usage metadata creation indivisible. The quota scopes are always locked
// before the billing account, establishing the lock order used at settlement.
func (s *Store) AdmitRequest(ctx context.Context, params AdmitRequestParams) (RequestAdmission, error) {
	if params.Quota.Now.IsZero() {
		params.Quota.Now = params.Usage.RequestedAt
		if params.Quota.Now.IsZero() {
			params.Quota.Now = s.now().UTC()
		}
	}
	if params.Usage.RequestedAt.IsZero() {
		params.Usage.RequestedAt = params.Quota.Now
	}
	quota, err := s.normalizeReserveQuota(params.Quota)
	if err != nil {
		return RequestAdmission{}, err
	}
	usage, err := s.normalizeBeginUsageRequest(params.Usage)
	if err != nil {
		return RequestAdmission{}, err
	}
	if usage.RequestID != quota.RequestID || usage.UserID != quota.UserID ||
		usage.APIKeyID != quota.APIKeyID || !usage.RequestedAt.Equal(quota.Now) {
		return RequestAdmission{}, fmt.Errorf("%w: inconsistent request admission", ErrInvalid)
	}
	switch usage.Endpoint {
	case "models":
		if params.Billing != nil {
			return RequestAdmission{}, fmt.Errorf("%w: models endpoint cannot be billed", ErrInvalid)
		}
	case "responses", "responses.compact":
		if params.Billing == nil {
			return RequestAdmission{}, fmt.Errorf("%w: response endpoint requires billing", ErrInvalid)
		}
	default:
		return RequestAdmission{}, fmt.Errorf("%w: unsupported usage endpoint", ErrInvalid)
	}
	if params.Billing != nil {
		if params.Billing.RequestID != quota.RequestID || params.Billing.UserID != quota.UserID ||
			params.Billing.APIKeyID != quota.APIKeyID || params.Billing.Model != usage.Model {
			return RequestAdmission{}, fmt.Errorf("%w: inconsistent billing admission", ErrInvalid)
		}
		params.Billing.Now = quota.Now
	}

	var admission RequestAdmission
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var txErr error
		admission.Quota, txErr = reserveQuotaTx(ctx, tx, quota)
		if txErr != nil {
			return txErr
		}
		if params.Billing != nil {
			value, billingErr := reserveBillingTx(ctx, tx, *params.Billing)
			if billingErr != nil {
				return billingErr
			}
			admission.Billing = &value
		}
		admission.Usage, txErr = beginUsageRequestTx(ctx, tx, usage)
		return txErr
	})
	return admission, err
}

type QuotaCounter struct {
	Day               time.Time
	ScopeType         string
	ScopeID           string
	RequestsReserved  int64
	RequestsCompleted int64
	TokensReserved    int64
	TokensUsed        int64
	UpdatedAt         time.Time
}

type QuotaExceededError struct {
	Scope      string
	Dimension  string
	Limit      int64
	Current    int64
	Requested  int64
	RetryAfter time.Duration
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("quota exceeded: %s %s (%d + %d > %d)",
		e.Scope, e.Dimension, e.Current, e.Requested, e.Limit)
}

func (e *QuotaExceededError) Unwrap() error { return ErrQuotaExceeded }

type quotaSnapshot struct {
	requests int64
	reserved int64
	used     int64
}

// ReserveQuota atomically admits an RPM window, key/user daily counters and
// key/user/global concurrency lease. A positive ReservedTokens value limits
// concurrent token overshoot; settlement replaces the estimate with actual
// input+output tokens.
func (s *Store) ReserveQuota(ctx context.Context, params ReserveQuotaParams) (QuotaReservation, error) {
	var err error
	params, err = s.normalizeReserveQuota(params)
	if err != nil {
		return QuotaReservation{}, err
	}
	var reservation QuotaReservation
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var txErr error
		reservation, txErr = reserveQuotaTx(ctx, tx, params)
		return txErr
	})
	return reservation, err
}

func (s *Store) normalizeReserveQuota(params ReserveQuotaParams) (ReserveQuotaParams, error) {
	if params.RequestID == "" || params.UserID == "" || params.APIKeyID == "" || params.ReservedTokens < 0 {
		return params, fmt.Errorf("%w: invalid quota reservation", ErrInvalid)
	}
	if params.Now.IsZero() {
		params.Now = s.now().UTC()
	}
	if params.Day.IsZero() {
		params.Day = params.Now
	}
	if params.LeaseTTL <= 0 {
		params.LeaseTTL = 5 * time.Minute
	}
	if params.LeaseTTL > 30*time.Minute {
		return params, fmt.Errorf("%w: quota lease TTL exceeds 30 minutes", ErrInvalid)
	}
	if err := validateQuotaLimits(params.Limits); err != nil {
		return params, err
	}
	return params, nil
}

func reserveQuotaTx(ctx context.Context, tx *sql.Tx, params ReserveQuotaParams) (QuotaReservation, error) {
	dayText := params.Day.Format("2006-01-02")
	windowStart := params.Now.Truncate(time.Minute)
	leaseExpiry := params.Now.Add(params.LeaseTTL)
	if err := lockQuotaScopes(ctx, tx, params.UserID, params.APIKeyID); err != nil {
		return QuotaReservation{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM concurrency_leases WHERE lease_expires_at <= $1`, params.Now,
	); err != nil {
		return QuotaReservation{}, mapDBError("expire concurrency leases", err)
	}

	var userConcurrent, keyConcurrent, globalConcurrent int64
	if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FILTER (WHERE user_id = $1),
				count(*) FILTER (WHERE api_key_id = $2), count(*)
			FROM concurrency_leases WHERE lease_expires_at > $3`,
		params.UserID, params.APIKeyID, params.Now,
	).Scan(&userConcurrent, &keyConcurrent, &globalConcurrent); err != nil {
		return QuotaReservation{}, mapDBError("read concurrency leases", err)
	}
	if err := enforceQuota("key", "concurrency", params.Limits.KeyConcurrent, keyConcurrent, 1, time.Second); err != nil {
		return QuotaReservation{}, err
	}
	if err := enforceQuota("user", "concurrency", params.Limits.UserConcurrent, userConcurrent, 1, time.Second); err != nil {
		return QuotaReservation{}, err
	}
	if err := enforceQuota("global", "concurrency", params.Limits.GlobalConcurrent, globalConcurrent, 1, time.Second); err != nil {
		return QuotaReservation{}, err
	}

	if _, err := tx.ExecContext(ctx, `
			INSERT INTO quota_rate_windows (window_start, scope_type, scope_id)
			VALUES ($1, 'key', $2), ($1, 'user', $3)
			ON CONFLICT DO NOTHING`, windowStart, params.APIKeyID, params.UserID,
	); err != nil {
		return QuotaReservation{}, mapDBError("initialize rate windows", err)
	}
	rateCounts, err := readRateCounts(ctx, tx, windowStart, params.UserID, params.APIKeyID)
	if err != nil {
		return QuotaReservation{}, err
	}
	retryMinute := windowStart.Add(time.Minute).Sub(params.Now)
	if retryMinute < time.Second {
		retryMinute = time.Second
	}
	if err := enforceQuota("key", "rpm", params.Limits.KeyRequestsPerMinute, rateCounts["key"], 1, retryMinute); err != nil {
		return QuotaReservation{}, err
	}
	if err := enforceQuota("user", "rpm", params.Limits.UserRequestsPerMinute, rateCounts["user"], 1, retryMinute); err != nil {
		return QuotaReservation{}, err
	}

	if _, err := tx.ExecContext(ctx, `
			INSERT INTO quota_counters (quota_day, scope_type, scope_id)
			VALUES ($1::date, 'key', $2), ($1::date, 'user', $3)
			ON CONFLICT DO NOTHING`, dayText, params.APIKeyID, params.UserID,
	); err != nil {
		return QuotaReservation{}, mapDBError("initialize quota counters", err)
	}
	counters, err := readQuotaSnapshots(ctx, tx, dayText, params.UserID, params.APIKeyID)
	if err != nil {
		return QuotaReservation{}, err
	}
	if err := enforceQuota("key", "daily_requests", params.Limits.KeyDailyRequests, counters["key"].requests, 1, 24*time.Hour); err != nil {
		return QuotaReservation{}, err
	}
	if err := enforceQuota("user", "daily_requests", params.Limits.UserDailyRequests, counters["user"].requests, 1, 24*time.Hour); err != nil {
		return QuotaReservation{}, err
	}
	rateResult, err := tx.ExecContext(ctx, `
			UPDATE quota_rate_windows
			SET request_count = request_count + 1, updated_at = $4
			WHERE window_start = $1 AND
				((scope_type = 'key' AND scope_id = $2) OR
				 (scope_type = 'user' AND scope_id = $3))`,
		windowStart, params.APIKeyID, params.UserID, params.Now,
	)
	if err != nil {
		return QuotaReservation{}, mapDBError("increment rate windows", err)
	}
	if err := requireExactlyAffected("increment rate windows", rateResult, 2); err != nil {
		return QuotaReservation{}, err
	}
	dailyResult, err := tx.ExecContext(ctx, `
			UPDATE quota_counters
			SET requests_reserved = requests_reserved + 1,
				tokens_reserved = tokens_reserved + $4, updated_at = $5
			WHERE quota_day = $1::date AND
				((scope_type = 'key' AND scope_id = $2) OR
				 (scope_type = 'user' AND scope_id = $3))`,
		dayText, params.APIKeyID, params.UserID, params.ReservedTokens, params.Now,
	)
	if err != nil {
		return QuotaReservation{}, mapDBError("reserve daily quota", err)
	}
	if err := requireExactlyAffected("reserve daily quota", dailyResult, 2); err != nil {
		return QuotaReservation{}, err
	}
	if _, err := tx.ExecContext(ctx, `
			INSERT INTO quota_reservations
				(request_id, quota_day, user_id, api_key_id, reserved_tokens, created_at)
			VALUES ($1, $2::date, $3, $4, $5, $6)`,
		params.RequestID, dayText, params.UserID, params.APIKeyID,
		params.ReservedTokens, params.Now,
	); err != nil {
		return QuotaReservation{}, mapDBError("insert quota reservation", err)
	}
	if _, err := tx.ExecContext(ctx, `
			INSERT INTO concurrency_leases
				(request_id, user_id, api_key_id, created_at, lease_expires_at)
			VALUES ($1, $2, $3, $4, $5)`,
		params.RequestID, params.UserID, params.APIKeyID, params.Now, leaseExpiry,
	); err != nil {
		return QuotaReservation{}, mapDBError("insert concurrency lease", err)
	}
	return QuotaReservation{
		RequestID: params.RequestID, Day: params.Day, UserID: params.UserID,
		APIKeyID: params.APIKeyID, ReservedTokens: params.ReservedTokens,
		State: "reserved", CreatedAt: params.Now, LeaseExpiresAt: &leaseExpiry,
	}, nil
}

func validateQuotaLimits(limits QuotaLimits) error {
	values := []int64{
		limits.KeyRequestsPerMinute, limits.UserRequestsPerMinute,
		limits.KeyConcurrent, limits.UserConcurrent, limits.GlobalConcurrent,
		limits.KeyDailyRequests, limits.UserDailyRequests,
	}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("%w: quota limits cannot be negative", ErrInvalid)
		}
	}
	return nil
}

func enforceQuota(scope, dimension string, limit, current, requested int64, retry time.Duration) error {
	if limit == 0 || current+requested <= limit {
		return nil
	}
	return &QuotaExceededError{
		Scope: scope, Dimension: dimension, Limit: limit, Current: current,
		Requested: requested, RetryAfter: retry,
	}
}

func lockQuotaScopes(ctx context.Context, tx *sql.Tx, userID, keyID string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_locks (scope_type, scope_id)
		VALUES ('global', 'global'), ('key', $2), ('user', $1)
		ON CONFLICT DO NOTHING`, userID, keyID,
	); err != nil {
		return mapDBError("initialize quota locks", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT scope_type FROM quota_locks
		WHERE (scope_type = 'global' AND scope_id = 'global')
		   OR (scope_type = 'key' AND scope_id = $2)
		   OR (scope_type = 'user' AND scope_id = $1)
		ORDER BY scope_type, scope_id FOR UPDATE`, userID, keyID,
	)
	if err != nil {
		return mapDBError("lock quota scopes", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var ignored string
		if err := rows.Scan(&ignored); err != nil {
			return fmt.Errorf("scan quota lock: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate quota locks: %w", err)
	}
	if count != 3 {
		return fmt.Errorf("lock quota scopes: expected 3 locks, got %d", count)
	}
	return nil
}

func readRateCounts(ctx context.Context, tx *sql.Tx, window time.Time, userID, keyID string) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT scope_type, request_count FROM quota_rate_windows
		WHERE window_start = $1 AND
			((scope_type = 'key' AND scope_id = $2) OR
			 (scope_type = 'user' AND scope_id = $3))`, window, keyID, userID,
	)
	if err != nil {
		return nil, mapDBError("read rate windows", err)
	}
	defer rows.Close()
	result := make(map[string]int64, 2)
	for rows.Next() {
		var scope string
		var count int64
		if err := rows.Scan(&scope, &count); err != nil {
			return nil, fmt.Errorf("scan rate window: %w", err)
		}
		result[scope] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rate windows: %w", err)
	}
	if len(result) != 2 {
		return nil, fmt.Errorf("read rate windows: expected 2 rows, got %d", len(result))
	}
	return result, nil
}

func readQuotaSnapshots(ctx context.Context, tx *sql.Tx, day, userID, keyID string) (map[string]quotaSnapshot, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT scope_type, requests_reserved, tokens_reserved, tokens_used
		FROM quota_counters WHERE quota_day = $1::date AND
			((scope_type = 'key' AND scope_id = $2) OR
			 (scope_type = 'user' AND scope_id = $3))`, day, keyID, userID,
	)
	if err != nil {
		return nil, mapDBError("read quota counters", err)
	}
	defer rows.Close()
	result := make(map[string]quotaSnapshot, 2)
	for rows.Next() {
		var scope string
		var value quotaSnapshot
		if err := rows.Scan(&scope, &value.requests, &value.reserved, &value.used); err != nil {
			return nil, fmt.Errorf("scan quota counter: %w", err)
		}
		result[scope] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quota counters: %w", err)
	}
	if len(result) != 2 {
		return nil, fmt.Errorf("read quota counters: expected 2 rows, got %d", len(result))
	}
	return result, nil
}

func (s *Store) SettleQuota(ctx context.Context, requestID string, actualTokens int64, at time.Time) error {
	if requestID == "" || actualTokens < 0 {
		return fmt.Errorf("%w: invalid quota settlement", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		pre, err := readReservation(ctx, tx, requestID, false)
		if err != nil {
			return err
		}
		if err := lockQuotaScopes(ctx, tx, pre.UserID, pre.APIKeyID); err != nil {
			return err
		}
		return settleQuotaTx(ctx, tx, requestID, actualTokens, at)
	})
}

func settleQuotaTx(ctx context.Context, tx *sql.Tx, requestID string, actualTokens int64, at time.Time) error {
	reservation, err := readReservation(ctx, tx, requestID, true)
	if err != nil {
		return err
	}
	switch reservation.State {
	case "settled":
		if reservation.ActualTokens != nil && *reservation.ActualTokens == actualTokens {
			return nil
		}
		return fmt.Errorf("settle quota: %w", ErrConflict)
	case "released":
		return fmt.Errorf("settle released quota: %w", ErrConflict)
	}
	result, err := tx.ExecContext(ctx, `
			UPDATE quota_counters
			SET requests_completed = requests_completed + 1,
				tokens_reserved = tokens_reserved - $4,
				tokens_used = tokens_used + $5, updated_at = $6
			WHERE quota_day = $1 AND tokens_reserved >= $4 AND
				((scope_type = 'key' AND scope_id = $2) OR
				 (scope_type = 'user' AND scope_id = $3))`,
		reservation.Day, reservation.APIKeyID, reservation.UserID,
		reservation.ReservedTokens, actualTokens, at,
	)
	if err != nil {
		return mapDBError("settle quota counters", err)
	}
	if err := requireExactlyAffected("settle quota counters", result, 2); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
			UPDATE quota_reservations
			SET state = 'settled', actual_tokens = $2, settled_at = $3
			WHERE request_id = $1`, requestID, actualTokens, at,
	); err != nil {
		return mapDBError("settle quota reservation", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM concurrency_leases WHERE request_id = $1`, requestID,
	); err != nil {
		return mapDBError("release concurrency lease", err)
	}
	return nil
}

// SettleRequest reads terminal usage metadata and settles traffic quota and
// monetary billing in one idempotent transaction. The requested-model pricing
// snapshot bound at admission is the only pricing input used by billing.
func (s *Store) SettleRequest(ctx context.Context, requestID string, at time.Time) error {
	if requestID == "" {
		return fmt.Errorf("%w: empty request settlement id", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		pre, err := readReservation(ctx, tx, requestID, false)
		if err != nil {
			return err
		}
		if err := lockQuotaScopes(ctx, tx, pre.UserID, pre.APIKeyID); err != nil {
			return err
		}
		var state, endpoint string
		var inputTokens, outputTokens int64
		if err := tx.QueryRowContext(ctx, `
			SELECT state, endpoint, input_tokens, output_tokens
			FROM usage_requests WHERE request_id = $1`, requestID,
		).Scan(&state, &endpoint, &inputTokens, &outputTokens); err != nil {
			return mapDBError("read terminal usage for settlement", err)
		}
		if state == "in_progress" || inputTokens > int64(^uint64(0)>>1)-outputTokens {
			return fmt.Errorf("settle request without terminal usage: %w", ErrConflict)
		}
		if err := settleQuotaTx(ctx, tx, requestID, inputTokens+outputTokens, at); err != nil {
			return err
		}
		if endpoint != "models" {
			if _, err := settleBillingTx(ctx, tx, requestID, at); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ReleaseQuota(ctx context.Context, requestID string, at time.Time) error {
	if requestID == "" {
		return fmt.Errorf("%w: empty quota reservation id", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		pre, err := readReservation(ctx, tx, requestID, false)
		if err != nil {
			return err
		}
		if err := lockQuotaScopes(ctx, tx, pre.UserID, pre.APIKeyID); err != nil {
			return err
		}
		return releaseQuotaTx(ctx, tx, requestID, at)
	})
}

func releaseQuotaTx(ctx context.Context, tx *sql.Tx, requestID string, at time.Time) error {
	reservation, err := readReservation(ctx, tx, requestID, true)
	if err != nil {
		return err
	}
	if reservation.State == "released" {
		return nil
	}
	if reservation.State == "settled" {
		return fmt.Errorf("release settled quota: %w", ErrConflict)
	}
	result, err := tx.ExecContext(ctx, `
			UPDATE quota_counters
			SET requests_reserved = requests_reserved - 1,
				tokens_reserved = tokens_reserved - $4, updated_at = $5
			WHERE quota_day = $1 AND requests_reserved > 0 AND tokens_reserved >= $4 AND
				((scope_type = 'key' AND scope_id = $2) OR
				 (scope_type = 'user' AND scope_id = $3))`,
		reservation.Day, reservation.APIKeyID, reservation.UserID,
		reservation.ReservedTokens, at,
	)
	if err != nil {
		return mapDBError("release quota counters", err)
	}
	if err := requireExactlyAffected("release quota counters", result, 2); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
			UPDATE quota_reservations
			SET state = 'released', settled_at = $2
			WHERE request_id = $1`, requestID, at,
	); err != nil {
		return mapDBError("release quota reservation", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM concurrency_leases WHERE request_id = $1`, requestID,
	); err != nil {
		return mapDBError("release concurrency lease", err)
	}
	return nil
}

// ReleaseRequest atomically releases both quota and billing reservations. It
// is only appropriate before terminal usage exists; maintenance enforces that
// condition when repairing stale requests.
func (s *Store) ReleaseRequest(ctx context.Context, requestID string, at time.Time) error {
	if requestID == "" {
		return fmt.Errorf("%w: empty request release id", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		pre, err := readReservation(ctx, tx, requestID, false)
		if err != nil {
			return err
		}
		if err := lockQuotaScopes(ctx, tx, pre.UserID, pre.APIKeyID); err != nil {
			return err
		}
		reservation, err := readReservation(ctx, tx, requestID, true)
		if err != nil {
			return err
		}
		switch reservation.State {
		case "released":
			return nil
		case "settled":
			return fmt.Errorf("release settled request: %w", ErrConflict)
		}

		// Lock the usage row before changing either reservation. This closes the
		// race between stale cleanup and terminal usage persistence: a terminal
		// completion that commits first can never be released, while cleanup that
		// wins records an explicit terminal cancellation before releasing quota.
		var usageState, endpoint string
		var requestedAt time.Time
		if err := tx.QueryRowContext(ctx, `
			SELECT state, endpoint, requested_at FROM usage_requests
			WHERE request_id = $1 FOR UPDATE`, requestID,
		).Scan(&usageState, &endpoint, &requestedAt); err != nil {
			return mapDBError("lock usage request for release", err)
		}
		if usageState != "in_progress" {
			return fmt.Errorf("release request with terminal usage: %w", ErrConflict)
		}
		if at.Before(requestedAt) {
			at = requestedAt
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE usage_requests
			SET state = 'cancelled', http_status = 499,
				error_code = 'reservation_released', completed_at = $2,
				duration_ms = GREATEST(
					0, ROUND(EXTRACT(EPOCH FROM ($2 - requested_at)) * 1000)::bigint
				)
			WHERE request_id = $1 AND state = 'in_progress'`, requestID, at,
		)
		if err != nil {
			return mapDBError("complete released usage request", err)
		}
		if err := requireExactlyAffected("complete released usage request", result, 1); err != nil {
			return err
		}
		if err := releaseQuotaTx(ctx, tx, requestID, at); err != nil {
			return err
		}
		if endpoint != "models" {
			if err := releaseBillingTx(ctx, tx, requestID, at); err != nil {
				return err
			}
		}
		return nil
	})
}

func readReservation(ctx context.Context, tx *sql.Tx, requestID string, forUpdate bool) (QuotaReservation, error) {
	query := `SELECT r.request_id, r.quota_day, r.user_id, r.api_key_id,
		r.reserved_tokens, r.actual_tokens, r.state, r.created_at, r.settled_at,
		l.lease_expires_at
		FROM quota_reservations r
		LEFT JOIN concurrency_leases l ON l.request_id = r.request_id
		WHERE r.request_id = $1`
	if forUpdate {
		query += ` FOR UPDATE OF r`
	}
	var reservation QuotaReservation
	err := tx.QueryRowContext(ctx, query, requestID).Scan(
		&reservation.RequestID, &reservation.Day, &reservation.UserID,
		&reservation.APIKeyID, &reservation.ReservedTokens, &reservation.ActualTokens,
		&reservation.State, &reservation.CreatedAt, &reservation.SettledAt,
		&reservation.LeaseExpiresAt,
	)
	return reservation, mapDBError("read quota reservation", err)
}

func (s *Store) RenewQuotaLease(ctx context.Context, requestID string, at time.Time, ttl time.Duration) error {
	if ttl <= 0 || ttl > 30*time.Minute {
		return fmt.Errorf("%w: invalid quota lease TTL", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE concurrency_leases l SET lease_expires_at = $3
		FROM quota_reservations r
		WHERE l.request_id = $1 AND r.request_id = l.request_id
		  AND r.state = 'reserved' AND l.lease_expires_at > $2`,
		requestID, at, at.Add(ttl),
	)
	if err != nil {
		return mapDBError("renew quota lease", err)
	}
	return requireAffected("renew quota lease", result)
}

func (s *Store) GetQuotaCounters(ctx context.Context, day time.Time, userID, keyID string) ([]QuotaCounter, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT quota_day, scope_type, scope_id, requests_reserved, requests_completed,
			tokens_reserved, tokens_used, updated_at
		FROM quota_counters WHERE quota_day = $1::date AND
			((scope_type = 'key' AND scope_id = $2) OR
			 (scope_type = 'user' AND scope_id = $3))
		ORDER BY scope_type`, day.Format("2006-01-02"), keyID, userID,
	)
	if err != nil {
		return nil, mapDBError("get quota counters", err)
	}
	defer rows.Close()
	var result []QuotaCounter
	for rows.Next() {
		var counter QuotaCounter
		if err := rows.Scan(
			&counter.Day, &counter.ScopeType, &counter.ScopeID,
			&counter.RequestsReserved, &counter.RequestsCompleted,
			&counter.TokensReserved, &counter.TokensUsed, &counter.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan quota counter: %w", err)
		}
		result = append(result, counter)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quota counters: %w", err)
	}
	return result, nil
}

// ReleaseStaleQuotaReservations repairs token reservations left by a crashed
// process. Its cutoff should be comfortably longer than the maximum stream
// duration and lease renewal interval.
func (s *Store) ReleaseStaleQuotaReservations(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.request_id, COALESCE(u.state, '')
		FROM quota_reservations r
		LEFT JOIN concurrency_leases l ON l.request_id = r.request_id
		LEFT JOIN usage_requests u ON u.request_id = r.request_id
		WHERE r.state = 'reserved' AND r.created_at < $1
		  AND (l.request_id IS NULL OR l.lease_expires_at <= now())
		  AND (u.request_id IS NULL OR u.state = 'in_progress')
		ORDER BY r.created_at LIMIT $2`, before, limit,
	)
	if err != nil {
		return 0, mapDBError("list stale quota reservations", err)
	}
	type staleReservation struct {
		id         string
		usageState string
	}
	var reservations []staleReservation
	for rows.Next() {
		var reservation staleReservation
		if err := rows.Scan(&reservation.id, &reservation.usageState); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan stale quota reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close stale quota reservations: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate stale quota reservations: %w", err)
	}
	released := 0
	for _, reservation := range reservations {
		var err error
		if reservation.usageState == "" {
			err = s.ReleaseQuota(ctx, reservation.id, s.now().UTC())
		} else {
			err = s.ReleaseRequest(ctx, reservation.id, s.now().UTC())
		}
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}

// RetryUnsettledRequests repairs requests whose terminal usage metadata was
// committed before the joint quota/billing settlement could commit. It runs
// before stale release so completed work is never mistaken for an abandoned
// admission.
func (s *Store) RetryUnsettledRequests(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT q.request_id
		FROM quota_reservations q
		JOIN usage_requests u ON u.request_id = q.request_id
		LEFT JOIN billing_reservations b ON b.request_id = q.request_id
		WHERE u.state <> 'in_progress'
		  AND (q.state = 'reserved' OR b.state = 'reserved')
		ORDER BY u.completed_at, q.created_at LIMIT $1`, limit)
	if err != nil {
		return 0, mapDBError("list unsettled terminal requests", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan unsettled terminal request: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close unsettled terminal requests: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate unsettled terminal requests: %w", err)
	}
	settled := 0
	for _, id := range ids {
		err := s.SettleRequest(ctx, id, s.now().UTC())
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return settled, err
		}
		settled++
	}
	return settled, nil
}

func (s *Store) DeleteQuotaStateBefore(ctx context.Context, before time.Time) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM quota_rate_windows WHERE window_start < $1`, before,
		); err != nil {
			return mapDBError("delete old quota rate windows", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM quota_reservations
			WHERE state <> 'reserved' AND settled_at < $1
			  AND NOT EXISTS (
				SELECT 1 FROM billing_reservations b
				WHERE b.request_id = quota_reservations.request_id
				  AND b.state = 'reserved'
			  )`, before,
		); err != nil {
			return mapDBError("delete old quota reservations", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM quota_counters WHERE quota_day < $1::date`, before.Format("2006-01-02"),
		); err != nil {
			return mapDBError("delete old quota counters", err)
		}
		return nil
	})
}

func requireExactlyAffected(operation string, result sql.Result, expected int64) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if n != expected {
		return fmt.Errorf("%s: expected %d rows, updated %d: %w", operation, expected, n, ErrConflict)
	}
	return nil
}
