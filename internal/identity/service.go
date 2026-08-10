// Package identity owns WebAuthn ceremonies and browser sessions. Ceremony
// state is intentionally short-lived and process-local: the supported
// deployment runs one gateway instance, and a restart merely asks the browser
// to begin again. Every flow ID is single-use, including failed assertions.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	ceremonyTTL          = 5 * time.Minute
	maxPendingCeremonies = 10_000
)

var (
	ErrInvalidCeremony    = errors.New("invalid or expired WebAuthn ceremony")
	ErrInvalidUsername    = errors.New("invalid username")
	ErrInvalidDisplayName = errors.New("invalid display name")
	ErrRecoveryRejected   = errors.New("recovery credentials rejected")
	ErrTooManyCeremonies  = errors.New("too many pending WebAuthn ceremonies")
)

type Config struct {
	RPID          string
	RPOrigins     []string
	TokenPepper   []byte
	SessionIdle   time.Duration
	SessionMax    time.Duration
	CeremonyTTL   time.Duration
	SecureCookies bool
}

type Service struct {
	store      *store.Store
	webAuthn   *webauthn.WebAuthn
	challenges *challengeStore
	config     Config
	now        func() time.Time
}

func New(repository *store.Store, config Config) (*Service, error) {
	if repository == nil || config.RPID == "" || len(config.RPOrigins) == 0 || len(config.TokenPepper) < security.MinimumPepperBytes {
		return nil, errors.New("identity: store, RP ID, RP origins and a 32-byte token pepper are required")
	}
	if config.SessionIdle <= 0 {
		config.SessionIdle = 12 * time.Hour
	}
	if config.SessionMax <= 0 {
		config.SessionMax = 7 * 24 * time.Hour
	}
	if config.CeremonyTTL <= 0 || config.CeremonyTTL > 10*time.Minute {
		config.CeremonyTTL = ceremonyTTL
	}
	requireResident := true
	web, err := webauthn.New(&webauthn.Config{
		RPID:          config.RPID,
		RPDisplayName: "Personal Codex Gateway",
		RPOrigins:     append([]string(nil), config.RPOrigins...),
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			RequireResidentKey: &requireResident,
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationRequired,
		},
		Debug: false,
	})
	if err != nil {
		return nil, fmt.Errorf("configure WebAuthn: %w", err)
	}
	return &Service{
		store: repository, webAuthn: web, config: config,
		challenges: newChallengeStore(config.CeremonyTTL), now: time.Now,
	}, nil
}

type Ceremony struct {
	FlowID  string `json:"flow_id"`
	Options any    `json:"options"`
}

type RegistrationResult struct {
	User          store.User
	SessionToken  string
	RecoveryCodes []string
}

type LoginResult struct {
	User         store.User
	SessionToken string
}

type pendingKind uint8

const (
	pendingInvitation pendingKind = iota + 1
	pendingLogin
	pendingReauthentication
	pendingAddCredential
	pendingRecovery
	pendingRecoveryInvitation
)

type pendingCeremony struct {
	kind           pendingKind
	session        webauthn.SessionData
	user           authnUser
	invitationHash []byte
	username       string
	displayName    string
	expiresAt      time.Time
}

// BeginInvitationRegistration validates an invitation without consuming it.
// The invitation is consumed only after WebAuthn attestation succeeds.
func (s *Service) BeginInvitationRegistration(ctx context.Context, token, username, displayName string) (Ceremony, error) {
	rawDigest, err := security.DigestOpaqueToken(security.InvitationToken, token)
	if err != nil {
		return Ceremony{}, store.ErrInvitationUnavailable
	}
	digest, err := security.PepperTokenDigest(s.config.TokenPepper, rawDigest)
	if err != nil {
		return Ceremony{}, err
	}
	now := s.now().UTC()
	invitation, err := s.store.GetAvailableInvitation(ctx, digest[:], now)
	if err != nil {
		return Ceremony{}, err
	}
	if invitation.Kind == store.InvitationRecovery {
		if invitation.TargetUserID == nil {
			return Ceremony{}, store.ErrInvitationUnavailable
		}
		return s.beginExistingRegistration(ctx, *invitation.TargetUserID, pendingRecoveryInvitation, digest[:])
	}
	username, displayName, err = validateNames(username, displayName)
	if err != nil {
		return Ceremony{}, err
	}
	handle := make([]byte, 32)
	if _, err := rand.Read(handle); err != nil {
		return Ceremony{}, fmt.Errorf("generate WebAuthn user handle: %w", err)
	}
	user := authnUser{record: store.User{
		Username: username, DisplayName: displayName, WebAuthnUserID: handle,
	}}
	options, session, err := s.webAuthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
	)
	if err != nil {
		return Ceremony{}, fmt.Errorf("begin registration: %w", err)
	}
	flowID, err := s.challenges.put(pendingCeremony{
		kind: pendingInvitation, session: *session, user: user,
		invitationHash: append([]byte(nil), digest[:]...), username: username,
		displayName: displayName, expiresAt: now.Add(s.config.CeremonyTTL),
	})
	if err != nil {
		return Ceremony{}, err
	}
	return Ceremony{FlowID: flowID, Options: options}, nil
}

