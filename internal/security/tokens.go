package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	OpaqueTokenBytes      = 32
	RecoveryCodeCount     = 10
	RecoveryCodeBytes     = 15 // 120 bits, encoded as 24 Crockford-base32 chars.
	recoveryCodeRawLength = 24
)

var (
	ErrInvalidToken        = errors.New("invalid opaque token")
	ErrInvalidTokenKind    = errors.New("invalid opaque token kind")
	ErrInvalidRecoveryCode = errors.New("invalid recovery code")
	ErrRandomCollision     = errors.New("random value collision")
	ErrTokenPepperTooShort = fmt.Errorf("token pepper must contain at least %d bytes", MinimumPepperBytes)
)

// TokenKind separates otherwise identical random-token formats and digests.
// This prevents a token issued for one purpose from being accepted for another.
type TokenKind uint8

const (
	InvitationToken TokenKind = iota + 1
	SessionToken
	RecoveryToken
)

func (k TokenKind) prefix() (string, bool) {
	switch k {
	case InvitationToken:
		return "cgi_v1_", true
	case SessionToken:
		return "cgs_v1_", true
	case RecoveryToken:
		return "cgr_v1_", true
	default:
		return "", false
	}
}

func (k TokenKind) domain() (string, bool) {
	switch k {
	case InvitationToken:
		return "codex-gateway/invitation-token/v1", true
	case SessionToken:
		return "codex-gateway/session-token/v1", true
	case RecoveryToken:
		return "codex-gateway/recovery-token/v1", true
	default:
		return "", false
	}
}

// TokenDigest is the SHA-256 value persisted for a cryptographically random
// opaque token or recovery code.
type TokenDigest [sha256.Size]byte

// GeneratedToken contains a one-time plaintext token and its safe-to-store
// digest. Token uses only unreserved URL characters, so it can be placed in a
// URL fragment without percent encoding.
type GeneratedToken struct {
	Token  string
	Digest TokenDigest
}

// PepperTokenDigest applies the server-held pepper to an already
// domain-separated token or recovery-code digest. Persistence layers should
// store this result, not the unpeppered digest returned by generation helpers.
func PepperTokenDigest(pepper []byte, digest TokenDigest) (TokenDigest, error) {
	if len(pepper) < MinimumPepperBytes {
		return TokenDigest{}, ErrTokenPepperTooShort
	}
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte("codex-gateway/token-digest-pepper/v1\x00"))
	_, _ = mac.Write(digest[:])
	var result TokenDigest
	copy(result[:], mac.Sum(nil))
	return result, nil
}

// String and GoString make accidental fmt logging safe. Callers must access
// Token explicitly when returning the one-time value to its owner.
func (t GeneratedToken) String() string   { return RedactedValue }
func (t GeneratedToken) GoString() string { return RedactedValue }

// GenerateOpaqueToken creates a 256-bit invitation, session, or account
// recovery token using crypto/rand.Reader.
func GenerateOpaqueToken(kind TokenKind) (GeneratedToken, error) {
	return GenerateOpaqueTokenFrom(kind, rand.Reader)
}

// GenerateOpaqueTokenFrom is the deterministic-test variant of
// GenerateOpaqueToken.
func GenerateOpaqueTokenFrom(kind TokenKind, random io.Reader) (GeneratedToken, error) {
	prefix, ok := kind.prefix()
	if !ok {
		return GeneratedToken{}, ErrInvalidTokenKind
	}
	if random == nil {
		return GeneratedToken{}, errors.New("generate opaque token: nil randomness source")
	}
	var entropy [OpaqueTokenBytes]byte
	if _, err := io.ReadFull(random, entropy[:]); err != nil {
		return GeneratedToken{}, fmt.Errorf("generate opaque token: %w", err)
	}
	defer clear(entropy[:])
	token := prefix + base64.RawURLEncoding.EncodeToString(entropy[:])
	digest, err := DigestOpaqueToken(kind, token)
	if err != nil {
		return GeneratedToken{}, err
	}
	return GeneratedToken{Token: token, Digest: digest}, nil
}

// DigestOpaqueToken strictly validates and hashes a token for persistence.
func DigestOpaqueToken(kind TokenKind, token string) (TokenDigest, error) {
	prefix, ok := kind.prefix()
	if !ok {
		return TokenDigest{}, ErrInvalidTokenKind
	}
	domain, _ := kind.domain()
	if len(token) != len(prefix)+43 || !strings.HasPrefix(token, prefix) {
		return TokenDigest{}, ErrInvalidToken
	}
	encoded := token[len(prefix):]
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != OpaqueTokenBytes ||
		base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return TokenDigest{}, ErrInvalidToken
	}
	return domainDigest(domain, token), nil
}

