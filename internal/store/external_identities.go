package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ExternalIdentity is local binding metadata, never a provider credential.
// HTTP responses must use an explicit allowlist rather than serialize this type.
type ExternalIdentity struct {
	ID          string
	UserID      string
	Issuer      string
	Subject     string
	MaskedEmail string
	LinkedAt    time.Time
	UnlinkedAt  *time.Time
}

type LinkExternalIdentityParams struct {
	UserID      string
	SessionID   string
	Issuer      string
	Subject     string
	MaskedEmail string
	At          time.Time
	// Optional original ceremony deadline. A later local reauthentication must
	// not extend an already-started binding confirmation while it waits on locks.
	VerificationExpiresAt time.Time
}

type CompleteExternalLoginParams struct {
	Issuer  string
	Subject string
	Session CreateSessionParams
	At      time.Time
}

const externalIdentityColumns = `id, user_id, issuer, subject, masked_email, linked_at, unlinked_at`

func scanExternalIdentity(row rowScanner) (ExternalIdentity, error) {
	var identity ExternalIdentity
	err := row.Scan(&identity.ID, &identity.UserID, &identity.Issuer, &identity.Subject,
		&identity.MaskedEmail, &identity.LinkedAt, &identity.UnlinkedAt)
	return identity, err
}

func validateExternalIdentity(issuer, subject, maskedEmail string) error {
	// Issuer and subject are exact, case-sensitive identifiers. Never normalize
	// them or fall back to email/username matching.
	for _, value := range []struct {
		text string
		min  int
		max  int
	}{{issuer, 1, 2048}, {subject, 1, 255}, {maskedEmail, 0, 320}} {
		if !utf8.ValidString(value.text) || strings.ContainsRune(value.text, '\x00') ||
			utf8.RuneCountInString(value.text) < value.min || utf8.RuneCountInString(value.text) > value.max {
			return fmt.Errorf("%w: invalid external identity metadata", ErrInvalid)
		}
	}
	return nil
}

func (s *Store) GetExternalIdentity(ctx context.Context, userID string) (ExternalIdentity, error) {
	identity, err := scanExternalIdentity(s.db.QueryRowContext(ctx,
		`SELECT `+externalIdentityColumns+` FROM external_identities WHERE user_id=$1 AND unlinked_at IS NULL`, userID))
	return identity, mapDBError("get external identity", err)
}

// externalIdentityTime rechecks freshness after waiting for transaction locks.
// A request timestamp must not extend the five-minute authorization window.
func (s *Store) externalIdentityTime(at time.Time) time.Time {
	now := s.now().UTC()
	if at.After(now) {
		return at.UTC()
	}
	return now
}

func lockExternalIdentityUser(ctx context.Context, tx *sql.Tx, userID string) (User, error) {
	user, err := scanUser(tx.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id=$1 AND status='active' FOR UPDATE`, userID))
	return user, mapDBError("lock external identity user", err)
}

func (s *Store) lockExternalIdentitySession(ctx context.Context, tx *sql.Tx, userID, sessionID string, at time.Time) (time.Time, error) {
	session, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+
		` FROM sessions WHERE id=$1 AND user_id=$2 FOR UPDATE`, sessionID, userID))
	if err != nil {
		return time.Time{}, mapDBError("lock external identity session", err)
	}
	at = s.externalIdentityTime(at)
	if session.RevokedAt != nil || !session.IdleExpiresAt.After(at) || !session.AbsoluteExpiresAt.After(at) ||
		session.RecentlyVerifiedAt == nil || !session.RecentlyVerifiedAt.After(at.Add(-5*time.Minute)) || session.RecentlyVerifiedAt.After(at) {
		return time.Time{}, fmt.Errorf("%w: recent local session verification required", ErrNotFound)
	}
	return at, nil
}

// LinkExternalIdentity, CompleteExternalLogin and UnlinkExternalIdentity always
// lock user -> binding -> session. The user lock prevents a login from issuing
// a session after unlink has collected and revoked the binding's sessions.
func (s *Store) LinkExternalIdentity(ctx context.Context, params LinkExternalIdentityParams) (ExternalIdentity, error) {
	if err := validateExternalIdentity(params.Issuer, params.Subject, params.MaskedEmail); err != nil {
		return ExternalIdentity{}, err
	}
	var identity ExternalIdentity
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := lockExternalIdentityUser(ctx, tx, params.UserID); err != nil {
			return err
		}
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id FROM external_identities WHERE user_id=$1 AND unlinked_at IS NULL FOR UPDATE`, params.UserID).Scan(&existing)
		if err == nil {
			return fmt.Errorf("%w: user already has an external identity", ErrConflict)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapDBError("lock current external identity", err)
		}
		params.At, err = s.lockExternalIdentitySession(ctx, tx, params.UserID, params.SessionID, params.At)
		if err != nil {
			return err
		}
		if !params.VerificationExpiresAt.IsZero() && !params.VerificationExpiresAt.After(params.At) {
			return fmt.Errorf("%w: external identity confirmation expired", ErrNotFound)
		}
		identity, err = scanExternalIdentity(tx.QueryRowContext(ctx, `INSERT INTO external_identities
			(user_id, issuer, subject, masked_email, linked_at) VALUES ($1,$2,$3,$4,$5)
			RETURNING `+externalIdentityColumns, params.UserID, params.Issuer, params.Subject, params.MaskedEmail, params.At))
		return mapDBError("link external identity", err)
	})
	if err != nil {
		return ExternalIdentity{}, err
	}
	return identity, nil
}