func (s *Service) FinishInvitationRegistration(ctx context.Context, flowID string, credentialJSON []byte, sourceIP net.IP, userAgent string) (RegistrationResult, error) {
	pending, err := s.challenges.take(flowID, pendingInvitation, pendingRecoveryInvitation)
	if err != nil {
		return RegistrationResult{}, err
	}
	credential, err := s.finishRegistration(pending, credentialJSON)
	if err != nil {
		return RegistrationResult{}, err
	}
	now := s.now().UTC()
	if pending.kind == pendingRecoveryInvitation {
		user := pending.user.record
		if _, err := s.store.ConsumeInvitation(ctx, pending.invitationHash, user.ID, now); err != nil {
			return RegistrationResult{}, err
		}
		return s.finishRecoveredAccount(ctx, user, credential, sourceIP, userAgent)
	}

	user, err := s.store.CreateUserFromInvitation(ctx, pending.invitationHash, store.CreateUserParams{
		Username: pending.username, DisplayName: pending.displayName,
		WebAuthnUserID: append([]byte(nil), pending.user.record.WebAuthnUserID...),
	})
	if err != nil {
		return RegistrationResult{}, err
	}
	if _, err := s.addCredential(ctx, user.ID, credential, "首个 Passkey"); err != nil {
		return RegistrationResult{}, fmt.Errorf("save initial Passkey: %w", err)
	}
	codes, err := s.replaceRecoveryCodes(ctx, user.ID)
	if err != nil {
		return RegistrationResult{}, err
	}
	token, _, err := s.createSession(ctx, user.ID, sourceIP, userAgent, now)
	if err != nil {
		return RegistrationResult{}, err
	}
	return RegistrationResult{User: user, SessionToken: token, RecoveryCodes: codes}, nil
}

func (s *Service) BeginLogin() (Ceremony, error) {
	options, session, err := s.webAuthn.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return Ceremony{}, fmt.Errorf("begin login: %w", err)
	}
	flowID, err := s.challenges.put(pendingCeremony{
		kind: pendingLogin, session: *session, expiresAt: s.now().UTC().Add(s.config.CeremonyTTL),
	})
	if err != nil {
		return Ceremony{}, err
	}
	return Ceremony{FlowID: flowID, Options: options}, nil
}