// VerifyOpaqueToken performs strict validation followed by a constant-time
// digest comparison.
func VerifyOpaqueToken(kind TokenKind, token string, expected TokenDigest) bool {
	actual, err := DigestOpaqueToken(kind, token)
	return err == nil && constantTimeDigestEqual(actual, expected)
}

// GeneratedRecoveryCode contains a code shown once and its persisted digest.
type GeneratedRecoveryCode struct {
	Code   string
	Digest TokenDigest
}

func (c GeneratedRecoveryCode) String() string   { return RedactedValue }
func (c GeneratedRecoveryCode) GoString() string { return RedactedValue }

// Crockford base32 omits I, L, O and U to reduce transcription errors.
var recoveryEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// GenerateRecoveryCodes creates exactly ten independent, one-time recovery
// codes using crypto/rand.Reader.
func GenerateRecoveryCodes() ([]GeneratedRecoveryCode, error) {
	return GenerateRecoveryCodesFrom(rand.Reader)
}

// GenerateRecoveryCodesFrom is the deterministic-test variant of
// GenerateRecoveryCodes.
func GenerateRecoveryCodesFrom(random io.Reader) ([]GeneratedRecoveryCode, error) {
	if random == nil {
		return nil, errors.New("generate recovery codes: nil randomness source")
	}
	codes := make([]GeneratedRecoveryCode, 0, RecoveryCodeCount)
	seen := make(map[string]struct{}, RecoveryCodeCount)
	for len(codes) < RecoveryCodeCount {
		var entropy [RecoveryCodeBytes]byte
		if _, err := io.ReadFull(random, entropy[:]); err != nil {
			return nil, fmt.Errorf("generate recovery code %d: %w", len(codes)+1, err)
		}
		raw := recoveryEncoding.EncodeToString(entropy[:])
		code := groupRecoveryCode(raw)
		if _, duplicate := seen[code]; duplicate {
			// A real 120-bit collision is effectively impossible. Failing closed
			// also prevents a broken entropy source from looping forever.
			return nil, ErrRandomCollision
		}
		digest, err := DigestRecoveryCode(code)
		if err != nil {
			return nil, err
		}
		seen[code] = struct{}{}
		codes = append(codes, GeneratedRecoveryCode{Code: code, Digest: digest})
	}
	return codes, nil
}

func groupRecoveryCode(raw string) string {
	var b strings.Builder
	b.Grow(len(raw) + len(raw)/4 - 1)
	for i := 0; i < len(raw); i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}

// DigestRecoveryCode strictly validates the displayed representation and
// returns its domain-separated digest.
func DigestRecoveryCode(code string) (TokenDigest, error) {
	raw, ok := ungroupRecoveryCode(code)
	if !ok {
		return TokenDigest{}, ErrInvalidRecoveryCode
	}
	decoded, err := recoveryEncoding.DecodeString(raw)
	if err != nil || len(decoded) != RecoveryCodeBytes || recoveryEncoding.EncodeToString(decoded) != raw {
		return TokenDigest{}, ErrInvalidRecoveryCode
	}
	return domainDigest("codex-gateway/recovery-code/v1", code), nil
}

func ungroupRecoveryCode(code string) (string, bool) {
	// Six groups of four characters: XXXX-XXXX-XXXX-XXXX-XXXX-XXXX.
	if len(code) != recoveryCodeRawLength+5 {
		return "", false
	}
	var b strings.Builder
	b.Grow(recoveryCodeRawLength)
	for i := 0; i < len(code); i++ {
		if (i+1)%5 == 0 {
			if code[i] != '-' {
				return "", false
			}
			continue
		}
		c := code[i]
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", rune(c)) {
			return "", false
		}
		b.WriteByte(c)
	}
	if b.Len() != recoveryCodeRawLength {
		return "", false
	}
	return b.String(), true
}

// VerifyRecoveryCode performs strict parsing and a constant-time digest
// comparison.
func VerifyRecoveryCode(code string, expected TokenDigest) bool {
	actual, err := DigestRecoveryCode(code)
	return err == nil && constantTimeDigestEqual(actual, expected)
}

func domainDigest(domain, value string) TokenDigest {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(value))
	var digest TokenDigest
	copy(digest[:], h.Sum(nil))
	return digest
}

func constantTimeDigestEqual(a, b TokenDigest) bool {
	// crypto/subtle is what hmac.Equal uses internally. Keeping this helper
	// local avoids accidentally replacing the comparison with == later.
	return hmacEqual(a[:], b[:])
}
