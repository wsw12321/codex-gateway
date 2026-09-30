package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

const applicationColumns = `id,invitation_id,user_id,username,display_name,registered_at,applied_at,status,reviewed_at,reviewed_by`

func scanInvitationApplication(row rowScanner) (InvitationApplication, error) {
	var a InvitationApplication
	err := row.Scan(&a.ID, &a.InvitationID, &a.UserID, &a.Username, &a.DisplayName, &a.RegisteredAt, &a.AppliedAt, &a.Status, &a.ReviewedAt, &a.ReviewedBy)
	return a, err
}

// Existing identities are locked in the same order as accounting and deletion:
// all accounts, then all users, with each class sorted by ID. NO KEY UPDATE
// preserves concurrent FK references to an actor (group operations insert their
// audit actor before taking member account locks). Pending status changes and
// deletion upgrade their target row lock as needed; pending identities cannot
// be business-operation actors. Missing users are allowed only for replayable
// review operations after a prior rejection.
func lockInvitationUsersTx(ctx context.Context, tx *sql.Tx, ids []string, allowMissing bool) (map[string]User, error) {
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	unique := ids[:0]
	for _, id := range ids {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	for _, id := range unique {
		var locked string
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM billing_accounts WHERE user_id=$1 FOR UPDATE`, id).Scan(&locked)
		if allowMissing && errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, mapDBError("lock invitation billing account", err)
		}
	}
	users := make(map[string]User, len(unique))
	for _, id := range unique {
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=$1 FOR NO KEY UPDATE`, id))
		if allowMissing && errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, mapDBError("lock invitation user", err)
		}
		users[id] = u
	}
	return users, nil
}

func lockActiveInvitationGroupTx(ctx context.Context, tx *sql.Tx, id string) error {
	var archived *time.Time
	err := tx.QueryRowContext(ctx, `SELECT archived_at FROM user_groups WHERE id=$1 FOR UPDATE`, id).Scan(&archived)
	if err != nil {
		return mapDBError("lock invitation group", err)
	}
	if archived != nil {
		return fmt.Errorf("%w: group is archived", ErrConflict)
	}
	return nil
}

func invitationCapacityTx(ctx context.Context, tx *sql.Tx, invitation Invitation, at time.Time) error {
	if invitation.UsedAt != nil || invitation.RevokedAt != nil || !invitation.ExpiresAt.After(at) {
		return ErrInvitationUnavailable
	}
	// A separate statement is essential after a contended FOR UPDATE: a subquery
	// in the locking statement could still observe its pre-lock MVCC snapshot.
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invitation_applications WHERE invitation_id=$1`, invitation.ID).Scan(&count); err != nil {
		return mapDBError("count invitation uses", err)
	}
	if count >= invitation.MaxUses {
		return ErrInvitationUnavailable
	}
	return nil
}

func lockRegistrationInvitationTx(ctx context.Context, tx *sql.Tx, hash []byte, at time.Time) (Invitation, error) {
	i, err := scanInvitation(tx.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE token_hash=$1 FOR UPDATE`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrInvitationUnavailable
	}
	if err != nil {
		return i, mapDBError("lock registration invitation", err)
	}
	if i.Kind != InvitationMember && i.Kind != InvitationOwnerBootstrap {
		return i, ErrInvitationUnavailable
	}
	return i, invitationCapacityTx(ctx, tx, i, at)
}

func createInvitedUserTx(ctx context.Context, tx *sql.Tx, i Invitation, params CreateUserParams) (User, error) {
	params.Role = UserRoleMember
	if i.Kind == InvitationOwnerBootstrap {
		params.Role = UserRoleOwner
	}
	params, err := normalizeCreateUser(params)
	if err != nil {
		return User{}, err
	}
	status := StatusActive
	if i.RequiresApproval {
		status = StatusPending
	}
	user, err := scanUser(tx.QueryRowContext(ctx, `INSERT INTO users(id,username,display_name,webauthn_user_id,role,status)
  VALUES($1,$2,$3,$4,$5,$6) RETURNING `+userColumns, params.ID, params.Username, params.DisplayName, params.WebAuthnUserID, params.Role, status))
	return user, mapDBError("create invited user", err)
}