// CompleteExternalLogin only resolves an existing exact issuer/subject binding;
// it never creates, merges or modifies a user or a local login credential.
func (s *Store) CompleteExternalLogin(ctx context.Context, params CompleteExternalLoginParams) (User, Session, error) {
	if err := validateExternalIdentity(params.Issuer, params.Subject, ""); err != nil {
		return User{}, Session{}, err
	}
	var user User
	var session Session
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Read a lock-order hint, then recheck this exact binding after locking
		// its user. Unlink/relink must not redirect a login to a new binding ID.
		var identityID, userID string
		if err := tx.QueryRowContext(ctx, `SELECT id, user_id FROM external_identities
			WHERE issuer=$1 AND subject=$2 AND unlinked_at IS NULL`, params.Issuer, params.Subject).Scan(&identityID, &userID); err != nil {
			return mapDBError("find external login binding", err)
		}
		var err error
		user, err = lockExternalIdentityUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM external_identities
			WHERE id=$1 AND user_id=$2 AND issuer=$3 AND subject=$4 AND unlinked_at IS NULL FOR UPDATE`,
			identityID, user.ID, params.Issuer, params.Subject).Scan(&locked); err != nil {
			return mapDBError("lock external login binding", err)
		}
		params.At = s.externalIdentityTime(params.At)
		if _, err := tx.ExecContext(ctx, `UPDATE users SET last_login_at=$2 WHERE id=$1`, user.ID, params.At); err != nil {
			return mapDBError("record external login", err)
		}
		user.LastLoginAt = &params.At
		params.Session.UserID = user.ID
		if params.Session.CreatedAt.IsZero() {
			params.Session.CreatedAt = params.At
		}
		// External authentication never grants local recent verification.
		session, err = insertSessionWithSource(ctx, tx, params.Session, false, identityID)
		return err
	})
	if err != nil {
		return User{}, Session{}, err
	}
	return user, session, nil
}

func (s *Store) UnlinkExternalIdentity(ctx context.Context, userID, sessionID string, at time.Time) (ExternalIdentity, error) {
	var identity ExternalIdentity
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := lockExternalIdentityUser(ctx, tx, userID); err != nil {
			return err
		}
		var err error
		identity, err = scanExternalIdentity(tx.QueryRowContext(ctx, `SELECT `+externalIdentityColumns+
			` FROM external_identities WHERE user_id=$1 AND unlinked_at IS NULL FOR UPDATE`, userID))
		if err != nil {
			return mapDBError("lock external identity to unlink", err)
		}
		at, err = s.lockExternalIdentitySession(ctx, tx, userID, sessionID, at)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET unlinked_at=$2 WHERE id=$1`, identity.ID, at); err != nil {
			return mapDBError("unlink external identity", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=$2, revoke_reason='external_identity_unlinked'
			WHERE external_identity_id=$1 AND revoked_at IS NULL`, identity.ID, at); err != nil {
			return mapDBError("revoke external identity sessions", err)
		}
		identity.UnlinkedAt = &at
		return nil
	})
	if err != nil {
		return ExternalIdentity{}, err
	}
	return identity, nil
}