func (s *Service) FinishLogin(ctx context.Context, flowID string, credentialJSON []byte, sourceIP net.IP, userAgent string) (LoginResult, error) {
	pending, err := s.challenges.take(flowID, pendingLogin)
	if err != nil {
		return LoginResult{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credentialJSON)
	if err != nil {
		return LoginResult{}, fmt.Errorf("parse WebAuthn assertion: %w", err)
	}
	validatedUser, credential, err := s.webAuthn.ValidatePasskeyLogin(
		func(_, userHandle []byte) (webauthn.User, error) {
			return s.loadUserByHandle(ctx, userHandle)
		}, pending.session, parsed,
	)
	if err != nil {
		return LoginResult{}, fmt.Errorf("validate WebAuthn assertion: %w", err)
	}
	user := validatedUser.(authnUser).record
	if err := s.persistCredentialUse(ctx, credential); err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	token, _, err := s.createSession(ctx, user.ID, sourceIP, userAgent, now)
	if err != nil {
		return LoginResult{}, err
	}
	_ = s.store.RecordUserLogin(ctx, user.ID, now)
	return LoginResult{User: user, SessionToken: token}, nil
}

func (s *Service) BeginReauthentication(ctx context.Context, userID string) (Ceremony, error) {
	return s.beginUserLogin(ctx, userID, pendingReauthentication)
}

func (s *Service) FinishReauthentication(ctx context.Context, flowID string, credentialJSON []byte, sessionID string) error {
	pending, err := s.challenges.take(flowID, pendingReauthentication)
	if err != nil {
		return err
	}
	if pending.user.record.ID == "" {
		return ErrInvalidCeremony
	}
	credential, err := s.validateUserAssertion(pending, credentialJSON)
	if err != nil {
		return err
	}
	if err := s.persistCredentialUse(ctx, credential); err != nil {
		return err
	}
	return s.store.MarkSessionVerified(ctx, sessionID, s.now().UTC())
}

func (s *Service) BeginAddCredential(ctx context.Context, userID string) (Ceremony, error) {
	return s.beginExistingRegistration(ctx, userID, pendingAddCredential, nil)
}

func (s *Service) FinishAddCredential(ctx context.Context, flowID string, credentialJSON []byte, nickname string) (store.WebAuthnCredential, error) {
	pending, err := s.challenges.take(flowID, pendingAddCredential)
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	credential, err := s.finishRegistration(pending, credentialJSON)
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	return s.addCredential(ctx, pending.user.record.ID, credential, nickname)
}

// BeginRecovery consumes a recovery code before issuing a new registration
// challenge. Abandoning the ceremony therefore cannot leave a reusable code.
func (s *Service) BeginRecovery(ctx context.Context, username, code string) (Ceremony, error) {
	user, err := s.store.GetUserByUsername(ctx, strings.TrimSpace(username))
	if err != nil || user.Status != store.StatusActive {
		return Ceremony{}, ErrRecoveryRejected
	}
	rawDigest, err := security.DigestRecoveryCode(strings.ToUpper(strings.TrimSpace(code)))
	if err != nil {
		return Ceremony{}, ErrRecoveryRejected
	}
	digest, err := security.PepperTokenDigest(s.config.TokenPepper, rawDigest)
	if err != nil {
		return Ceremony{}, err
	}
	consumed, err := s.store.ConsumeRecoveryCode(ctx, user.ID, digest[:], s.now().UTC())
	if err != nil || !consumed {
		return Ceremony{}, ErrRecoveryRejected
	}
	return s.beginExistingRegistration(ctx, user.ID, pendingRecovery, nil)
}

func (s *Service) FinishRecovery(ctx context.Context, flowID string, credentialJSON []byte, sourceIP net.IP, userAgent string) (RegistrationResult, error) {
	pending, err := s.challenges.take(flowID, pendingRecovery)
	if err != nil {
		return RegistrationResult{}, err
	}
	credential, err := s.finishRegistration(pending, credentialJSON)
	if err != nil {
		return RegistrationResult{}, err
	}
	return s.finishRecoveredAccount(ctx, pending.user.record, credential, sourceIP, userAgent)
}

func (s *Service) finishRecoveredAccount(ctx context.Context, user store.User, credential *webauthn.Credential, sourceIP net.IP, userAgent string) (RegistrationResult, error) {
	if _, err := s.addCredential(ctx, user.ID, credential, "恢复 Passkey"); err != nil {
		return RegistrationResult{}, err
	}
	now := s.now().UTC()
	if _, err := s.store.RevokeUserSessions(ctx, user.ID, "account_recovery", now); err != nil {
		return RegistrationResult{}, err
	}
	codes, err := s.replaceRecoveryCodes(ctx, user.ID)
	if err != nil {
		return RegistrationResult{}, err
	}
	token, session, err := s.createSession(ctx, user.ID, sourceIP, userAgent, now)
	if err != nil {
		return RegistrationResult{}, err
	}
	if err := s.store.MarkSessionVerified(ctx, session.ID, now); err != nil {
		return RegistrationResult{}, err
	}
	return RegistrationResult{User: user, SessionToken: token, RecoveryCodes: codes}, nil
}

func (s *Service) beginExistingRegistration(ctx context.Context, userID string, kind pendingKind, invitationHash []byte) (Ceremony, error) {
	user, err := s.loadUser(ctx, userID)
	if err != nil {
		return Ceremony{}, err
	}
	exclusions := make([]protocol.CredentialDescriptor, 0, len(user.credentials))
	for _, credential := range user.credentials {
		exclusions = append(exclusions, credential.Descriptor())
	}
	options, session, err := s.webAuthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclusions),
	)
	if err != nil {
		return Ceremony{}, fmt.Errorf("begin Passkey registration: %w", err)
	}
	flowID, err := s.challenges.put(pendingCeremony{
		kind: kind, session: *session, user: user,
		invitationHash: append([]byte(nil), invitationHash...),
		expiresAt:      s.now().UTC().Add(s.config.CeremonyTTL),
	})
	if err != nil {
		return Ceremony{}, err
	}
	return Ceremony{FlowID: flowID, Options: options}, nil
}

