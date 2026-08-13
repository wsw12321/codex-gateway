package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net"
	"strings"

	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

const dummyPasswordHash = "$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func (s *Service) RegisterPassword(ctx context.Context, invitationToken, username, displayName, password string, sourceIP net.IP, userAgent string) (RegistrationResult, error) {
	username, displayName, err := validateNames(username, displayName)
	if err != nil {
		return RegistrationResult{}, err
	}
	raw, err := security.DigestOpaqueToken(security.InvitationToken, invitationToken)
	if err != nil {
		return RegistrationResult{}, store.ErrInvitationUnavailable
	}
	digest, err := security.PepperTokenDigest(s.config.TokenPepper, raw)
	if err != nil {
		return RegistrationResult{}, err
	}
	invitation, err := s.store.GetAvailableInvitation(ctx, digest[:], s.now().UTC())
	if err != nil || invitation.Kind == store.InvitationRecovery {
		return RegistrationResult{}, store.ErrInvitationUnavailable
	}
	encoded, err := hashPasswordContext(ctx, password)
	if err != nil {
		return RegistrationResult{}, err
	}
	handle := make([]byte, 32)
	if _, err := rand.Read(handle); err != nil {
		return RegistrationResult{}, err
	}
	plain, hashes, err := s.generateRecoveryCodes()
	if err != nil {
		return RegistrationResult{}, err
	}
	now := s.now().UTC()
	token, session, err := s.newSessionParams("", sourceIP, userAgent, now)
	if err != nil {
		return RegistrationResult{}, err
	}
	user, _, err := s.store.CompleteInvitationRegistration(ctx, store.CompleteInvitationParams{
		InvitationHash: digest[:],
		User:           store.CreateUserParams{Username: username, DisplayName: displayName, WebAuthnUserID: handle},
		Credential:     store.IdentityCredential{EncodedPasswordHash: encoded},
		RecoveryHashes: hashes, Session: session,
	})
	if err != nil {
		return RegistrationResult{}, err
	}
	return RegistrationResult{User: user, SessionToken: token, RecoveryCodes: plain}, nil
}

