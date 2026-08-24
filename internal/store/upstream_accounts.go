package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var upstreamAccountIDPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)
var upstreamMaskedEmailPattern = regexp.MustCompile(`^[A-Za-z0-9]\*{3}@[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

func normalizeUpstreamAccountID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !upstreamAccountIDPattern.MatchString(value) {
		return "", fmt.Errorf("%w: invalid upstream account id", ErrInvalid)
	}
	return value, nil
}

func normalizeUpstreamAccountSnapshot(value UpstreamAccountSnapshot) (UpstreamAccountSnapshot, error) {
	var err error
	value.ID, err = normalizeUpstreamAccountID(value.ID)
	if err != nil || value.ID == "" {
		return UpstreamAccountSnapshot{}, fmt.Errorf("%w: invalid upstream account snapshot id", ErrInvalid)
	}
	value.MaskedEmail = strings.TrimSpace(value.MaskedEmail)
	if !validMaskedEmail(value.MaskedEmail) {
		return UpstreamAccountSnapshot{}, fmt.Errorf("%w: upstream email is not masked", ErrInvalid)
	}
	value.Plan = strings.ToLower(strings.TrimSpace(value.Plan))
	switch value.Plan {
	case "", "unknown":
		value.Plan = "unknown"
	case "plus", "chatgpt plus":
		value.Plan = "plus"
	case "pro", "chatgpt pro":
		value.Plan = "pro"
	default:
		return UpstreamAccountSnapshot{}, fmt.Errorf("%w: invalid upstream account plan", ErrInvalid)
	}
	switch strings.ToLower(strings.TrimSpace(value.Status)) {
	case UpstreamAccountStatusAvailable, "active", "enabled":
		value.Status = UpstreamAccountStatusAvailable
	case "", UpstreamAccountStatusUnavailable, "inactive", "disabled":
		value.Status = UpstreamAccountStatusUnavailable
	default:
		return UpstreamAccountSnapshot{}, fmt.Errorf("%w: invalid upstream account status", ErrInvalid)
	}
	return value, nil
}

func validMaskedEmail(value string) bool {
	return len(value) >= 7 && len(value) <= 254 && upstreamMaskedEmailPattern.MatchString(value)
}

func scanUpstreamAccount(row rowScanner) (UpstreamAccount, error) {
	var value UpstreamAccount
	var lastSynced sql.NullTime
	err := row.Scan(&value.ID, &value.MaskedEmail, &value.Plan, &value.Status,
		&lastSynced, &value.CreatedAt, &value.UpdatedAt)
	if lastSynced.Valid {
		value.LastSyncedAt = &lastSynced.Time
	}
	return value, err
}

const upstreamAccountColumns = `id, masked_email, plan, status,
	last_synced_at, created_at, updated_at`

// EnsureUpstreamAccount creates a metadata-free placeholder for a recognized,
// opaque tracing ID. Synchronization later replaces its display metadata. It
// never overwrites a previously synchronized account.
func (s *Store) EnsureUpstreamAccount(ctx context.Context, id string, at time.Time) error {
	var err error
	id, err = normalizeUpstreamAccountID(id)
	if err != nil || id == "" {
		return fmt.Errorf("%w: invalid upstream account id", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	} else {
		at = at.UTC()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO upstream_accounts
			(id, masked_email, plan, status, created_at, updated_at)
		VALUES ($1,'','unknown','unavailable',$2,$2)
		ON CONFLICT (id) DO NOTHING`, id, at)
	return mapDBError("ensure upstream account", err)
}

