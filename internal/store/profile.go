package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrUsernameTaken               = fmt.Errorf("%w: username taken", ErrConflict)
	ErrProfileVerificationRequired = errors.New("store: profile requires recent identity verification")
)

type UpdateUserProfileParams struct {
	UserID             string
	SessionID          string
	Username           *string
	DisplayName        *string
	VerificationMaxAge time.Duration
	SourceIP           string
	RequestID          string
}

// UpdateUserProfile keeps ownership anchored to the user ID. The row update and
// its audit event commit together; a name conflict cannot partially save either
// field. The current session is checked again after acquiring the user lock.
func (s *Store) UpdateUserProfile(ctx context.Context, params UpdateUserProfileParams) (User, error) {
	if params.UserID == "" || params.SessionID == "" || (params.Username == nil && params.DisplayName == nil) {
		return User{}, ErrInvalid
	}
	if params.VerificationMaxAge <= 0 {
		params.VerificationMaxAge = 5 * time.Minute
	}
	var user User
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		previous, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=$1 AND status='active' FOR UPDATE`, params.UserID))
		if err != nil {
			return mapDBError("lock profile user", err)
		}
		session, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id=$1 AND user_id=$2 FOR UPDATE`, params.SessionID, params.UserID))
		if err != nil {
			return mapDBError("lock profile session", err)
		}
		checkAuthorization := func(now time.Time) error {
			if session.RevokedAt != nil || !session.IdleExpiresAt.After(now) || !session.AbsoluteExpiresAt.After(now) {
				return ErrNotFound
			}
			if params.Username != nil && (session.RecentlyVerifiedAt == nil || session.RecentlyVerifiedAt.After(now) || !session.RecentlyVerifiedAt.Add(params.VerificationMaxAge).After(now)) {
				return ErrProfileVerificationRequired
			}
			return nil
		}
		now := s.now().UTC()
		if err := checkAuthorization(now); err != nil {
			return err
		}
		username, displayName := previous.Username, previous.DisplayName
		if params.Username != nil {
			username = *params.Username
		}
		if params.DisplayName != nil {
			displayName = *params.DisplayName
		}
		user, err = scanUser(tx.QueryRowContext(ctx, `UPDATE users SET username=$2, display_name=$3, updated_at=$4 WHERE id=$1 RETURNING `+userColumns,
			params.UserID, username, displayName, now))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_username_lower_key" {
				return ErrUsernameTaken
			}
			return mapDBError("update profile", err)
		}
		metadata, err := marshalSafeMetadata(map[string]any{"username_changed": username != previous.Username, "display_name_changed": displayName != previous.DisplayName})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at,actor_user_id,actor_session_id,event_type,severity,success,source_ip,subject_type,subject_id,request_id,metadata)
			VALUES ($1,$2,$3,'identity.profile_updated','info',true,$4::inet,'user',$5,$6,$7::jsonb)`,
			now, user.ID, session.ID, valueOrNil(params.SourceIP), user.ID, valueOrNil(params.RequestID), metadata)
		if err != nil {
			return mapDBError("audit profile update", err)
		}
		// Index contention or audit writes may wait after the session lock.
		// Those waits must not extend the session or verification window.
		return checkAuthorization(s.now().UTC())
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}
