package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

type rowScanner interface {
	Scan(dest ...any) error
}

const userColumns = `id, username, display_name, webauthn_user_id, role, status,
	created_at, updated_at, disabled_at, last_login_at`

func scanUser(row rowScanner) (User, error) {
	var user User
	err := row.Scan(
		&user.ID, &user.Username, &user.DisplayName, &user.WebAuthnUserID, &user.Role, &user.Status,
		&user.CreatedAt, &user.UpdatedAt, &user.DisabledAt, &user.LastLoginAt,
	)
	return user, err
}

type CreateUserParams struct {
	ID             string
	Username       string
	DisplayName    string
	WebAuthnUserID []byte
	Role           string
}

func normalizeCreateUser(params CreateUserParams) (CreateUserParams, error) {
	params.Username = strings.ToLower(strings.TrimSpace(params.Username))
	params.DisplayName = strings.TrimSpace(params.DisplayName)
	if params.Username == "" {
		return params, fmt.Errorf("%w: username is empty", ErrInvalid)
	}
	if params.Role == "" {
		params.Role = UserRoleMember
	}
	if params.Role != UserRoleMember && params.Role != UserRoleOwner {
		return params, fmt.Errorf("%w: unknown user role", ErrInvalid)
	}
	if params.ID == "" {
		var err error
		params.ID, err = newUUID()
		if err != nil {
			return params, err
		}
	}
	if len(params.WebAuthnUserID) == 0 {
		var err error
		params.WebAuthnUserID, err = randomBytes(32)
		if err != nil {
			return params, err
		}
	}
	if len(params.WebAuthnUserID) != 32 {
		return params, fmt.Errorf("%w: WebAuthn user id must be 32 bytes", ErrInvalid)
	}
	return params, nil
}

func (s *Store) CreateUser(ctx context.Context, params CreateUserParams) (User, error) {
	params, err := normalizeCreateUser(params)
	if err != nil {
		return User{}, err
	}
	user, err := scanUser(s.db.QueryRowContext(ctx, `
		INSERT INTO users (id, username, display_name, webauthn_user_id, role)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+userColumns,
		params.ID, params.Username, params.DisplayName, params.WebAuthnUserID, params.Role,
	))
	return user, mapDBError("create user", err)
}

func (s *Store) GetUser(ctx context.Context, userID string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, userID,
	))
	return user, mapDBError("get user", err)
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = lower($1)`, strings.TrimSpace(username),
	))
	return user, mapDBError("get user by username", err)
}

func (s *Store) GetUserByWebAuthnID(ctx context.Context, webauthnUserID []byte) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE webauthn_user_id = $1`, webauthnUserID,
	))
	return user, mapDBError("get user by WebAuthn id", err)
}

func (s *Store) DisableUser(ctx context.Context, userID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET status = 'disabled', disabled_at = $2
		WHERE id = $1 AND status = 'active'`, userID, at,
	)
	if err != nil {
		return mapDBError("disable user", err)
	}
	return requireAffected("disable user", result)
}