func (s *Service) beginUserLogin(ctx context.Context, userID string, kind pendingKind) (Ceremony, error) {
	user, err := s.loadUser(ctx, userID)
	if err != nil {
		return Ceremony{}, err
	}
	options, session, err := s.webAuthn.BeginLogin(user,
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return Ceremony{}, fmt.Errorf("begin WebAuthn verification: %w", err)
	}
	flowID, err := s.challenges.put(pendingCeremony{
		kind: kind, session: *session, user: user,
		expiresAt: s.now().UTC().Add(s.config.CeremonyTTL),
	})
	if err != nil {
		return Ceremony{}, err
	}
	return Ceremony{FlowID: flowID, Options: options}, nil
}

func (s *Service) validateUserAssertion(pending pendingCeremony, raw []byte) (*webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parse WebAuthn assertion: %w", err)
	}
	credential, err := s.webAuthn.ValidateLogin(pending.user, pending.session, parsed)
	if err != nil {
		return nil, fmt.Errorf("validate WebAuthn assertion: %w", err)
	}
	return credential, nil
}

func (s *Service) finishRegistration(pending pendingCeremony, raw []byte) (*webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parse WebAuthn attestation: %w", err)
	}
	credential, err := s.webAuthn.CreateCredential(pending.user, pending.session, parsed)
	if err != nil {
		return nil, fmt.Errorf("validate WebAuthn attestation: %w", err)
	}
	return credential, nil
}

func (s *Service) loadUser(ctx context.Context, userID string) (authnUser, error) {
	record, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return authnUser{}, err
	}
	if record.Status != store.StatusActive {
		return authnUser{}, store.ErrNotFound
	}
	credentials, err := s.store.ListWebAuthnCredentials(ctx, record.ID)
	if err != nil {
		return authnUser{}, err
	}
	return makeAuthnUser(record, credentials)
}

func (s *Service) loadUserByHandle(ctx context.Context, handle []byte) (authnUser, error) {
	record, err := s.store.GetUserByWebAuthnID(ctx, handle)
	if err != nil {
		return authnUser{}, err
	}
	return s.loadUser(ctx, record.ID)
}

func makeAuthnUser(record store.User, stored []store.WebAuthnCredential) (authnUser, error) {
	credentials := make([]webauthn.Credential, 0, len(stored))
	for _, value := range stored {
		credential, err := fromStoredCredential(value)
		if err != nil {
			return authnUser{}, err
		}
		credentials = append(credentials, credential)
	}
	return authnUser{record: record, credentials: credentials}, nil
}

type authnUser struct {
	record      store.User
	credentials []webauthn.Credential
}

func (u authnUser) WebAuthnID() []byte          { return append([]byte(nil), u.record.WebAuthnUserID...) }
func (u authnUser) WebAuthnName() string        { return u.record.Username }
func (u authnUser) WebAuthnDisplayName() string { return u.record.DisplayName }
func (u authnUser) WebAuthnCredentials() []webauthn.Credential {
	return append([]webauthn.Credential(nil), u.credentials...)
}

func fromStoredCredential(value store.WebAuthnCredential) (webauthn.Credential, error) {
	var credential webauthn.Credential
	if err := json.Unmarshal(value.CredentialJSON, &credential); err != nil {
		return webauthn.Credential{}, fmt.Errorf("decode stored WebAuthn credential: %w", err)
	}
	if !hmac.Equal(credential.ID, value.CredentialID) || value.SignCount > uint64(^uint32(0)) {
		return webauthn.Credential{}, errors.New("stored WebAuthn credential metadata mismatch")
	}
	// The dedicated counter is authoritative even if a process crashed between
	// its compare-and-set and updating the serialized credential.
	credential.Authenticator.SignCount = uint32(value.SignCount)
	return credential, nil
}

func (s *Service) addCredential(ctx context.Context, userID string, credential *webauthn.Credential, nickname string) (store.WebAuthnCredential, error) {
	credentialJSON, err := json.Marshal(credential)
	if err != nil {
		return store.WebAuthnCredential{}, fmt.Errorf("encode WebAuthn credential: %w", err)
	}
	transports := make([]string, 0, len(credential.Transport))
	for _, transport := range credential.Transport {
		transports = append(transports, string(transport))
	}
	return s.store.AddWebAuthnCredential(ctx, store.AddWebAuthnCredentialParams{
		UserID: userID, CredentialID: credential.ID, CredentialJSON: credentialJSON,
		SignCount: uint64(credential.Authenticator.SignCount), Transports: transports,
		BackupEligible: credential.Flags.BackupEligible, BackupState: credential.Flags.BackupState,
		Discoverable: true, AAGUID: formatUUID(credential.Authenticator.AAGUID),
		Nickname: strings.TrimSpace(nickname),
	})
}