func (s *Service) PasswordLogin(ctx context.Context, username, password string, sourceIP net.IP, userAgent string) (LoginResult, error) {
	user, userErr := s.store.GetUserByUsername(ctx, username)
	credential := store.PasswordCredential{EncodedHash: dummyPasswordHash}
	credentialErr := error(nil)
	if userErr == nil {
		credential, credentialErr = s.store.GetPasswordCredential(ctx, user.ID)
	}
	usable := userErr == nil && credentialErr == nil && user.Status == store.StatusActive
	valid, rehash, err := verifyPassword(password, credential.EncodedHash)
	if err != nil {
		if errors.Is(err, ErrHashBusy) {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	if userErr != nil && !errors.Is(userErr, store.ErrNotFound) {
		return LoginResult{}, userErr
	}
	if credentialErr != nil && !errors.Is(credentialErr, store.ErrNotFound) {
		return LoginResult{}, credentialErr
	}
	if !usable || !valid {
		return LoginResult{}, ErrInvalidCredentials
	}
	replacement := ""
	if rehash {
		replacement, err = hashPasswordContext(ctx, password)
		if err != nil {
			return LoginResult{}, err
		}
	}
	now := s.now().UTC()
	token, session, err := s.newSessionParams(user.ID, sourceIP, userAgent, now)
	if err != nil {
		return LoginResult{}, err
	}
	user, _, err = s.store.CompletePasswordLogin(ctx, store.CompletePasswordLoginParams{
		UserID: user.ID, ExpectedHash: credential.EncodedHash, ReplacementHash: replacement, Session: session, At: now,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, err
	}
	return LoginResult{User: user, SessionToken: token}, nil
}

func (s *Service) RecoverWithPassword(ctx context.Context, username, recoveryCode, invitationToken, password string, sourceIP net.IP, userAgent string) (RegistrationResult, error) {
	if err := validatePassword(password); err != nil {
		return RegistrationResult{}, err
	}
	var userID string
	var recoveryHash, invitationHash []byte
	if invitationToken != "" && username == "" && recoveryCode == "" {
		raw, err := security.DigestOpaqueToken(security.InvitationToken, invitationToken)
		if err != nil {
			return RegistrationResult{}, ErrRecoveryRejected
		}
		digest, err := security.PepperTokenDigest(s.config.TokenPepper, raw)
		if err != nil {
			return RegistrationResult{}, err
		}
		invitation, err := s.store.GetAvailableInvitation(ctx, digest[:], s.now().UTC())
		if err != nil || invitation.Kind != store.InvitationRecovery || invitation.TargetUserID == nil {
			return RegistrationResult{}, ErrRecoveryRejected
		}
		userID, invitationHash = *invitation.TargetUserID, append([]byte(nil), digest[:]...)
	} else if invitationToken == "" && username != "" && recoveryCode != "" {
		user, err := s.store.GetUserByUsername(ctx, username)
		if err != nil || user.Status != store.StatusActive {
			return RegistrationResult{}, ErrRecoveryRejected
		}
		raw, err := security.DigestRecoveryCode(strings.ToUpper(strings.TrimSpace(recoveryCode)))
		if err != nil {
			return RegistrationResult{}, ErrRecoveryRejected
		}
		digest, err := security.PepperTokenDigest(s.config.TokenPepper, raw)
		if err != nil {
			return RegistrationResult{}, err
		}
		if _, err := s.store.GetUnusedRecoveryCode(ctx, user.ID, digest[:]); err != nil {
			return RegistrationResult{}, ErrRecoveryRejected
		}
		userID, recoveryHash = user.ID, append([]byte(nil), digest[:]...)
	} else {
		return RegistrationResult{}, ErrRecoveryRejected
	}
	encoded, err := hashPasswordContext(ctx, password)
	if err != nil {
		return RegistrationResult{}, err
	}
	plain, hashes, err := s.generateRecoveryCodes()
	if err != nil {
		return RegistrationResult{}, err
	}
	now := s.now().UTC()
	token, session, err := s.newSessionParams(userID, sourceIP, userAgent, now)
	if err != nil {
		return RegistrationResult{}, err
	}
	user, _, err := s.store.CompleteAccountRecovery(ctx, store.CompleteRecoveryParams{
		UserID: userID, RecoveryHash: recoveryHash, InvitationHash: invitationHash,
		Credential: store.IdentityCredential{EncodedPasswordHash: encoded}, RecoveryHashes: hashes, Session: session, At: now,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvitationUnavailable) {
			return RegistrationResult{}, ErrRecoveryRejected
		}
		return RegistrationResult{}, err
	}
	return RegistrationResult{User: user, SessionToken: token, RecoveryCodes: plain}, nil
}

func (s *Service) ReauthenticatePassword(ctx context.Context, userID, sessionID, password string) error {
	credential, err := s.store.GetPasswordCredential(ctx, userID)
	if err != nil {
		_, _, slotErr := verifyPassword(password, dummyPasswordHash)
		if slotErr != nil {
			return slotErr
		}
		return ErrInvalidCredentials
	}
	valid, _, err := verifyPassword(password, credential.EncodedHash)
	if err != nil {
		return err
	}
	if !valid {
		return ErrInvalidCredentials
	}
	if err := s.store.VerifyPasswordSession(ctx, userID, sessionID, credential.EncodedHash, s.now().UTC()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ErrInvalidCredentials
		}
		return err
	}
	return nil
}

func (s *Service) SetPassword(ctx context.Context, userID, currentSessionID, password string) error {
	encoded, err := hashPasswordContext(ctx, password)
	if err != nil {
		return err
	}
	return s.store.SetPassword(ctx, userID, currentSessionID, encoded, s.now().UTC())
}

func PasswordUsernameDigest(username string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	return string(digest[:])
}
