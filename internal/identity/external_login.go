package identity

import (
	"context"
	"net"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

// LoginExternal issues a normal local session only for an existing active
// mapping. Its provenance is stored atomically with the binding status check.
type ExternalLoginResult struct {
	LoginResult
	SessionID string
}

func (s *Service) LoginExternal(ctx context.Context, external OIDCIdentity, sourceIP net.IP, userAgent string) (ExternalLoginResult, error) {
	return s.LoginExternalAt(ctx, external, s.now().UTC(), sourceIP, userAgent)
}

func (s *Service) LoginExternalAt(ctx context.Context, external OIDCIdentity, verifiedAt time.Time, sourceIP net.IP, userAgent string) (ExternalLoginResult, error) {
	now := s.now().UTC()
	token, params, err := s.newSessionParams("", sourceIP, userAgent, now)
	if err != nil {
		return ExternalLoginResult{}, err
	}
	user, session, err := s.store.CompleteExternalLogin(ctx, store.CompleteExternalLoginParams{Issuer: external.Issuer, Subject: external.Subject, Session: params, At: now, VerifiedAt: verifiedAt})
	if err != nil {
		return ExternalLoginResult{}, err
	}
	return ExternalLoginResult{LoginResult: LoginResult{User: user, SessionToken: token}, SessionID: session.ID}, nil
}

func (s *Service) RegisterExternal(ctx context.Context, external OIDCIdentity, username, displayName string, verifiedAt, expiresAt time.Time, sourceIP net.IP, userAgent string) (ExternalLoginResult, error) {
	username, displayName, err := validateNames(username, displayName)
	if err != nil {
		return ExternalLoginResult{}, err
	}
	now := s.now().UTC()
	token, params, err := s.newSessionParams("", sourceIP, userAgent, now)
	if err != nil {
		return ExternalLoginResult{}, err
	}
	user, session, err := s.store.CompleteExternalRegistration(ctx, store.CompleteExternalRegistrationParams{
		Issuer: external.Issuer, Subject: external.Subject, MaskedEmail: external.MaskedEmail,
		Username: username, DisplayName: displayName,
		Session: params, At: now, VerifiedAt: verifiedAt, ExpiresAt: expiresAt,
	})
	if err != nil {
		return ExternalLoginResult{}, err
	}
	return ExternalLoginResult{LoginResult: LoginResult{User: user, SessionToken: token}, SessionID: session.ID}, nil
}
