package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PostgreSQL INTEGER is signed 32-bit. Keep the application bound in sync
// with the database type so an accepted value can always be persisted.
const MaxUpstreamConcurrentLimit = 1<<31 - 1

type UpstreamAccountEligibility struct {
	ID              string
	ConcurrentLimit int
}

type SetUpstreamAccountConcurrentLimitParams struct {
	AccountID      string
	Limit          int
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

// SetUpstreamAccountConcurrentLimit updates a local admission preference.
// Metadata synchronization intentionally does not overwrite this value.
func (s *Store) SetUpstreamAccountConcurrentLimit(ctx context.Context, params SetUpstreamAccountConcurrentLimitParams) (UpstreamAccount, error) {
	var result UpstreamAccount
	if !upstreamAccountIDPattern.MatchString(params.AccountID) ||
		params.Limit < 1 || params.Limit > MaxUpstreamConcurrentLimit ||
		strings.TrimSpace(params.ActorUserID) == "" {
		return result, fmt.Errorf("%w: invalid concurrent limit, account, or actor", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Synchronization takes a table lock before its row writes. Use the same
		// lock ordering as the other local upstream preferences.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account concurrent limit edit", err)
		}
		var previousLimit int
		if err := tx.QueryRowContext(ctx, `SELECT concurrent_limit FROM upstream_accounts
			WHERE id = $1 FOR UPDATE`, params.AccountID).Scan(&previousLimit); err != nil {
			return mapDBError("lock upstream account concurrent limit", err)
		}
		var err error
		result, err = scanUpstreamAccount(tx.QueryRowContext(ctx, `UPDATE upstream_accounts
			SET concurrent_limit = $2 WHERE id = $1 RETURNING `+upstreamAccountColumns,
			params.AccountID, params.Limit))
		if err != nil {
			return mapDBError("set upstream account concurrent limit", err)
		}
		metadata, err := marshalSafeMetadata(map[string]any{
			"previous_limit": previousLimit, "limit": params.Limit,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
				success, source_ip, subject_type, subject_id, request_id, metadata)
			VALUES ($1,$2,$3,'upstream_account.concurrent_limit_changed','info',true,
				$4::inet,'upstream_account',$5,$6,$7::jsonb)`,
			params.At, params.ActorUserID, valueOrNil(params.ActorSessionID),
			valueOrNil(params.SourceIP), params.AccountID, valueOrNil(params.RequestID), metadata)
		return mapDBError("audit upstream account concurrent limit", err)
	})
	if err != nil {
		return UpstreamAccount{}, err
	}
	return result, nil
}

// EligibleUpstreamAccountLimits returns the accounts authorized for userID and
// their Gateway-owned admission limits. Unknown accounts retain the default
// limit of one until synchronized metadata is persisted.
func (s *Store) EligibleUpstreamAccountLimits(ctx context.Context, userID string, ids []string) ([]UpstreamAccountEligibility, error) {
	args, values, err := upstreamCandidateArguments(userID, ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH candidates(id) AS (VALUES `+strings.Join(values, ",")+
		`) SELECT c.id, COALESCE(a.concurrent_limit, 1) FROM candidates c
		LEFT JOIN upstream_accounts a ON a.id=c.id
		WHERE EXISTS(SELECT 1 FROM users WHERE id=$1::uuid AND status='active')
		AND (a.id IS NULL OR a.access_mode='shared' OR EXISTS(
			SELECT 1 FROM upstream_account_users u WHERE u.upstream_account_id=a.id AND u.user_id=$1::uuid))
		ORDER BY c.id`, args...)
	if err != nil {
		return nil, mapDBError("read upstream account eligibility limits", err)
	}
	defer rows.Close()
	result := make([]UpstreamAccountEligibility, 0)
	for rows.Next() {
		var item UpstreamAccountEligibility
		if err := rows.Scan(&item.ID, &item.ConcurrentLimit); err != nil {
			return nil, fmt.Errorf("scan upstream eligibility limit: %w", err)
		}
		if item.ConcurrentLimit < 1 || item.ConcurrentLimit > MaxUpstreamConcurrentLimit {
			return nil, fmt.Errorf("%w: invalid upstream concurrent limit", ErrInvalid)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
