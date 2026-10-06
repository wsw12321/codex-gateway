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

type UserUpstreamAccess struct {
	UserID     string   `json:"user_id"`
	Provider   string   `json:"provider"`
	Mode       string   `json:"mode"`
	AccountIDs []string `json:"account_ids"`
}

type SetUserUpstreamAccessParams struct {
	UserID         string
	Mode           string
	AccountIDs     []string
	Reason         string
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

func normalizeUserUpstreamAccess(params SetUserUpstreamAccessParams) (SetUserUpstreamAccessParams, error) {
	params.Reason = strings.TrimSpace(params.Reason)
	userID, err := uuid.Parse(params.UserID)
	if err != nil || userID.String() != params.UserID || strings.TrimSpace(params.ActorUserID) == "" ||
		params.Reason == "" || len([]rune(params.Reason)) > 500 ||
		(params.Mode != "all" && params.Mode != "selected") || len(params.AccountIDs) > 10000 ||
		(params.Mode == "all" && len(params.AccountIDs) != 0) {
		return params, fmt.Errorf("%w: invalid user upstream access", ErrInvalid)
	}
	seen := make(map[string]bool, len(params.AccountIDs))
	for _, id := range params.AccountIDs {
		if !upstreamAccountIDPattern.MatchString(id) || seen[id] {
			return params, fmt.Errorf("%w: invalid or duplicate upstream account", ErrInvalid)
		}
		seen[id] = true
	}
	params.AccountIDs = append([]string{}, params.AccountIDs...)
	sort.Strings(params.AccountIDs)
	return params, nil
}

// GetUserUpstreamAccess uses one snapshot for the rule and its members. Missing
// rules mean all accounts, including accounts registered after this read.
func (s *Store) GetUserUpstreamAccess(ctx context.Context, userID string) (UserUpstreamAccess, error) {
	parsed, err := uuid.Parse(userID)
	if !s.validUpstreamProvider() || err != nil || parsed.String() != userID {
		return UserUpstreamAccess{}, fmt.Errorf("%w: invalid upstream provider or user", ErrInvalid)
	}
	var status, accountIDs string
	result := UserUpstreamAccess{UserID: userID, Provider: s.upstreamProviderName()}
	err = s.db.QueryRowContext(ctx, `SELECT u.status,COALESCE(a.mode,'all'),
		COALESCE((SELECT jsonb_agg(m.upstream_account_id ORDER BY m.upstream_account_id)
			FROM user_upstream_access_accounts m WHERE m.user_id=u.id AND m.provider=$2),'[]'::jsonb)::text
		FROM users u LEFT JOIN user_upstream_access a ON a.user_id=u.id AND a.provider=$2
		WHERE u.id=$1`, userID, result.Provider).Scan(&status, &result.Mode, &accountIDs)
	if err != nil {
		return UserUpstreamAccess{}, mapDBError("get user upstream access", err)
	}
	if status == "pending" {
		return UserUpstreamAccess{}, fmt.Errorf("%w: user registration is awaiting approval", ErrConflict)
	}
	if err := json.Unmarshal([]byte(accountIDs), &result.AccountIDs); err != nil {
		return UserUpstreamAccess{}, fmt.Errorf("decode user upstream accounts: %w", err)
	}
	return result, nil
}

// SetUserUpstreamAccess atomically replaces one provider's scope and audit.
// Upstream accounts are locked before the user, matching account access edits;
// the user lock serializes concurrent edits even when no rule exists yet.
func (s *Store) SetUserUpstreamAccess(ctx context.Context, params SetUserUpstreamAccessParams) (UserUpstreamAccess, error) {
	if !s.validUpstreamProvider() {
		return UserUpstreamAccess{}, fmt.Errorf("%w: invalid upstream provider", ErrInvalid)
	}
	params, err := normalizeUserUpstreamAccess(params)
	if err != nil {
		return UserUpstreamAccess{}, err
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	result := UserUpstreamAccess{UserID: params.UserID, Provider: s.upstreamProviderName(), Mode: params.Mode, AccountIDs: params.AccountIDs}
	encoded, _ := json.Marshal(params.AccountIDs)
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Metadata synchronization takes the stronger table lock first.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock user upstream access edit", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM upstream_accounts
			WHERE provider=$1 AND id IN (SELECT value FROM jsonb_array_elements_text($2::jsonb))
			ORDER BY id FOR UPDATE`, result.Provider, string(encoded))
		if err != nil {
			return mapDBError("validate user upstream accounts", err)
		}
		count := 0
		for rows.Next() {
			count++
		}
		if err := rows.Close(); err != nil {
			return mapDBError("close user upstream accounts", err)
		}
		if err := rows.Err(); err != nil {
			return mapDBError("read user upstream accounts", err)
		}
		if count != len(params.AccountIDs) {
			return fmt.Errorf("%w: upstream account does not exist for this provider", ErrInvalid)
		}
		if err := lockNonPendingUserTx(ctx, tx, params.UserID); err != nil {
			return err
		}
		var previousMode, previousAccounts string
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT mode FROM user_upstream_access WHERE user_id=$1 AND provider=$2),'all'),
			COALESCE((SELECT jsonb_agg(upstream_account_id ORDER BY upstream_account_id)
				FROM user_upstream_access_accounts WHERE user_id=$1 AND provider=$2),'[]'::jsonb)::text`,
			params.UserID, result.Provider).Scan(&previousMode, &previousAccounts); err != nil {
			return mapDBError("read previous user upstream access", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_upstream_access(user_id,provider,mode,updated_at)
			VALUES($1,$2,$3,$4) ON CONFLICT(user_id,provider) DO UPDATE SET mode=EXCLUDED.mode,updated_at=EXCLUDED.updated_at`,
			params.UserID, result.Provider, params.Mode, params.At); err != nil {
			return mapDBError("set user upstream access", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_upstream_access_accounts WHERE user_id=$1 AND provider=$2`, params.UserID, result.Provider); err != nil {
			return mapDBError("replace user upstream accounts", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_upstream_access_accounts(user_id,provider,upstream_account_id)
			SELECT $1,$2,value FROM jsonb_array_elements_text($3::jsonb)`, params.UserID, result.Provider, string(encoded)); err != nil {
			return mapDBError("set user upstream accounts", err)
		}
		var previousIDs []string
		if err := json.Unmarshal([]byte(previousAccounts), &previousIDs); err != nil {
			return fmt.Errorf("decode previous user upstream access: %w", err)
		}
		previousJSON, _ := json.Marshal(previousIDs)
		metadata, err := marshalSafeMetadata(map[string]any{
			"provider": result.Provider, "previous_mode": previousMode,
			"previous_account_count": len(previousIDs), "previous_accounts_sha256": fmt.Sprintf("%x", sha256.Sum256(previousJSON)),
			"mode": params.Mode, "account_count": len(params.AccountIDs),
			"accounts_sha256": fmt.Sprintf("%x", sha256.Sum256(encoded)), "reason": params.Reason,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at,actor_user_id,actor_session_id,event_type,severity,success,source_ip,subject_type,subject_id,request_id,metadata)
			VALUES($1,$2,$3,'user.upstream_access_changed','info',true,$4::inet,'user',$5,$6,$7::jsonb)`,
			params.At, params.ActorUserID, valueOrNil(params.ActorSessionID), valueOrNil(params.SourceIP), params.UserID, valueOrNil(params.RequestID), metadata)
		return mapDBError("audit user upstream access", err)
	})
	if err != nil {
		return UserUpstreamAccess{}, err
	}
	return result, nil
}

// Keep eligibility, concurrency admission, and fresh allocation identical.
// accountSQL refers to the registered account row, so selected mode also denies
// candidates whose metadata has never been synchronized or registered.
func userUpstreamAccessPredicate(userArg, providerArg, accountSQL string) string {
	return `(COALESCE((SELECT ua.mode FROM user_upstream_access ua
		WHERE ua.user_id=` + userArg + `::uuid AND ua.provider=` + providerArg + `),'all')='all'
		OR EXISTS(SELECT 1 FROM user_upstream_access_accounts um
			WHERE um.user_id=` + userArg + `::uuid AND um.provider=` + providerArg + ` AND um.upstream_account_id=` + accountSQL + `))`
}