func (s *Store) RecordUserLogin(ctx context.Context, userID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = $2 WHERE id = $1 AND status = 'active'`, userID, at,
	)
	if err != nil {
		return mapDBError("record user login", err)
	}
	return requireAffected("record user login", result)
}

const invitationColumns = `id, kind, token_hash, inviter_id, target_user_id, created_at, expires_at,
	used_at, used_by_user_id, revoked_at, host(source_ip)`

func scanInvitation(row rowScanner) (Invitation, error) {
	var invitation Invitation
	err := row.Scan(
		&invitation.ID, &invitation.Kind, &invitation.TokenHash, &invitation.InviterID,
		&invitation.TargetUserID, &invitation.CreatedAt, &invitation.ExpiresAt,
		&invitation.UsedAt, &invitation.UsedByUserID, &invitation.RevokedAt, &invitation.SourceIP,
	)
	return invitation, err
}

type CreateInvitationParams struct {
	ID           string
	Kind         string
	TokenHash    []byte
	InviterID    string
	TargetUserID string
	ExpiresAt    time.Time
	SourceIP     string
}

func (s *Store) CreateInvitation(ctx context.Context, params CreateInvitationParams) (Invitation, error) {
	if len(params.TokenHash) != 32 || params.ExpiresAt.IsZero() {
		return Invitation{}, fmt.Errorf("%w: invalid invitation hash or expiry", ErrInvalid)
	}
	if params.ID == "" {
		var err error
		params.ID, err = newUUID()
		if err != nil {
			return Invitation{}, err
		}
	}
	invitation, err := scanInvitation(s.db.QueryRowContext(ctx, `
		INSERT INTO invitations
			(id, kind, token_hash, inviter_id, target_user_id, expires_at, source_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7::inet)
		RETURNING `+invitationColumns,
		params.ID, params.Kind, params.TokenHash, valueOrNil(params.InviterID),
		valueOrNil(params.TargetUserID), params.ExpiresAt, valueOrNil(params.SourceIP),
	))
	return invitation, mapDBError("create invitation", err)
}

// GetAvailableInvitation is a read-only ceremony preflight. Security-sensitive
// completion must still use CreateUserFromInvitation or ConsumeInvitation,
// which re-check availability while holding a row lock.
func (s *Store) GetAvailableInvitation(ctx context.Context, tokenHash []byte, at time.Time) (Invitation, error) {
	if at.IsZero() {
		at = s.now().UTC()
	}
	invitation, err := scanInvitation(s.db.QueryRowContext(ctx, `
		SELECT `+invitationColumns+` FROM invitations
		WHERE token_hash = $1 AND used_at IS NULL AND revoked_at IS NULL
		  AND expires_at > $2`, tokenHash, at,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationUnavailable
	}
	return invitation, mapDBError("get available invitation", err)
}

// ConsumeInvitation is a single-use compare-and-set. For recovery invitations,
// usedByUserID must match the invitation target.
func (s *Store) ConsumeInvitation(ctx context.Context, tokenHash []byte, usedByUserID string, at time.Time) (Invitation, error) {
	invitation, err := scanInvitation(s.db.QueryRowContext(ctx, `
		UPDATE invitations
		SET used_at = $3, used_by_user_id = $2
		WHERE token_hash = $1
		  AND used_at IS NULL AND revoked_at IS NULL AND expires_at > $3
		  AND (target_user_id IS NULL OR target_user_id = $2)
		RETURNING `+invitationColumns,
		tokenHash, usedByUserID, at,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationUnavailable
	}
	return invitation, mapDBError("consume invitation", err)
}

// CreateUserFromInvitation atomically creates a member/owner and consumes the
// invitation. Recovery invitations intentionally use ConsumeInvitation.
func (s *Store) CreateUserFromInvitation(ctx context.Context, tokenHash []byte, params CreateUserParams) (User, error) {
	var created User
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		invitation, err := scanInvitation(tx.QueryRowContext(ctx, `
			SELECT `+invitationColumns+`
			FROM invitations WHERE token_hash = $1 FOR UPDATE`, tokenHash,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvitationUnavailable
		}
		if err != nil {
			return mapDBError("lock invitation", err)
		}
		now := s.now().UTC()
		if invitation.Kind == InvitationRecovery || invitation.UsedAt != nil ||
			invitation.RevokedAt != nil || !invitation.ExpiresAt.After(now) {
			return ErrInvitationUnavailable
		}
		if invitation.Kind == InvitationOwnerBootstrap {
			params.Role = UserRoleOwner
		} else {
			params.Role = UserRoleMember
		}
		params, err = normalizeCreateUser(params)
		if err != nil {
			return err
		}
		created, err = scanUser(tx.QueryRowContext(ctx, `
			INSERT INTO users (id, username, display_name, webauthn_user_id, role)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+userColumns,
			params.ID, params.Username, params.DisplayName, params.WebAuthnUserID, params.Role,
		))
		if err != nil {
			return mapDBError("create invited user", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE invitations SET used_at = $2, used_by_user_id = $3
			WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL`,
			invitation.ID, now, created.ID,
		)
		if err != nil {
			return mapDBError("consume invitation", err)
		}
		return requireAffected("consume invitation", result)
	})
	return created, err
}

func (s *Store) RevokeInvitation(ctx context.Context, invitationID, inviterID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE invitations SET revoked_at = $3
		WHERE id = $1 AND inviter_id = $2 AND used_at IS NULL AND revoked_at IS NULL`,
		invitationID, inviterID, at,
	)
	if err != nil {
		return mapDBError("revoke invitation", err)
	}
	return requireAffected("revoke invitation", result)
}

