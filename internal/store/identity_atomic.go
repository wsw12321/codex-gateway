package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// IdentityCredential carries exactly one newly verified login credential.
// EncodedPasswordHash and WebAuthnCredential are mutually exclusive.
type IdentityCredential struct {
	EncodedPasswordHash string
	WebAuthnCredential  *AddWebAuthnCredentialParams
}

type CompleteInvitationParams struct {
	InvitationHash []byte
	User           CreateUserParams
	Credential     IdentityCredential
	RecoveryHashes [][]byte
	Session        CreateSessionParams
}

type CompleteRecoveryParams struct {
	UserID         string
	RecoveryHash   []byte
	InvitationHash []byte
	Credential     IdentityCredential
	RecoveryHashes [][]byte
	Session        CreateSessionParams
	At             time.Time
}

func validateIdentityCredential(value IdentityCredential) error {
	password := value.EncodedPasswordHash != ""
	passkey := value.WebAuthnCredential != nil
	if password == passkey || (password && (len(value.EncodedPasswordHash) < 64 || len(value.EncodedPasswordHash) > 512)) {
		return fmt.Errorf("%w: exactly one valid identity credential is required", ErrInvalid)
	}
	return nil
}

func validateRecoveryHashes(hashes [][]byte) error {
	if len(hashes) != 10 {
		return fmt.Errorf("%w: exactly 10 recovery code hashes are required", ErrInvalid)
	}
	for _, hash := range hashes {
		if len(hash) != 32 {
			return fmt.Errorf("%w: recovery code hash must be 32 bytes", ErrInvalid)
		}
	}
	return nil
}

func insertPasswordCredential(ctx context.Context, tx *sql.Tx, userID, encoded string, at time.Time, upsert bool) error {
	query := `INSERT INTO password_credentials (user_id, encoded_hash, created_at, updated_at)
		VALUES ($1, $2, $3, $3)`
	if upsert {
		query += ` ON CONFLICT (user_id) DO UPDATE SET encoded_hash = EXCLUDED.encoded_hash,
			updated_at = EXCLUDED.updated_at`
	}
	_, err := tx.ExecContext(ctx, query, userID, encoded, at)
	return mapDBError("write password credential", err)
}

func insertWebAuthnCredential(ctx context.Context, tx *sql.Tx, params AddWebAuthnCredentialParams) (WebAuthnCredential, error) {
	if len(params.CredentialID) == 0 || len(params.CredentialID) > 1023 || len(params.CredentialJSON) == 0 || params.SignCount > mathMaxInt64 {
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
	credential, err := scanCredential(tx.QueryRowContext(ctx, `
		INSERT INTO webauthn_credentials
			(id, user_id, credential_id, credential_json, sign_count, transports,
			 backup_eligible, backup_state, discoverable, aaguid, nickname)
		VALUES ($1, $2, $3, $4, $5,
			ARRAY(SELECT jsonb_array_elements_text($6::jsonb)), $7, $8, $9, $10, $11)
		RETURNING `+credentialColumns,
		params.ID, params.UserID, params.CredentialID, params.CredentialJSON, params.SignCount,
		transports, params.BackupEligible, params.BackupState, params.Discoverable,
		valueOrNil(params.AAGUID), params.Nickname))
	return credential, mapDBError("add WebAuthn credential", err)
}

const mathMaxInt64 = uint64(1<<63 - 1)

func insertRecoveryCodes(ctx context.Context, tx *sql.Tx, userID string, hashes [][]byte) error {
	if err := validateRecoveryHashes(hashes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID); err != nil {
		return mapDBError("invalidate recovery codes", err)
	}
	batchID, err := newUUID()
	if err != nil {
		return err
	}
	for _, hash := range hashes {
		id, err := newUUID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (id, user_id, batch_id, code_hash) VALUES ($1,$2,$3,$4)`, id, userID, batchID, hash); err != nil {
			return mapDBError("insert recovery code", err)
		}
	}
	return nil
}

func insertSession(ctx context.Context, tx *sql.Tx, params CreateSessionParams, recentlyVerified bool) (Session, error) {
	if len(params.TokenHash) != 32 || len(params.CSRFSecret) < 32 || (len(params.UserAgentHash) != 0 && len(params.UserAgentHash) != 32) {
		return Session{}, fmt.Errorf("%w: invalid session secret hash", ErrInvalid)
	}
	if params.CreatedAt.IsZero() || !params.IdleExpiresAt.After(params.CreatedAt) ||
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
	verified := any(nil)
	if recentlyVerified {
		verified = params.CreatedAt
	}
	session, err := scanSession(tx.QueryRowContext(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, csrf_secret, source_ip, user_agent_hash,
			created_at, last_seen_at, idle_expires_at, absolute_expires_at, recently_verified_at)
		VALUES ($1,$2,$3,$4,$5::inet,$6,$7,$7,$8,$9,$10)
		RETURNING `+sessionColumns, params.ID, params.UserID, params.TokenHash, params.CSRFSecret,
		valueOrNil(params.SourceIP), valueOrNilBytes(params.UserAgentHash), params.CreatedAt,
		params.IdleExpiresAt, params.AbsoluteExpiresAt, verified))
	return session, mapDBError("create session", err)
}

