package identity

import (
	"context"
	"net"

	"github.com/wsw/codex-gateway/internal/store"
)

// LoginExternal issues a normal local session only for an existing active
// mapping. Its provenance is stored atomically with the binding status check.
// An external login deliberately does not confer local recent verification.
type ExternalLoginResult struct {
	LoginResult
	SessionID string
}

func (s *Service) LoginExternal(ctx context.Context, external OIDCIdentity, sourceIP net.IP, userAgent string) (ExternalLoginResult, error) {
	now := s.now().UTC()
	token, params, err := s.newSessionParams("", sourceIP, userAgent, now)
	if err != nil {
		return ExternalLoginResult{}, err
	}
	user, session, err := s.store.CompleteExternalLogin(ctx, store.CompleteExternalLoginParams{Issuer: external.Issuer, Subject: external.Subject, Session: params, At: now})
	if err != nil {
		return ExternalLoginResult{}, err
	}
	return ExternalLoginResult{LoginResult: LoginResult{User: user, SessionToken: token}, SessionID: session.ID}, nil
}