type AddWebAuthnCredentialParams struct {
	ID             string
	UserID         string
	CredentialID   []byte
	CredentialJSON []byte
	SignCount      uint64
	Transports     []string
	BackupEligible bool
	BackupState    bool
	Discoverable   bool
	AAGUID         string
	Nickname       string
}

const credentialColumns = `id, user_id, credential_id, credential_json, sign_count,
	to_json(transports)::text, backup_eligible, backup_state, discoverable,
	aaguid, nickname, created_at, last_used_at`

func scanCredential(row rowScanner) (WebAuthnCredential, error) {
	var credential WebAuthnCredential
	var transports []byte
	err := row.Scan(
		&credential.ID, &credential.UserID, &credential.CredentialID, &credential.CredentialJSON,
		&credential.SignCount, &transports, &credential.BackupEligible, &credential.BackupState,
		&credential.Discoverable, &credential.AAGUID, &credential.Nickname,
		&credential.CreatedAt, &credential.LastUsedAt,
	)
	if err == nil {
		err = unmarshalStringArray(transports, &credential.Transports)
	}
	return credential, err
}

func (s *Store) AddWebAuthnCredential(ctx context.Context, params AddWebAuthnCredentialParams) (WebAuthnCredential, error) {
	if len(params.CredentialID) == 0 || len(params.CredentialID) > 1023 ||
		len(params.CredentialJSON) == 0 || params.SignCount > math.MaxInt64 {
		return WebAuthnCredential{}, fmt.Errorf("%w: invalid WebAuthn credential", ErrInvalid)
	}
	if params.ID == "" {
		var err error
		params.ID, err = newUUID()
		if err != nil {
			return WebAuthnCredential{}, err
		}
	}
	transports, err := marshalStringArray(params.Transports)
	if err != nil {
		return WebAuthnCredential{}, err
	}
	credential, err := scanCredential(s.db.QueryRowContext(ctx, `
		INSERT INTO webauthn_credentials
			(id, user_id, credential_id, credential_json, sign_count, transports,
			 backup_eligible, backup_state, discoverable, aaguid, nickname)
		VALUES ($1, $2, $3, $4, $5,
			ARRAY(SELECT jsonb_array_elements_text($6::jsonb)), $7, $8, $9, $10, $11)
		RETURNING `+credentialColumns,
		params.ID, params.UserID, params.CredentialID, params.CredentialJSON, params.SignCount,
		transports, params.BackupEligible, params.BackupState, params.Discoverable,
		valueOrNil(params.AAGUID), params.Nickname,
	))
	return credential, mapDBError("add WebAuthn credential", err)
}

func (s *Store) GetWebAuthnCredential(ctx context.Context, credentialID []byte) (WebAuthnCredential, error) {
	credential, err := scanCredential(s.db.QueryRowContext(ctx, `
		SELECT `+credentialColumns+` FROM webauthn_credentials
		WHERE credential_id = $1`, credentialID,
	))
	return credential, mapDBError("get WebAuthn credential", err)
}

