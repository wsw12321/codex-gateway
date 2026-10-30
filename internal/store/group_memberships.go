package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// GroupMembershipPreference is the user-controlled priority of a group in
// the user's ordered group funding list.
type GroupMembershipPreference struct {
	GroupID  string `json:"group_id"`
	UserID   string `json:"user_id"`
	Priority int    `json:"priority"`
}

type SetGroupMembershipPreferenceParams struct {
	BillingWriteParams
	GroupID  string
	UserID   string
	Priority int
}

// SetGroupMembershipPriority is intentionally owner-only: ActorUserID must be
// the same user whose membership is being changed. The operation is idempotent
// through group_operations and remains usable when a member belongs to many
// groups.
func (s *Store) SetGroupMembershipPriority(ctx context.Context, p SetGroupMembershipPreferenceParams) (GroupMembershipPreference, error) {
	var result GroupMembershipPreference
	if err := validateBillingWrite(p.BillingWriteParams); err != nil {
		return result, err
	}
	gid, err := canonicalGroupID(strings.TrimSpace(p.GroupID))
	if err != nil {
		return result, err
	}
	uid, err := canonicalGroupID(strings.TrimSpace(p.UserID))
	if err != nil {
		return result, err
	}
	if p.ActorUserID != uid || p.Priority < 1 || p.Priority > 100 {
		return result, fmt.Errorf("%w: membership priority requires the member and a value between 1 and 100", ErrInvalid)
	}
	p.GroupID, p.UserID = gid, uid
	p.At = normalizedBillingTime(p.At, s.now)
	fingerprint := billingOperationFingerprint("membership_priority", gid, uid, fmt.Sprint(p.Priority), strings.TrimSpace(p.Reason))
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, uid); err != nil {
			return err
		}
		if err := lockGroupTx(ctx, tx, gid); err != nil {
			return err
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO group_operations(operation_id,actor_user_id,action,request_fingerprint,created_at)
			VALUES($1,$2,'membership_priority',$3,$4) ON CONFLICT DO NOTHING`, p.OperationID, p.ActorUserID, fingerprint, p.At)
		if err != nil { return mapDBError("claim membership priority operation", err) }
		n, _ := inserted.RowsAffected()
		if n == 0 {
			var actor, action string; var stored, encoded []byte
			if err := tx.QueryRowContext(ctx, `SELECT actor_user_id,action,request_fingerprint,response FROM group_operations WHERE operation_id=$1 FOR UPDATE`, p.OperationID).Scan(&actor, &action, &stored, &encoded); err != nil { return mapDBError("read membership priority operation", err) }
			if actor != p.ActorUserID || action != "membership_priority" || string(stored) != string(fingerprint) || len(encoded) == 0 { return fmt.Errorf("%w: group operation replay mismatch", ErrConflict) }
			return json.Unmarshal(encoded, &result)
		}
		var archived sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT archived_at FROM user_groups WHERE id=$1`, gid).Scan(&archived); err != nil { return mapDBError("read group status", err) }
		if archived.Valid { return fmt.Errorf("%w: group is archived", ErrConflict) }
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_memberships WHERE group_id=$1 AND user_id=$2)`, gid, uid).Scan(&exists); err != nil { return mapDBError("check group membership", err) }
		if !exists { return fmt.Errorf("%w: group membership not found", ErrNotFound) }
		if _, err := tx.ExecContext(ctx, `UPDATE group_memberships SET priority=$3,updated_at=$4 WHERE group_id=$1 AND user_id=$2`, gid, uid, p.Priority, p.At); err != nil {
			return mapDBError("save membership priority", err)
		}
		result = GroupMembershipPreference{GroupID: gid, UserID: uid, Priority: p.Priority}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE group_operations SET response=$2::jsonb WHERE operation_id=$1`, p.OperationID, string(encoded)); err != nil {
			return mapDBError("finish membership priority operation", err)
		}
		return appendBillingAuditTx(ctx, tx, p.BillingWriteParams, "group.membership_priority", "group_membership", gid,
			map[string]any{"user_id": uid, "priority": p.Priority, "operation_id": p.OperationID})
	})
	return result, err
}

// ListUserGroupMemberships returns memberships in deterministic admission
// order: ascending priority, then group UUID.
func (s *Store) ListUserGroupMemberships(ctx context.Context, userID string) ([]GroupMembershipPreference, error) {
	uid, err := canonicalGroupID(strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT group_id::text,user_id::text,priority
		FROM group_memberships WHERE user_id=$1 ORDER BY priority,group_id`, uid)
	if err != nil {
		return nil, mapDBError("list group memberships", err)
	}
	defer rows.Close()
	result := make([]GroupMembershipPreference, 0)
	for rows.Next() {
		var item GroupMembershipPreference
		if err := rows.Scan(&item.GroupID, &item.UserID, &item.Priority); err != nil {
			return nil, fmt.Errorf("scan group membership: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// Compatibility aliases for callers that use billing-oriented naming.
type SetBillingGroupPreferenceParams = SetGroupMembershipPreferenceParams
type SetGroupMembershipPreferencesParams = SetGroupMembershipPreferenceParams

func (s *Store) SetGroupMembershipPreferences(ctx context.Context, p SetGroupMembershipPreferencesParams) (GroupMembershipPreference, error) {
	return s.SetGroupMembershipPriority(ctx, p)
}
