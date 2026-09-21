package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type UpstreamAccountAccess struct {
	AccountID string   `json:"id"`
	Mode      string   `json:"access_mode"`
	UserIDs   []string `json:"authorized_user_ids"`
}

type SetUpstreamAccountAccessParams struct {
	AccountID      string
	Mode           string
	UserIDs        []string
	Reason         string
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

func normalizeUpstreamAccess(params SetUpstreamAccountAccessParams) (SetUpstreamAccountAccessParams, error) {
	params.Reason = strings.TrimSpace(params.Reason)
	if !upstreamAccountIDPattern.MatchString(params.AccountID) || params.ActorUserID == "" || params.Reason == "" || len([]rune(params.Reason)) > 500 ||
		(params.Mode != "shared" && params.Mode != "exclusive") || len(params.UserIDs) > 10000 ||
		(params.Mode == "shared" && len(params.UserIDs) != 0) || (params.Mode == "exclusive" && len(params.UserIDs) == 0) {
		return params, fmt.Errorf("%w: invalid upstream account access", ErrInvalid)
	}
	seen := make(map[string]bool, len(params.UserIDs))
	for _, id := range params.UserIDs {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id || seen[id] {
			return params, fmt.Errorf("%w: invalid or duplicate authorized user", ErrInvalid)
		}
		seen[id] = true
	}
	params.UserIDs = append([]string{}, params.UserIDs...)
	sort.Strings(params.UserIDs)
	return params, nil
}

func (s *Store) SetUpstreamAccountAccess(ctx context.Context, params SetUpstreamAccountAccessParams) (UpstreamAccountAccess, error) {
	params, err := normalizeUpstreamAccess(params)
	if err != nil {
		return UpstreamAccountAccess{}, err
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	}
	result := UpstreamAccountAccess{AccountID: params.AccountID, Mode: params.Mode, UserIDs: params.UserIDs}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account access edit", err)
		}
		var previousMode string
		if err := tx.QueryRowContext(ctx, `SELECT access_mode FROM upstream_accounts WHERE id=$1 FOR UPDATE`, params.AccountID).Scan(&previousMode); err != nil {
			return mapDBError("lock upstream account access", err)
		}
		var previousUsers string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(user_id::text ORDER BY user_id),'[]'::jsonb)::text FROM upstream_account_users WHERE upstream_account_id=$1`, params.AccountID).Scan(&previousUsers); err != nil {
			return mapDBError("read previous upstream users", err)
		}
		encoded, _ := json.Marshal(params.UserIDs)
		// Lock users in stable order, protecting membership validation from deletion.
		rows, err := tx.QueryContext(ctx, `SELECT id FROM users WHERE id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)) ORDER BY id FOR KEY SHARE`, string(encoded))
		if err != nil {
			return mapDBError("validate upstream authorized users", err)
		}
		count := 0
		for rows.Next() {
			count++
		}
		if err := rows.Close(); err != nil {
			return mapDBError("close upstream authorized users", err)
		}
		if err := rows.Err(); err != nil {
			return mapDBError("read upstream authorized users", err)
		}
		if count != len(params.UserIDs) {
			return fmt.Errorf("%w: authorized user does not exist", ErrInvalid)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE upstream_accounts SET access_mode=$2 WHERE id=$1`, params.AccountID, params.Mode); err != nil {
			return mapDBError("set upstream account access", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM upstream_account_users WHERE upstream_account_id=$1`, params.AccountID); err != nil {
			return mapDBError("replace upstream authorized users", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO upstream_account_users(upstream_account_id,user_id) SELECT $1,value::uuid FROM jsonb_array_elements_text($2::jsonb)`, params.AccountID, string(encoded)); err != nil {
			return mapDBError("set upstream authorized users", err)
		}
		var previousIDs []string
		if err := json.Unmarshal([]byte(previousUsers), &previousIDs); err != nil {
			return fmt.Errorf("decode previous upstream access: %w", err)
		}
		previousJSON, _ := json.Marshal(previousIDs)
		metadata, err := marshalSafeMetadata(map[string]any{"previous_mode": previousMode, "previous_user_count": len(previousIDs), "previous_users_sha256": fmt.Sprintf("%x", sha256.Sum256(previousJSON)), "mode": params.Mode, "user_count": len(params.UserIDs), "users_sha256": fmt.Sprintf("%x", sha256.Sum256(encoded)), "reason": params.Reason})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(occurred_at,actor_user_id,actor_session_id,event_type,severity,success,source_ip,subject_type,subject_id,request_id,metadata) VALUES($1,$2,$3,'upstream_account.access_changed','info',true,$4::inet,'upstream_account',$5,$6,$7::jsonb)`, params.At, params.ActorUserID, valueOrNil(params.ActorSessionID), valueOrNil(params.SourceIP), params.AccountID, valueOrNil(params.RequestID), metadata)
		return mapDBError("audit upstream account access", err)
	})
	if err != nil {
		return UpstreamAccountAccess{}, err
	}
	return result, nil
}

func (s *Store) ListUpstreamAccountAccess(ctx context.Context) ([]UpstreamAccountAccess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.access_mode,COALESCE(jsonb_agg(u.user_id::text ORDER BY u.user_id) FILTER (WHERE u.user_id IS NOT NULL),'[]'::jsonb)::text FROM upstream_accounts a LEFT JOIN upstream_account_users u ON u.upstream_account_id=a.id GROUP BY a.id ORDER BY a.id`)
	if err != nil {
		return nil, mapDBError("list upstream account access", err)
	}
	defer rows.Close()
	result := make([]UpstreamAccountAccess, 0)
	for rows.Next() {
		var item UpstreamAccountAccess
		var users string
		if err := rows.Scan(&item.AccountID, &item.Mode, &users); err != nil {
			return nil, fmt.Errorf("scan upstream account access: %w", err)
		}
		if err := json.Unmarshal([]byte(users), &item.UserIDs); err != nil {
			return nil, fmt.Errorf("decode upstream authorized users: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func upstreamCandidateArguments(userID string, ids []string) ([]any, []string, error) {
	parsed, err := uuid.Parse(userID)
	if err != nil || parsed.String() != userID || len(ids) == 0 || len(ids) > MaxUpstreamAllocationCandidates {
		return nil, nil, fmt.Errorf("%w: invalid upstream user or candidates", ErrInvalid)
	}
	args := []any{userID}
	values := make([]string, len(ids))
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if !upstreamAccountIDPattern.MatchString(id) || seen[id] {
			return nil, nil, fmt.Errorf("%w: invalid upstream candidates", ErrInvalid)
		}
		seen[id] = true
		args = append(args, id)
		values[i] = fmt.Sprintf("($%d::text)", i+2)
	}
	return args, values, nil
}

// Eligibility deliberately ignores allocation weight: a zero-weight account
// may retain an existing binding, but it must still authorize this user.
func (s *Store) EligibleUpstreamAccounts(ctx context.Context, userID string, ids []string) ([]string, error) {
	args, values, err := upstreamCandidateArguments(userID, ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH candidates(id) AS (VALUES `+strings.Join(values, ",")+`) SELECT c.id FROM candidates c LEFT JOIN upstream_accounts a ON a.id=c.id WHERE EXISTS(SELECT 1 FROM users WHERE id=$1::uuid AND status='active') AND (a.id IS NULL OR a.access_mode='shared' OR EXISTS(SELECT 1 FROM upstream_account_users u WHERE u.upstream_account_id=a.id AND u.user_id=$1::uuid)) ORDER BY c.id`, args...)
	if err != nil {
		return nil, mapDBError("read upstream account eligibility", err)
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan upstream eligibility: %w", err)
		}
		result = append(result, id)
	}
	return result, rows.Err()
}