// UpdateWebAuthnCredential replaces the serialized, verified credential state.
// Counter replay protection remains available separately through
// UpdateWebAuthnCounter for callers that keep the counter in a dedicated field.
func (s *Store) UpdateWebAuthnCredential(ctx context.Context, credentialID, credentialJSON []byte) error {
	if len(credentialID) == 0 || len(credentialJSON) == 0 {
		return fmt.Errorf("%w: invalid WebAuthn credential update", ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE webauthn_credentials
		SET credential_json = $2, last_used_at = $3
		WHERE credential_id = $1`, credentialID, credentialJSON, s.now().UTC(),
	)
	if err != nil {
		return mapDBError("update WebAuthn credential", err)
	}
	return requireAffected("update WebAuthn credential", result)
}

func (s *Store) ListWebAuthnCredentials(ctx context.Context, userID string) ([]WebAuthnCredential, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+credentialColumns+` FROM webauthn_credentials
		WHERE user_id = $1 ORDER BY created_at`, userID,
	)
	if err != nil {
		return nil, mapDBError("list WebAuthn credentials", err)
	}
	defer rows.Close()
	var result []WebAuthnCredential
	for rows.Next() {
		credential, err := scanCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("scan WebAuthn credential: %w", err)
		}
		result = append(result, credential)
	}
	return result, rows.Err()
}

// UpdateWebAuthnCounter rejects a decreasing or repeated non-zero signature
// counter. Authenticators that permanently report zero remain supported.
func (s *Store) UpdateWebAuthnCounter(ctx context.Context, credentialID []byte, signCount uint64, usedAt time.Time) error {
	if signCount > math.MaxInt64 {
		return fmt.Errorf("%w: WebAuthn counter exceeds bigint", ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE webauthn_credentials
		SET sign_count = $2, last_used_at = $3
		WHERE credential_id = $1
		  AND ((sign_count = 0 AND $2 = 0) OR sign_count < $2)`,
		credentialID, signCount, usedAt,
	)
	if err != nil {
		return mapDBError("update WebAuthn counter", err)
	}
	return requireAffectedAs("update WebAuthn counter", result, ErrConflict)
}

func (s *Store) DeleteWebAuthnCredential(ctx context.Context, userID, credentialID string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2`, credentialID, userID,
	)
	if err != nil {
		return mapDBError("delete WebAuthn credential", err)
	}
	return requireAffected("delete WebAuthn credential", result)
}

// ReplaceRecoveryCodes invalidates an existing unused batch and inserts
// exactly ten pre-hashed codes. Plaintext recovery codes are never accepted.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes [][]byte) (string, error) {
	if len(hashes) != 10 {
		return "", fmt.Errorf("%w: exactly 10 recovery code hashes are required", ErrInvalid)
	}
	for _, hash := range hashes {
		if len(hash) != 32 {
			return "", fmt.Errorf("%w: recovery code hash must be 32 bytes", ErrInvalid)
		}
	}
	batchID, err := newUUID()
	if err != nil {
		return "", err
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID,
		); err != nil {
			return mapDBError("invalidate recovery codes", err)
		}
		for _, hash := range hashes {
			id, err := newUUID()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO recovery_codes (id, user_id, batch_id, code_hash)
				VALUES ($1, $2, $3, $4)`, id, userID, batchID, hash,
			); err != nil {
				return mapDBError("insert recovery code", err)
			}
		}
		return nil
	})
	return batchID, err
}

func (s *Store) ConsumeRecoveryCode(ctx context.Context, userID string, codeHash []byte, at time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE recovery_codes SET used_at = $3
		WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, codeHash, at,
	)
	if err != nil {
		return false, mapDBError("consume recovery code", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("consume recovery code rows affected: %w", err)
	}
	return n == 1, nil
}