func insertInvitationApplicationTx(ctx context.Context, tx *sql.Tx, i Invitation, u User, at time.Time) (InvitationApplication, error) {
	status := "approved"
	var reviewed any = at
	if i.RequiresApproval {
		status = "pending"
		reviewed = nil
	}
	a, err := scanInvitationApplication(tx.QueryRowContext(ctx, `INSERT INTO invitation_applications
  (invitation_id,user_id,username,display_name,registered_at,applied_at,status,reviewed_at)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+applicationColumns,
		i.ID, u.ID, u.Username, u.DisplayName, u.CreatedAt, at, status, reviewed))
	return a, mapDBError("create invitation application", err)
}

func recordRegistrationApplicationTx(ctx context.Context, tx *sql.Tx, i Invitation, u User, at time.Time) error {
	if i.Kind == InvitationMember {
		_, err := insertInvitationApplicationTx(ctx, tx, i, u, at)
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE invitations SET used_at=$2,used_by_user_id=$3
  WHERE id=$1 AND used_at IS NULL AND revoked_at IS NULL`, i.ID, at, u.ID)
	if err != nil {
		return mapDBError("consume setup invitation", err)
	}
	return requireAffected("consume setup invitation", result)
}

func invitationPage(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return 0, 0, ErrInvalid
	}
	return limit, offset, nil
}

func (s *Store) ListInvitations(ctx context.Context, kind, groupID string, limit, offset int) ([]Invitation, error) {
	limit, offset, err := invitationPage(limit, offset)
	if err != nil {
		return nil, err
	}
	if kind != "" && kind != InvitationMember && kind != InvitationGroup && kind != InvitationRecovery && kind != InvitationOwnerBootstrap {
		return nil, ErrInvalid
	}
	if groupID != "" {
		groupID, err = canonicalGroupID(groupID)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+invitationColumns+` FROM invitations
  WHERE ($1='' OR kind=$1) AND ($2::uuid IS NULL OR group_id=$2::uuid)
  ORDER BY created_at DESC,id LIMIT $3 OFFSET $4`, kind, valueOrNil(groupID), limit, offset)
	if err != nil {
		return nil, mapDBError("list invitations", err)
	}
	defer rows.Close()
	result := make([]Invitation, 0)
	for rows.Next() {
		i, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, i)
	}
	return result, rows.Err()
}