func writeIdentityCredential(ctx context.Context, tx *sql.Tx, userID string, credential IdentityCredential, at time.Time, upsert bool) error {
	if err := validateIdentityCredential(credential); err != nil {
		return err
	}
	if credential.EncodedPasswordHash != "" {
		return insertPasswordCredential(ctx, tx, userID, credential.EncodedPasswordHash, at, upsert)
	}
	value := *credential.WebAuthnCredential
	value.UserID = userID
	_, err := insertWebAuthnCredential(ctx, tx, value)
	return err
}

// CompleteInvitationRegistration creates every durable identity artifact and
// consumes the invitation in one transaction.
func (s *Store) CompleteInvitationRegistration(ctx context.Context, params CompleteInvitationParams) (User, Session, error) {
	var user User
	var session Session
	err := validateIdentityCredential(params.Credential)
	if err != nil {
		return user, session, err
	}
	if err := validateRecoveryHashes(params.RecoveryHashes); err != nil {
		return user, session, err
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		invitation, err := scanInvitation(tx.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE token_hash=$1 FOR UPDATE`, params.InvitationHash))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvitationUnavailable
		}
		if err != nil {
			return mapDBError("lock invitation", err)
		}
		now := s.now().UTC()
		if invitation.Kind == InvitationRecovery || invitation.UsedAt != nil || invitation.RevokedAt != nil || !invitation.ExpiresAt.After(now) {
			return ErrInvitationUnavailable
		}
		if invitation.Kind == InvitationOwnerBootstrap {
			params.User.Role = UserRoleOwner
		} else {
			params.User.Role = UserRoleMember
		}
		params.User, err = normalizeCreateUser(params.User)
		if err != nil {
			return err
		}
		user, err = scanUser(tx.QueryRowContext(ctx, `INSERT INTO users (id,username,display_name,webauthn_user_id,role) VALUES ($1,$2,$3,$4,$5) RETURNING `+userColumns,
			params.User.ID, params.User.Username, params.User.DisplayName, params.User.WebAuthnUserID, params.User.Role))
		if err != nil {
			return mapDBError("create invited user", err)
		}
		if err := writeIdentityCredential(ctx, tx, user.ID, params.Credential, now, false); err != nil {
			return err
		}
		if err := insertRecoveryCodes(ctx, tx, user.ID, params.RecoveryHashes); err != nil {
			return err
		}
		params.Session.UserID = user.ID
		session, err = insertSession(ctx, tx, params.Session, false)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE invitations SET used_at=$2,used_by_user_id=$3 WHERE id=$1 AND used_at IS NULL AND revoked_at IS NULL`, invitation.ID, now, user.ID)
		if err != nil {
			return mapDBError("consume invitation", err)
		}
		return requireAffected("consume invitation", result)
	})
	return user, session, err
}

