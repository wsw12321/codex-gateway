package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type DeletableUser struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

type UserDeletionBlocker struct {
	UserID  string   `json:"user_id"`
	Reasons []string `json:"reasons"`
}

type UserDeletionBlockedError struct {
	Blockers []UserDeletionBlocker `json:"blockers"`
}

func (e *UserDeletionBlockedError) Error() string {
	return "selected users are no longer eligible for deletion"
}
func (e *UserDeletionBlockedError) Unwrap() error { return ErrConflict }

type DeleteInformationUsersParams struct {
	BillingWriteParams
	UserIDs []string
}

type DeleteInformationUsersResult struct {
	DeletedCount int      `json:"deleted_count"`
	UserIDs      []string `json:"user_ids"`
}

// Use the same predicates for the candidate list and the final transactional
// recheck. An automatically-created, empty billing account is not history.
const informationUserBlockers = `array_remove(ARRAY[
	CASE WHEN u.role <> 'member' THEN 'owner' END,
	CASE WHEN EXISTS(SELECT 1 FROM billing_accounts a WHERE a.user_id=u.id AND a.balance_usd<>0) THEN 'balance' END,
	CASE WHEN EXISTS(SELECT 1 FROM billing_subscriptions s WHERE s.user_id=u.id) THEN 'subscriptions' END,
	CASE WHEN EXISTS(SELECT 1 FROM usage_requests r WHERE r.user_id=u.id) THEN 'requests' END,
	CASE WHEN EXISTS(SELECT 1 FROM usage_daily d WHERE d.user_id=u.id)
		OR EXISTS(SELECT 1 FROM usage_monthly m WHERE m.user_id=u.id) THEN 'usage_summaries' END,
	CASE WHEN EXISTS(SELECT 1 FROM billing_ledger_entries l WHERE l.user_id=u.id OR l.actor_user_id=u.id) THEN 'ledger' END,
	CASE WHEN EXISTS(SELECT 1 FROM billing_operations o WHERE o.target_user_id=u.id OR o.actor_user_id=u.id) THEN 'billing_operations' END,
	CASE WHEN EXISTS(SELECT 1 FROM billing_cash_credit_lots c WHERE c.user_id=u.id) THEN 'cash_lots' END,
	CASE WHEN EXISTS(SELECT 1 FROM billing_reservations b WHERE b.user_id=u.id)
		OR EXISTS(SELECT 1 FROM quota_reservations q WHERE q.user_id=u.id) THEN 'reservations' END,
	CASE WHEN EXISTS(SELECT 1 FROM concurrency_leases c WHERE c.user_id=u.id) THEN 'running_requests' END,
	CASE WHEN EXISTS(SELECT 1 FROM group_operations g WHERE g.actor_user_id=u.id)
		OR EXISTS(SELECT 1 FROM billing_settings b WHERE b.updated_by_user_id=u.id) THEN 'management_history' END
]::text[],NULL)`