func (s *Store) ListInvitationApplications(ctx context.Context, invitationID string, limit, offset int) ([]InvitationApplication, error) {
	limit, offset, err := invitationPage(limit, offset)
	if err != nil {
		return nil, err
	}
	invitationID, err = canonicalGroupID(invitationID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+applicationColumns+` FROM invitation_applications
  WHERE invitation_id=$1 ORDER BY applied_at DESC,id LIMIT $2 OFFSET $3`, invitationID, limit, offset)
	if err != nil {
		return nil, mapDBError("list invitation applications", err)
	}
	defer rows.Close()
	result := make([]InvitationApplication, 0)
	for rows.Next() {
		a, err := scanInvitationApplication(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// InspectInvitation discloses only metadata to a caller holding the token.
// Existing applicants may revisit a full, expired or revoked group invitation;
// JoinInvitation remains responsible for deciding whether a new use is allowed.
func (s *Store) InspectInvitation(ctx context.Context, hash []byte, _ time.Time) (Invitation, error) {
	invitation, err := scanInvitation(s.db.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE token_hash=$1`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationUnavailable
	}
	return invitation, mapDBError("inspect invitation", err)
}

func (s *Store) JoinInvitation(ctx context.Context, hash []byte, userID string) (InvitationApplication, error) {
	var result InvitationApplication
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		users, err := lockInvitationUsersTx(ctx, tx, []string{userID}, false)
		if err != nil {
			return err
		}
		user := users[userID]
		if user.Status != StatusActive {
			return ErrConflict
		}
		i, err := scanInvitation(tx.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE token_hash=$1 FOR UPDATE`, hash))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvitationUnavailable
		}
		if err != nil {
			return mapDBError("lock group invitation", err)
		}
		if i.Kind != InvitationGroup || i.GroupID == nil {
			return ErrInvitationUnavailable
		}
		previous, err := scanInvitationApplication(tx.QueryRowContext(ctx, `SELECT `+applicationColumns+` FROM invitation_applications WHERE invitation_id=$1 AND user_id=$2 FOR UPDATE`, i.ID, userID))
		if err == nil {
			result = previous
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapDBError("read previous group application", err)
		}
		var current *string
		if err := tx.QueryRowContext(ctx, `SELECT group_id FROM billing_accounts WHERE user_id=$1`, userID).Scan(&current); err != nil {
			return mapDBError("read current group", err)
		}
		if current != nil {
			if *current != *i.GroupID {
				return fmt.Errorf("%w: user already belongs to another group", ErrConflict)
			}
			result = InvitationApplication{InvitationID: i.ID, UserID: &user.ID, Username: user.Username, DisplayName: user.DisplayName, RegisteredAt: user.CreatedAt, Status: "approved"}
			return nil
		}
		now := s.now().UTC()
		if err := invitationCapacityTx(ctx, tx, i, now); err != nil {
			return err
		}
		if err := lockActiveInvitationGroupTx(ctx, tx, *i.GroupID); err != nil {
			return err
		}
		result, err = insertInvitationApplicationTx(ctx, tx, i, user, now)
		if err != nil {
			return err
		}
		if !i.RequiresApproval {
			_, err = tx.ExecContext(ctx, `UPDATE billing_accounts SET group_id=$2 WHERE user_id=$1`, userID, *i.GroupID)
			if err != nil {
				return mapDBError("join invitation group", err)
			}
		}
		// Record only a newly accepted application. Replays return above, and
		// audit metadata never retains the applicant's profile after rejection.
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
  (occurred_at,event_type,severity,success,subject_type,subject_id,metadata)
  VALUES($1,'invitation.applied','info',true,'invitation',$2,'{"count":1}'::jsonb)`, now, i.ID)
		return mapDBError("audit group invitation application", err)
	})
	return result, err
}

// ReviewInvitationApplications addresses immutable application IDs. A rejected
// application is deleted, so a delayed request cannot affect a later reapply.
func (s *Store) ReviewInvitationApplications(ctx context.Context, invitationID string, applicationIDs []string, decision, actorID string, at time.Time) (int, error) {
	if (decision != "approve" && decision != "reject") || len(applicationIDs) < 1 || len(applicationIDs) > 100 {
		return 0, ErrInvalid
	}
	invitationID, err := canonicalGroupID(invitationID)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(applicationIDs))
	seen := map[string]bool{}
	for _, id := range applicationIDs {
		id, err = canonicalGroupID(id)
		if err != nil {
			return 0, err
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	sort.Strings(ids)
	encoded, err := marshalStringArray(ids)
	if err != nil {
		return 0, err
	}
	if at.IsZero() {
		at = s.now().UTC()
	}
	changed := 0
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Catalog synchronization holds a stronger users table lock while it
		// adds permission defaults. Acquire the write lock before deleting
		// defaults so rejection cannot invert that table/permission ordering.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE users IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock review identities table", err)
		}
		// Read IDs before locks; re-read applications below after all identity locks.
		rows, err := tx.QueryContext(ctx, `SELECT user_id FROM invitation_applications WHERE invitation_id=$1
   AND id IN (SELECT jsonb_array_elements_text($2::jsonb)::uuid) AND user_id IS NOT NULL`, invitationID, encoded)
		if err != nil {
			return mapDBError("read review identities", err)
		}
		userIDs := []string{}
		// Approval writes reviewed_by after locking the group. Lock the actor
		// first as well, so its FK check cannot wait on an invitation creator
		// holding the actor row while waiting for that same group.
		if actorID != "" {
			userIDs = append(userIDs, actorID)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			userIDs = append(userIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		users, err := lockInvitationUsersTx(ctx, tx, userIDs, true)
		if err != nil {
			return err
		}
		if actorID != "" && users[actorID].Status != StatusActive {
			return ErrConflict
		}
		i, err := scanInvitation(tx.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE id=$1 FOR UPDATE`, invitationID))
		if err != nil {
			return mapDBError("lock reviewed invitation", err)
		}
		if i.Kind != InvitationMember && i.Kind != InvitationGroup {
			return ErrInvalid
		}
		rows, err = tx.QueryContext(ctx, `SELECT `+applicationColumns+` FROM invitation_applications WHERE invitation_id=$1
   AND id IN (SELECT jsonb_array_elements_text($2::jsonb)::uuid) ORDER BY id FOR UPDATE`, invitationID, encoded)
		if err != nil {
			return mapDBError("lock reviewed applications", err)
		}
		applications := []InvitationApplication{}
		for rows.Next() {
			a, err := scanInvitationApplication(rows)
			if err != nil {
				rows.Close()
				return err
			}
			applications = append(applications, a)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if decision == "approve" && len(applications) != len(ids) {
			return fmt.Errorf("%w: application no longer exists", ErrConflict)
		}
		pending := false
		for _, a := range applications {
			if a.Status == "pending" {
				pending = true
			}
		}
		if pending && decision == "approve" && i.Kind == InvitationGroup {
			if i.GroupID == nil {
				return ErrConflict
			}
			if err := lockActiveInvitationGroupTx(ctx, tx, *i.GroupID); err != nil {
				return err
			}
		}
		for _, a := range applications {
			if a.Status == "approved" {
				if decision == "reject" {
					return fmt.Errorf("%w: approved application cannot be rejected", ErrConflict)
				}
				continue
			}
			if a.UserID == nil {
				return ErrConflict
			}
			user, exists := users[*a.UserID]
			if !exists {
				return ErrConflict
			}
			if decision == "reject" {
				if _, err := tx.ExecContext(ctx, `DELETE FROM invitation_applications WHERE id=$1`, a.ID); err != nil {
					return mapDBError("reject invitation application", err)
				}
				if i.Kind == InvitationMember {
					if err := deletePendingInvitationUserTx(ctx, tx, user); err != nil {
						return err
					}
				}
			} else {
				if i.Kind == InvitationMember {
					if user.Status != StatusPending {
						return ErrConflict
					}
					if _, err := tx.ExecContext(ctx, `UPDATE users SET status='active' WHERE id=$1`, user.ID); err != nil {
						return mapDBError("approve registration", err)
					}
				} else {
					if user.Status != StatusActive {
						return ErrConflict
					}
					var current *string
					if err := tx.QueryRowContext(ctx, `SELECT group_id FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&current); err != nil {
						return mapDBError("read application group", err)
					}
					if current != nil && *current != *i.GroupID {
						return fmt.Errorf("%w: user already belongs to another group", ErrConflict)
					}
					if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET group_id=$2 WHERE user_id=$1`, user.ID, *i.GroupID); err != nil {
						return mapDBError("approve group membership", err)
					}
				}
				if _, err := tx.ExecContext(ctx, `UPDATE invitation_applications SET status='approved',reviewed_at=$2,reviewed_by=$3 WHERE id=$1`, a.ID, at, valueOrNil(actorID)); err != nil {
					return mapDBError("approve invitation application", err)
				}
			}
			changed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

func deletePendingInvitationUserTx(ctx context.Context, tx *sql.Tx, user User) error {
	if user.Status != StatusPending || user.Role != UserRoleMember {
		return ErrConflict
	}
	// Registration creates only credentials, recovery codes, default model
	// permissions and an empty account. Foreign keys fail closed if unexpected
	// business data exists; the whole rejection (including its slot) rolls back.
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_model_access WHERE user_id=$1`, user.ID); err != nil {
		return mapDBError("delete pending model defaults", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM billing_accounts WHERE user_id=$1 AND balance_usd=0 AND group_id IS NULL`, user.ID)
	if err != nil {
		return mapDBError("delete pending billing account", err)
	}
	if err := requireAffected("delete pending billing account", result); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id=$1 AND status='pending'`, user.ID)
	if err != nil {
		return mapDBError("delete pending registration", err)
	}
	return requireAffected("delete pending registration", result)
}