func (s *Service) persistCredentialUse(ctx context.Context, credential *webauthn.Credential) error {
	if credential.Authenticator.CloneWarning {
		return errors.New("WebAuthn authenticator counter indicates a possible clone")
	}
	if err := s.store.UpdateWebAuthnCounter(ctx, credential.ID, uint64(credential.Authenticator.SignCount), s.now().UTC()); err != nil {
		return err
	}
	credentialJSON, err := json.Marshal(credential)
	if err != nil {
		return fmt.Errorf("encode updated WebAuthn credential: %w", err)
	}
	return s.store.UpdateWebAuthnCredential(ctx, credential.ID, credentialJSON)
}

func (s *Service) replaceRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	generated, err := security.GenerateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	hashes := make([][]byte, 0, len(generated))
	plaintext := make([]string, 0, len(generated))
	for _, code := range generated {
		digest, err := security.PepperTokenDigest(s.config.TokenPepper, code.Digest)
		if err != nil {
			return nil, err
		}
		hashCopy := append([]byte(nil), digest[:]...)
		hashes = append(hashes, hashCopy)
		plaintext = append(plaintext, code.Code)
	}
	if _, err := s.store.ReplaceRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, err
	}
	return plaintext, nil
}

func (s *Service) createSession(ctx context.Context, userID string, sourceIP net.IP, userAgent string, now time.Time) (string, store.Session, error) {
	generated, err := security.GenerateOpaqueToken(security.SessionToken)
	if err != nil {
		return "", store.Session{}, err
	}
	digest, err := security.PepperTokenDigest(s.config.TokenPepper, generated.Digest)
	if err != nil {
		return "", store.Session{}, err
	}
	csrf := make([]byte, 32)
	if _, err := rand.Read(csrf); err != nil {
		return "", store.Session{}, err
	}
	ua := sha256.Sum256([]byte(userAgent))
	ip := ""
	if sourceIP != nil {
		ip = sourceIP.String()
	}
	session, err := s.store.CreateSession(ctx, store.CreateSessionParams{
		UserID: userID, TokenHash: digest[:], CSRFSecret: csrf,
		SourceIP: ip, UserAgentHash: ua[:], CreatedAt: now,
		IdleExpiresAt: now.Add(s.config.SessionIdle), AbsoluteExpiresAt: now.Add(s.config.SessionMax),
	})
	if err != nil {
		return "", store.Session{}, err
	}
	return generated.Token, session, nil
}

func validateNames(username, displayName string) (string, string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 32 || username[0] < 'a' || username[0] > 'z' {
		return "", "", ErrInvalidUsername
	}
	for _, r := range username {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return "", "", ErrInvalidUsername
		}
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) > 80 {
		return "", "", ErrInvalidDisplayName
	}
	return username, displayName, nil
}

func formatUUID(value []byte) string {
	if len(value) != 16 {
		return ""
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

type challengeStore struct {
	mu      sync.Mutex
	entries map[string]pendingCeremony
	ttl     time.Duration
	now     func() time.Time
}

func newChallengeStore(ttl time.Duration) *challengeStore {
	return &challengeStore{entries: make(map[string]pendingCeremony), ttl: ttl, now: time.Now}
}

func (s *challengeStore) put(value pendingCeremony) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	for key, existing := range s.entries {
		if !existing.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
	if len(s.entries) >= maxPendingCeremonies {
		return "", ErrTooManyCeremonies
	}
	for attempts := 0; attempts < 3; attempts++ {
		var raw [24]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		id := base64.RawURLEncoding.EncodeToString(raw[:])
		if _, exists := s.entries[id]; !exists {
			if value.expiresAt.IsZero() {
				value.expiresAt = now.Add(s.ttl)
			}
			s.entries[id] = value
			return id, nil
		}
	}
	return "", security.ErrRandomCollision
}

func (s *challengeStore) take(id string, kinds ...pendingKind) (pendingCeremony, error) {
	if len(id) != 32 {
		return pendingCeremony{}, ErrInvalidCeremony
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.entries[id]
	delete(s.entries, id)
	if !ok || !value.expiresAt.After(s.now().UTC()) {
		return pendingCeremony{}, ErrInvalidCeremony
	}
	for _, kind := range kinds {
		if value.kind == kind {
			return value, nil
		}
	}
	return pendingCeremony{}, ErrInvalidCeremony
}
