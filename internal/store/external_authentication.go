package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func externalVerificationTime(verifiedAt, at time.Time) (time.Time, error) {
	return validateExternalVerificationTime(verifiedAt, at, 5*time.Minute)
}

// The first-login choice can use the remaining ten-minute ceremony lifetime.
// Persisting its original proof keeps sensitive operations' five-minute window
// expired when the user spends longer choosing how to open their account.
func externalLoginVerificationTime(verifiedAt, at time.Time) (time.Time, error) {
	return validateExternalVerificationTime(verifiedAt, at, 10*time.Minute)
}

func validateExternalVerificationTime(verifiedAt, at time.Time, maxAge time.Duration) (time.Time, error) {
	verifiedAt = verifiedAt.UTC().Truncate(time.Microsecond)
	if verifiedAt.IsZero() || verifiedAt.After(at) || !verifiedAt.After(at.Add(-maxAge)) {
		return time.Time{}, fmt.Errorf("%w: external verification expired", ErrNotFound)
	}
	return verifiedAt, nil
}

func setExternalLoginVerification(ctx context.Context, tx *sql.Tx, session *Session, verifiedAt, at time.Time) error {
	verifiedAt, err := externalLoginVerificationTime(verifiedAt, at)
	if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `UPDATE sessions SET recently_verified_at=$2 WHERE id=$1
		RETURNING recently_verified_at`, session.ID, verifiedAt).Scan(&verifiedAt); err != nil {
		return mapDBError("set external login verification", err)
	}
	session.RecentlyVerifiedAt = &verifiedAt
	return nil
}

// CompleteExternalRegistration deliberately accepts no username, role, group,
// balance or local credential. Existing user-insert triggers supply the normal
// model defaults and empty billing account. Unique binding indexes arbitrate
// registration/link races; every artifact rolls back when either insert fails.
func (s *Store) CompleteExternalRegistration(ctx context.Context, params CompleteExternalRegistrationParams) (User, Session, error) {
	if err := validateExternalIdentity(params.Issuer, params.Subject, params.MaskedEmail); err != nil {
		return User{}, Session{}, err
	}
	id, err := newUUID()
	if err != nil {
		return User{}, Session{}, err
	}
	userParams, err := normalizeCreateUser(CreateUserParams{
		ID: id, Username: "water5_" + strings.ReplaceAll(id, "-", ""), DisplayName: "吾水阁用户",
	})
	if err != nil {
		return User{}, Session{}, err
	}
	var user User
	var session Session
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		at := s.externalIdentityTime(params.At)
		verifiedAt, err := externalLoginVerificationTime(params.VerifiedAt, at)
		if err != nil {
			return err
		}
		user, err = scanUser(tx.QueryRowContext(ctx, `INSERT INTO users
			(id, username, display_name, webauthn_user_id, role, status, last_login_at)
			VALUES ($1,$2,$3,$4,'member','active',$5) RETURNING `+userColumns,
			userParams.ID, userParams.Username, userParams.DisplayName, userParams.WebAuthnUserID, at))
		if err != nil {
			return mapDBError("create external user", err)
		}
		var identityID string
		if err := tx.QueryRowContext(ctx, `INSERT INTO external_identities
			(user_id, issuer, subject, masked_email, linked_at) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			user.ID, params.Issuer, params.Subject, params.MaskedEmail, at).Scan(&identityID); err != nil {
			return mapDBError("bind external registration", err)
		}
		params.Session.UserID = user.ID
		if params.Session.CreatedAt.IsZero() {
			params.Session.CreatedAt = at
		}
		session, err = insertSessionWithSource(ctx, tx, params.Session, false, identityID)
		if err != nil {
			return err
		}
		return setExternalLoginVerification(ctx, tx, &session, verifiedAt, s.externalIdentityTime(at))
	})
	if err != nil {
		return User{}, Session{}, err
	}
	return user, session, nil
}

// CompleteExternalReauthentication authorizes only the original active user,
// exact binding and original session. It preserves both user and provenance.
// Unlink takes the same user -> binding -> session locks, so either it rejects
// this proof or clears it before committing. The returned timestamp identifies
// this write for conditional cancellation; stale/equal proofs never overwrite
// a newer proof (including PostgreSQL's microsecond timestamp collisions).
func (s *Store) CompleteExternalReauthentication(ctx context.Context, params CompleteExternalReauthenticationParams) (time.Time, error) {
	if err := validateExternalIdentity(params.Issuer, params.Subject, ""); err != nil {
		return time.Time{}, err
	}
	var verifiedAt time.Time
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := lockExternalIdentityUser(ctx, tx, params.UserID); err != nil {
			return err
		}
		var identityID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM external_identities WHERE id=$1 AND user_id=$2
			AND issuer=$3 AND subject=$4 AND unlinked_at IS NULL FOR UPDATE`,
			params.ExternalIdentityID, params.UserID, params.Issuer, params.Subject).Scan(&identityID); err != nil {
			return mapDBError("lock external reauthentication binding", err)
		}
		session, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+
			` FROM sessions WHERE id=$1 AND user_id=$2 FOR UPDATE`, params.SessionID, params.UserID))
		if err != nil {
			return mapDBError("lock external reauthentication session", err)
		}
		at := s.externalIdentityTime(params.At)
		if session.RevokedAt != nil || !session.IdleExpiresAt.After(at) || !session.AbsoluteExpiresAt.After(at) {
			return ErrNotFound
		}
		verifiedAt, err = externalVerificationTime(params.VerifiedAt, at)
		if err != nil {
			return err
		}
		if session.RecentlyVerifiedAt != nil && !verifiedAt.After(*session.RecentlyVerifiedAt) {
			return fmt.Errorf("%w: newer session verification already exists", ErrConflict)
		}
		if err := tx.QueryRowContext(ctx, `UPDATE sessions SET recently_verified_at=$2 WHERE id=$1
			RETURNING recently_verified_at`, session.ID, verifiedAt).Scan(&verifiedAt); err != nil {
			return mapDBError("verify external session", err)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return verifiedAt, nil
}

// ClearSessionVerificationIfCurrent is an idempotent rollback for an abandoned
// handoff. It neither clears a later proof nor revokes a local login session.
func (s *Store) ClearSessionVerificationIfCurrent(ctx context.Context, userID, sessionID string, verifiedAt time.Time) error {
	if verifiedAt.IsZero() {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET recently_verified_at=NULL
		WHERE id=$1 AND user_id=$2 AND recently_verified_at=$3`, sessionID, userID, verifiedAt)
	return mapDBError("cancel external session verification", err)
}