// CompleteAccountRecovery consumes the authorization only after all new
// identity state has been written successfully. Existing unselected login
// methods are intentionally retained.
func (s *Store) CompleteAccountRecovery(ctx context.Context, params CompleteRecoveryParams) (User, Session, error) {
	var user User
	var session Session
	if err := validateIdentityCredential(params.Credential); err != nil {
		return user, session, err
	}
	if err := validateRecoveryHashes(params.RecoveryHashes); err != nil {
		return user, session, err
	}
	if (len(params.RecoveryHash) == 32) == (len(params.InvitationHash) == 32) {
		return user, session, fmt.Errorf("%w: exactly one recovery authorization is required", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	}
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var err error
		if len(params.InvitationHash) == 32 {
			invitation, scanErr := scanInvitation(tx.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE token_hash=$1 FOR UPDATE`, params.InvitationHash))
			if errors.Is(scanErr, sql.ErrNoRows) {
				return ErrInvitationUnavailable
			}
			if scanErr != nil {
				return mapDBError("lock recovery invitation", scanErr)
			}
			if invitation.Kind != InvitationRecovery || invitation.TargetUserID == nil || invitation.UsedAt != nil || invitation.RevokedAt != nil || !invitation.ExpiresAt.After(params.At) {
				return ErrInvitationUnavailable
			}
			params.UserID = *invitation.TargetUserID
			result, execErr := tx.ExecContext(ctx, `UPDATE invitations SET used_at=$2,used_by_user_id=$3 WHERE id=$1 AND used_at IS NULL AND revoked_at IS NULL`, invitation.ID, params.At, params.UserID)
			if execErr != nil {
				return mapDBError("consume recovery invitation", execErr)
			}
			if err := requireAffected("consume recovery invitation", result); err != nil {
				return err
			}
		} else {
			var codeID string
			err = tx.QueryRowContext(ctx, `SELECT id FROM recovery_codes WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL FOR UPDATE`, params.UserID, params.RecoveryHash).Scan(&codeID)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return mapDBError("lock recovery code", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE recovery_codes SET used_at=$2 WHERE id=$1`, codeID, params.At); err != nil {
				return mapDBError("consume recovery code", err)
			}
		}
		user, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=$1 AND status='active' FOR UPDATE`, params.UserID))
		if err != nil {
			return mapDBError("lock recovery user", err)
		}
		if err := writeIdentityCredential(ctx, tx, user.ID, params.Credential, params.At, true); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=$2,revoke_reason='account_recovery' WHERE user_id=$1 AND revoked_at IS NULL`, user.ID, params.At); err != nil {
			return mapDBError("revoke recovery sessions", err)
		}
		if err := insertRecoveryCodes(ctx, tx, user.ID, params.RecoveryHashes); err != nil {
			return err
		}
		params.Session.UserID = user.ID
		session, err = insertSession(ctx, tx, params.Session, true)
		return err
	})
	return user, session, err
}

type CompletePasswordLoginParams struct {
	UserID, ExpectedHash, ReplacementHash string
	Session                               CreateSessionParams
	At                                    time.Time
}

// ComparePasswordHash is the pure compare-and-swap predicate used by password
// login and rehash transactions.
func ComparePasswordHash(current, expected string) error {
	if current != expected {
		return ErrConflict
	}
	return nil
}

func (s *Store) CompletePasswordLogin(ctx context.Context, params CompletePasswordLoginParams) (User, Session, error) {
	var user User
	var session Session
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var current string
		userRow := tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=$1 AND status='active' FOR UPDATE`, params.UserID)
		var err error
		user, err = scanUser(userRow)
		if err != nil {
			return mapDBError("lock password user", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT encoded_hash FROM password_credentials WHERE user_id=$1 FOR UPDATE`, user.ID).Scan(&current); err != nil {
			return mapDBError("lock password credential", err)
		}
		if err := ComparePasswordHash(current, params.ExpectedHash); err != nil {
			return err
		}
		if params.ReplacementHash != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE password_credentials SET encoded_hash=$2,updated_at=$3,last_used_at=$3 WHERE user_id=$1 AND encoded_hash=$4`, user.ID, params.ReplacementHash, params.At, current); err != nil {
				return mapDBError("rehash password", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE password_credentials SET last_used_at=$2 WHERE user_id=$1`, user.ID, params.At); err != nil {
			return mapDBError("record password use", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET last_login_at=$2 WHERE id=$1`, user.ID, params.At); err != nil {
			return mapDBError("record password login", err)
		}
		params.Session.UserID = user.ID
		session, err = insertSession(ctx, tx, params.Session, false)
		return err
	})
	return user, session, err
}

func (s *Store) VerifyPasswordSession(ctx context.Context, userID, sessionID, expectedHash string, at time.Time) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT encoded_hash FROM password_credentials WHERE user_id=$1 FOR UPDATE`, userID).Scan(&current); err != nil {
			return mapDBError("lock password credential", err)
		}
		if err := ComparePasswordHash(current, expectedHash); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE sessions SET recently_verified_at=$3 WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL AND idle_expires_at>$3 AND absolute_expires_at>$3`, sessionID, userID, at)
		if err != nil {
			return mapDBError("verify password session", err)
		}
		return requireAffected("verify password session", result)
	})
}

func (s *Store) SetPassword(ctx context.Context, userID, currentSessionID, encodedHash string, at time.Time) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 AND status='active' FOR UPDATE`, userID).Scan(&locked); err != nil {
			return mapDBError("lock password user", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL AND idle_expires_at>$3 AND absolute_expires_at>$3 FOR UPDATE`, currentSessionID, userID, at).Scan(&locked); err != nil {
			return mapDBError("lock current password session", err)
		}
		if err := insertPasswordCredential(ctx, tx, userID, encodedHash, at, true); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=$3,revoke_reason='password_changed' WHERE user_id=$1 AND id<>$2 AND revoked_at IS NULL`, userID, currentSessionID, at); err != nil {
			return mapDBError("revoke sessions after password change", err)
		}
		return nil
	})
}