func (s *Store) ListDeletableInformationUsers(ctx context.Context, search string, limit, offset int) ([]DeletableUser, error) {
	search = strings.TrimSpace(search)
	if len([]rune(search)) > 200 || limit < 1 || limit > 100 || offset < 0 {
		return nil, fmt.Errorf("%w: invalid user candidate filter", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.username,u.display_name,u.status,u.created_at
		FROM users u WHERE cardinality(`+informationUserBlockers+`)=0
		AND ($1='' OR strpos(lower(u.username),lower($1))>0 OR strpos(lower(u.display_name),lower($1))>0
		OR u.id::text=$1) ORDER BY u.username,u.id LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		return nil, mapDBError("list deletable users", err)
	}
	defer rows.Close()
	users := make([]DeletableUser, 0)
	for rows.Next() {
		var user DeletableUser
		if err := rows.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Status, &user.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan deletable user: %w", err)
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func normalizeInformationUserIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, fmt.Errorf("%w: select 1-100 users", ErrInvalid)
	}
	result := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || parsed.String() != id || seen[id] {
			return nil, fmt.Errorf("%w: invalid or duplicate user id", ErrInvalid)
		}
		seen[id] = true
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) DeleteInformationUsers(ctx context.Context, params DeleteInformationUsersParams) (DeleteInformationUsersResult, error) {
	ids, err := normalizeInformationUserIDs(params.UserIDs)
	if err != nil {
		return DeleteInformationUsersResult{}, err
	}
	op, err := uuid.Parse(params.OperationID)
	if err != nil || op == uuid.Nil || op.String() != params.OperationID || params.ActorUserID == "" {
		return DeleteInformationUsersResult{}, fmt.Errorf("%w: invalid deletion operation", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	encoded, _ := json.Marshal(ids)
	fingerprint := billingOperationFingerprint("information.users.delete", params.ActorUserID, string(encoded))
	result := DeleteInformationUsersResult{DeletedCount: len(ids), UserIDs: ids}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockInformationMaintenanceTx(ctx, tx); err != nil {
			return err
		}
		var owner bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='owner' AND status='active')`, params.ActorUserID).Scan(&owner); err != nil {
			return err
		}
		if !owner {
			return fmt.Errorf("%w: active Owner required", ErrInvalid)
		}
		claim, err := tx.ExecContext(ctx, `INSERT INTO information_user_deletions
			(operation_id,actor_user_id_snapshot,request_fingerprint,user_ids,transaction_id,created_at)
			VALUES($1,$2,$3,$4::jsonb,txid_current(),$5) ON CONFLICT DO NOTHING`, params.OperationID, params.ActorUserID, fingerprint, string(encoded), params.At)
		if err != nil {
			return mapDBError("claim user deletion", err)
		}
		n, err := claim.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var previous []byte
			var completed sql.NullTime
			if err := tx.QueryRowContext(ctx, `SELECT request_fingerprint,completed_at FROM information_user_deletions WHERE operation_id=$1`, params.OperationID).Scan(&previous, &completed); err != nil {
				return err
			}
			if !completed.Valid || !bytes.Equal(previous, fingerprint) {
				return fmt.Errorf("%w: deletion operation replay mismatch", ErrConflict)
			}
			return nil
		}
		// Admission and settlement take global quota -> billing account locks.
		// Monetary operations also lock the account before inserting user FKs.
		// Follow that order, then lock principals to close credential races.
		if _, err := tx.ExecContext(ctx, `INSERT INTO quota_locks(scope_type,scope_id) VALUES('global','global') ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM quota_locks WHERE scope_type='global' AND scope_id='global' FOR UPDATE`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT user_id FROM billing_accounts WHERE user_id IN
			(SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)) ORDER BY user_id FOR UPDATE`, string(encoded)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT id FROM users WHERE id IN
			(SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)) ORDER BY id FOR UPDATE`, string(encoded)); err != nil {
			return err
		}
		blockers := make([]UserDeletionBlocker, 0)
		for _, id := range ids {
			var reasonsJSON []byte
			err := tx.QueryRowContext(ctx, `SELECT to_json(`+informationUserBlockers+`) FROM users u WHERE u.id=$1`, id).Scan(&reasonsJSON)
			if err == sql.ErrNoRows {
				blockers = append(blockers, UserDeletionBlocker{UserID: id, Reasons: []string{"not_found"}})
				continue
			}
			if err != nil {
				return mapDBError("recheck user deletion", err)
			}
			var reasons []string
			if err := json.Unmarshal(reasonsJSON, &reasons); err != nil {
				return err
			}
			if len(reasons) > 0 {
				blockers = append(blockers, UserDeletionBlocker{UserID: id, Reasons: reasons})
			}
		}
		if len(blockers) > 0 {
			return &UserDeletionBlockedError{Blockers: blockers}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE information_user_deletions SET approved_at=$2 WHERE operation_id=$1`, params.OperationID, params.At); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('gateway.user_deletion_operation',$1,true)`, params.OperationID); err != nil {
			return err
		}
		// Preserve audit identities before removing their live foreign keys.
		queries := []string{
			`UPDATE audit_events SET actor_user_id_snapshot=COALESCE(actor_user_id_snapshot,actor_user_id::text),
			actor_session_id_snapshot=COALESCE(actor_session_id_snapshot,actor_session_id::text),
			actor_api_key_id_snapshot=COALESCE(actor_api_key_id_snapshot,actor_api_key_id::text),
			actor_session_id=NULL,actor_api_key_id=NULL,actor_user_id=NULL WHERE actor_user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM invitations WHERE inviter_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)) OR target_user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)) OR used_by_user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`UPDATE model_access_defaults SET updated_by_user_id=NULL WHERE updated_by_user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`UPDATE user_model_access SET updated_by_user_id=NULL WHERE updated_by_user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM user_model_access WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM upstream_account_users WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM quota_counters WHERE (scope_type='user' AND scope_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))) OR (scope_type='key' AND scope_id IN (SELECT id FROM api_key_history WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))))`,
			`DELETE FROM quota_rate_windows WHERE (scope_type='user' AND scope_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))) OR (scope_type='key' AND scope_id IN (SELECT id FROM api_key_history WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))))`,
			`DELETE FROM quota_locks WHERE (scope_type='user' AND scope_id IN (SELECT value FROM jsonb_array_elements_text($1::jsonb))) OR (scope_type='key' AND scope_id IN (SELECT id::text FROM api_key_history WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))))`,
			`DELETE FROM api_keys WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM api_key_history WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM devices WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM projects WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM billing_accounts WHERE user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
			`DELETE FROM users WHERE id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))`,
		}
		for _, query := range queries {
			if _, err := tx.ExecContext(ctx, query, string(encoded)); err != nil {
				return mapDBError("delete user dependencies", err)
			}
		}
		auditIDs := make([]any, len(ids))
		for i, id := range ids {
			auditIDs[i] = id
		}
		if err := appendBillingAuditTx(ctx, tx, params.BillingWriteParams, "information.users.deleted", "information_operation", params.OperationID, map[string]any{"user_ids": auditIDs, "deleted_count": len(ids), "operation_id": params.OperationID}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE information_user_deletions SET completed_at=$2 WHERE operation_id=$1`, params.OperationID, params.At)
		return err
	})
	if err != nil {
		return DeleteInformationUsersResult{}, err
	}
	return result, nil
}