// SyncUpstreamAccounts applies one authoritative, non-secret sidecar snapshot.
// Accounts absent from the snapshot are retained and marked unavailable so
// historical attribution and immutable ledger references remain valid.
func (s *Store) SyncUpstreamAccounts(ctx context.Context, accounts []UpstreamAccountSnapshot, at time.Time) error {
	if at.IsZero() {
		at = s.now().UTC()
	} else {
		at = at.UTC()
	}
	normalized := make([]UpstreamAccountSnapshot, len(accounts))
	seen := make(map[string]struct{}, len(accounts))
	for index, account := range accounts {
		value, err := normalizeUpstreamAccountSnapshot(account)
		if err != nil {
			return err
		}
		if _, exists := seen[value.ID]; exists {
			return fmt.Errorf("%w: duplicate upstream account id", ErrInvalid)
		}
		if value.LastSyncedAt.IsZero() {
			value.LastSyncedAt = at
		} else {
			value.LastSyncedAt = value.LastSyncedAt.UTC()
		}
		seen[value.ID] = struct{}{}
		normalized[index] = value
	}

	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`LOCK TABLE upstream_accounts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account synchronization", err)
		}
		for _, account := range normalized {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO upstream_accounts
					(id, masked_email, plan, status, last_synced_at, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$6)
				ON CONFLICT (id) DO UPDATE SET
					masked_email = EXCLUDED.masked_email,
					plan = EXCLUDED.plan,
					status = EXCLUDED.status,
					last_synced_at = EXCLUDED.last_synced_at,
					updated_at = EXCLUDED.updated_at
				WHERE upstream_accounts.updated_at <= EXCLUDED.updated_at`,
				account.ID, account.MaskedEmail, account.Plan, account.Status,
				account.LastSyncedAt, at,
			); err != nil {
				return mapDBError("synchronize upstream account", err)
			}
		}
		missingQuery := `
			UPDATE upstream_accounts
			SET status = 'unavailable',
				updated_at = $1
			WHERE updated_at <= $1`
		missingArgs := []any{at}
		if len(normalized) > 0 {
			placeholders := make([]string, len(normalized))
			for index, account := range normalized {
				missingArgs = append(missingArgs, account.ID)
				placeholders[index] = fmt.Sprintf("$%d", index+2)
			}
			missingQuery += ` AND id NOT IN (` + strings.Join(placeholders, ",") + `)`
		}
		if _, err := tx.ExecContext(ctx, missingQuery, missingArgs...); err != nil {
			return mapDBError("mark missing upstream accounts unavailable", err)
		}
		return nil
	})
}

func (s *Store) ListUpstreamAccounts(ctx context.Context) ([]UpstreamAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+upstreamAccountColumns+`
		FROM upstream_accounts
		ORDER BY CASE status WHEN 'available' THEN 0 ELSE 1 END,
			lower(masked_email), id`)
	if err != nil {
		return nil, mapDBError("list upstream accounts", err)
	}
	defer rows.Close()
	result := make([]UpstreamAccount, 0)
	for rows.Next() {
		value, err := scanUpstreamAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan upstream account: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upstream accounts: %w", err)
	}
	return result, nil
}

// AttributeUsageRequest records the final successful service account before
// immutable billing settlement. Missing or malformed IDs degrade to the
// unattributed group; an already-created usage ledger entry cannot diverge.
func (s *Store) AttributeUsageRequest(ctx context.Context, requestID, accountID string) error {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%w: empty usage request id", ErrInvalid)
	}
	normalized, err := normalizeUpstreamAccountID(accountID)
	if err != nil {
		normalized = ""
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE usage_requests u
		SET upstream_account_id = (
			SELECT id FROM upstream_accounts WHERE id = $2
		)
		WHERE u.request_id = $1
		  AND (
			NOT EXISTS (
				SELECT 1 FROM billing_ledger_entries l
				WHERE l.request_id = u.request_id AND l.entry_type = 'usage_charge'
			)
			OR u.upstream_account_id IS NOT DISTINCT FROM (
				SELECT id FROM upstream_accounts WHERE id = $2
			)
		  )`, requestID, valueOrNil(normalized))
	if err != nil {
		return mapDBError("attribute usage request", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("attribute usage request rows affected: %w", err)
	}
	if affected == 1 {
		return nil
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM usage_requests WHERE request_id = $1)`, requestID,
	).Scan(&exists); err != nil {
		return mapDBError("check usage request attribution", err)
	}
	if !exists {
		return fmt.Errorf("attribute usage request: %w", ErrNotFound)
	}
	return fmt.Errorf("change settled usage attribution: %w", ErrConflict)
}