type CreateSessionParams struct {
	ID                string
	UserID            string
	TokenHash         []byte
	CSRFSecret        []byte
	SourceIP          string
	UserAgentHash     []byte
	CreatedAt         time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

const sessionColumns = `id, user_id, token_hash, csrf_secret, host(source_ip), user_agent_hash,
	created_at, last_seen_at, idle_expires_at, absolute_expires_at,
	recently_verified_at, revoked_at, revoke_reason`

func scanSession(row rowScanner) (Session, error) {
	var session Session
	err := row.Scan(
		&session.ID, &session.UserID, &session.TokenHash, &session.CSRFSecret,
		&session.SourceIP, &session.UserAgentHash, &session.CreatedAt, &session.LastSeenAt,
		&session.IdleExpiresAt, &session.AbsoluteExpiresAt, &session.RecentlyVerifiedAt,
		&session.RevokedAt, &session.RevokeReason,
	)
	return session, err
}

func (s *Store) CreateSession(ctx context.Context, params CreateSessionParams) (Session, error) {
	if len(params.TokenHash) != 32 || len(params.CSRFSecret) < 32 ||
		(len(params.UserAgentHash) != 0 && len(params.UserAgentHash) != 32) {
		return Session{}, fmt.Errorf("%w: invalid session secret hash", ErrInvalid)
	}
	if params.CreatedAt.IsZero() {
		params.CreatedAt = s.now().UTC()
	}
	if !params.IdleExpiresAt.After(params.CreatedAt) ||
		params.IdleExpiresAt.After(params.CreatedAt.Add(12*time.Hour)) ||
		params.IdleExpiresAt.After(params.AbsoluteExpiresAt) ||
		params.AbsoluteExpiresAt.After(params.CreatedAt.Add(7*24*time.Hour)) {
		return Session{}, fmt.Errorf("%w: invalid session expiry", ErrInvalid)
	}
	if params.ID == "" {
		var err error
		params.ID, err = newUUID()
		if err != nil {
			return Session{}, err
		}
	}
	session, err := scanSession(s.db.QueryRowContext(ctx, `
		INSERT INTO sessions
			(id, user_id, token_hash, csrf_secret, source_ip, user_agent_hash,
			 created_at, last_seen_at, idle_expires_at, absolute_expires_at)
		VALUES ($1, $2, $3, $4, $5::inet, $6, $7, $7, $8, $9)
		RETURNING `+sessionColumns,
		params.ID, params.UserID, params.TokenHash, params.CSRFSecret,
		valueOrNil(params.SourceIP), valueOrNilBytes(params.UserAgentHash), params.CreatedAt,
		params.IdleExpiresAt, params.AbsoluteExpiresAt,
	))
	return session, mapDBError("create session", err)
}

func (s *Store) GetActiveSession(ctx context.Context, tokenHash []byte, at time.Time) (Session, error) {
	session, err := scanSession(s.db.QueryRowContext(ctx, `
		SELECT `+prefixColumns("s", sessionColumns)+`
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL
		  AND s.idle_expires_at > $2 AND s.absolute_expires_at > $2
		  AND u.status = 'active'`, tokenHash, at,
	))
	return session, mapDBError("get active session", err)
}

func (s *Store) TouchSession(ctx context.Context, sessionID string, at time.Time, idleTTL time.Duration) error {
	if idleTTL <= 0 || idleTTL > 12*time.Hour {
		return fmt.Errorf("%w: idle TTL must be within 12 hours", ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET last_seen_at = $2, idle_expires_at = LEAST(absolute_expires_at, $3)
		WHERE id = $1 AND revoked_at IS NULL
		  AND idle_expires_at > $2 AND absolute_expires_at > $2`,
		sessionID, at, at.Add(idleTTL),
	)
	if err != nil {
		return mapDBError("touch session", err)
	}
	return requireAffected("touch session", result)
}

func (s *Store) MarkSessionVerified(ctx context.Context, sessionID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET recently_verified_at = $2
		WHERE id = $1 AND revoked_at IS NULL
		  AND idle_expires_at > $2 AND absolute_expires_at > $2`, sessionID, at,
	)
	if err != nil {
		return mapDBError("mark session verified", err)
	}
	return requireAffected("mark session verified", result)
}

func (s *Store) RevokeSession(ctx context.Context, sessionID, reason string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = $3, revoke_reason = $2
		WHERE id = $1 AND revoked_at IS NULL`, sessionID, reason, at,
	)
	if err != nil {
		return mapDBError("revoke session", err)
	}
	return requireAffected("revoke session", result)
}

func (s *Store) RevokeUserSessions(ctx context.Context, userID, reason string, at time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = $3, revoke_reason = $2
		WHERE user_id = $1 AND revoked_at IS NULL`, userID, reason, at,
	)
	if err != nil {
		return 0, mapDBError("revoke user sessions", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions rows affected: %w", err)
	}
	return n, nil
}

func requireAffected(operation string, result sql.Result) error {
	return requireAffectedAs(operation, result, ErrNotFound)
}

func requireAffectedAs(operation string, result sql.Result, sentinel error) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", operation, sentinel)
	}
	return nil
}

func valueOrNilBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

// prefixColumns qualifies a fixed, internal column list. It is never called
// with user input.
func prefixColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "(") {
			// host(source_ip) is the only expression in current scan lists.
			part = strings.Replace(part, "source_ip", alias+".source_ip", 1)
			parts[i] = part
			continue
		}
		parts[i] = alias + "." + part
	}
	return strings.Join(parts, ", ")
}