// SummarizeUpstreamAccounts returns every known account, including zero-use
// accounts, plus a nil-ID row when unattributed usage exists. All-history mode
// uses monthly aggregates before LiveFrom so detail retention cannot erase
// request and token attribution; cost always comes from the immutable ledger.
func (s *Store) SummarizeUpstreamAccounts(ctx context.Context, filter UpstreamAccountSummaryFilter) ([]UpstreamAccountSummary, error) {
	var usageSource, ledgerTimeClause string
	var args []any
	if filter.All {
		if filter.LiveFrom.IsZero() || filter.Until.IsZero() || !filter.LiveFrom.Before(filter.Until) {
			return nil, fmt.Errorf("%w: invalid all-history upstream account interval", ErrInvalid)
		}
		usageSource = `
			SELECT upstream_account_id, request_count, error_count, input_tokens,
				cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens
			FROM usage_monthly WHERE usage_month < $1::date
			UNION ALL
			SELECT upstream_account_id, 1::bigint,
				CASE WHEN state <> 'completed' OR http_status >= 400 OR error_code IS NOT NULL THEN 1 ELSE 0 END::bigint,
				input_tokens, cached_input_tokens, cache_write_tokens,
				output_tokens, reasoning_tokens
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND requested_at >= $2 AND requested_at < $3`
		args = []any{monthBucket(filter.LiveFrom), filter.LiveFrom, filter.Until}
		ledgerTimeClause = `COALESCE(usage_requested_at, created_at) < $3`
	} else {
		if filter.From.IsZero() || filter.Until.IsZero() || !filter.From.Before(filter.Until) {
			return nil, fmt.Errorf("%w: invalid bounded upstream account interval", ErrInvalid)
		}
		usageSource = `
			SELECT upstream_account_id, 1::bigint AS request_count,
				CASE WHEN state <> 'completed' OR http_status >= 400 OR error_code IS NOT NULL THEN 1 ELSE 0 END::bigint AS error_count,
				input_tokens, cached_input_tokens, cache_write_tokens,
				output_tokens, reasoning_tokens
			FROM usage_requests
			WHERE state <> 'in_progress' AND completed_at IS NOT NULL
			  AND requested_at >= $1 AND requested_at < $2`
		args = []any{filter.From, filter.Until}
		ledgerTimeClause = `COALESCE(usage_requested_at, created_at) >= $1
			AND COALESCE(usage_requested_at, created_at) < $2`
	}
	query := `WITH usage_source AS (` + usageSource + `), usage_totals AS (
		SELECT upstream_account_id, sum(request_count)::bigint request_count,
			sum(error_count)::bigint error_count, sum(input_tokens)::bigint input_tokens,
			sum(cached_input_tokens)::bigint cached_input_tokens,
			sum(cache_write_tokens)::bigint cache_write_tokens,
			sum(output_tokens)::bigint output_tokens,
			sum(reasoning_tokens)::bigint reasoning_tokens
		FROM usage_source GROUP BY upstream_account_id
	), ledger_totals AS (
		SELECT upstream_account_id,
			COALESCE(sum(actual_cost_usd), 0::numeric)::text equivalent_cost_usd
		FROM billing_ledger_entries
		WHERE entry_type = 'usage_charge' AND ` + ledgerTimeClause + `
		GROUP BY upstream_account_id
	), dimensions AS (
		SELECT id AS upstream_account_id FROM upstream_accounts
		UNION SELECT upstream_account_id FROM usage_totals
		UNION SELECT upstream_account_id FROM ledger_totals
	)
	SELECT d.upstream_account_id, COALESCE(a.masked_email, ''),
		COALESCE(a.plan, 'unknown'), COALESCE(a.status, 'unattributed'),
		a.last_synced_at, COALESCE(u.request_count, 0), COALESCE(u.error_count, 0),
		COALESCE(u.input_tokens, 0), COALESCE(u.cached_input_tokens, 0),
		COALESCE(u.cache_write_tokens, 0), COALESCE(u.output_tokens, 0),
		COALESCE(u.reasoning_tokens, 0), COALESCE(l.equivalent_cost_usd, '0')
	FROM dimensions d
	LEFT JOIN upstream_accounts a ON a.id = d.upstream_account_id
	LEFT JOIN usage_totals u ON u.upstream_account_id IS NOT DISTINCT FROM d.upstream_account_id
	LEFT JOIN ledger_totals l ON l.upstream_account_id IS NOT DISTINCT FROM d.upstream_account_id
	ORDER BY d.upstream_account_id IS NULL,
		CASE COALESCE(a.status, 'unattributed') WHEN 'available' THEN 0 ELSE 1 END,
		lower(COALESCE(a.masked_email, '')), d.upstream_account_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError("summarize upstream accounts", err)
	}
	defer rows.Close()
	result := make([]UpstreamAccountSummary, 0)
	for rows.Next() {
		var value UpstreamAccountSummary
		var accountID sql.NullString
		var lastSynced sql.NullTime
		if err := rows.Scan(&accountID, &value.MaskedEmail, &value.Plan, &value.Status,
			&lastSynced, &value.RequestCount, &value.ErrorCount, &value.InputTokens,
			&value.CachedInputTokens, &value.CacheWriteTokens, &value.OutputTokens,
			&value.ReasoningTokens, &value.EquivalentCostUSD); err != nil {
			return nil, fmt.Errorf("scan upstream account summary: %w", err)
		}
		value.AccountID = nullableString(accountID)
		if lastSynced.Valid {
			value.LastSyncedAt = &lastSynced.Time
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upstream account summaries: %w", err)
	}
	return result, nil
}
