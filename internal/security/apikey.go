// Package security contains the small, auditable security primitives used by
// the gateway. It deliberately depends only on the Go standard library.
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// APIKeyVersionPrefix is the externally visible version marker.
	APIKeyVersionPrefix = "cgk_v1_"

	// APIKeyPublicIDBytes gives public IDs 96 bits of randomness. Public IDs are
	// lookup identifiers, not authenticators.
	APIKeyPublicIDBytes = 12
	// APIKeySecretBytes gives every API key a 256-bit authentication secret.
	APIKeySecretBytes = 32
	// MinimumPepperBytes prevents accidentally configuring a weak server pepper.
	MinimumPepperBytes = 32

	apiKeyPublicIDEncodedLen = 16
	apiKeySecretEncodedLen   = 43
)

var (
	ErrInvalidAPIKey  = errors.New("invalid API key")
	ErrPepperTooShort = fmt.Errorf("API key pepper must contain at least %d bytes", MinimumPepperBytes)
)

// APIKeyDigest is the HMAC-SHA256 value stored for an API key. The plaintext
// key must only be returned once, at creation time.
type APIKeyDigest [sha256.Size]byte

// GeneratedAPIKey contains the one-time plaintext value and its non-secret
// lookup/logging fields. Token is sensitive and must never be persisted or
// logged.
type GeneratedAPIKey struct {
	Token    string
	PublicID string
	// Prefix is safe to persist and log. It uniquely identifies the key without
	// disclosing any part of its 256-bit secret.
	Prefix string
}

// ParsedAPIKey is the result of strict parsing. Its secret intentionally has no
// exported accessor; callers should pass it to the hashing helpers in this
// package instead of copying it into application data structures.
type ParsedAPIKey struct {
	PublicID string
	secret   [APIKeySecretBytes]byte
	valid    bool
}

// GenerateAPIKey creates a key using crypto/rand.Reader.
func GenerateAPIKey() (GeneratedAPIKey, error) {
	return GenerateAPIKeyFrom(rand.Reader)
}

// GenerateAPIKeyFrom is GenerateAPIKey with an injectable entropy source. It
// is exported to make deterministic tests possible; production code should use
// GenerateAPIKey.
func GenerateAPIKeyFrom(random io.Reader) (GeneratedAPIKey, error) {
	if random == nil {
		return GeneratedAPIKey{}, errors.New("generate API key: nil randomness source")
	}

	var publicID [APIKeyPublicIDBytes]byte
	var secret [APIKeySecretBytes]byte
	if _, err := io.ReadFull(random, publicID[:]); err != nil {
		return GeneratedAPIKey{}, fmt.Errorf("generate API key public ID: %w", err)
	}
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return GeneratedAPIKey{}, fmt.Errorf("generate API key secret: %w", err)
	}
	defer clear(secret[:])

	publicEncoded := base64.RawURLEncoding.EncodeToString(publicID[:])
	secretEncoded := base64.RawURLEncoding.EncodeToString(secret[:])
	prefix := APIKeyVersionPrefix + publicEncoded + "_"
	return GeneratedAPIKey{
		Token:    prefix + secretEncoded,
		PublicID: publicEncoded,
		Prefix:   prefix,
	}, nil
}

// ParseAPIKey accepts only the canonical
// cgk_v1_<16-char-public-id>_<43-char-secret> representation. Whitespace,
// padding, alternate base64 encodings and trailing data are rejected.
func ParseAPIKey(raw string) (ParsedAPIKey, error) {
	var parsed ParsedAPIKey
	if len(raw) != len(APIKeyVersionPrefix)+apiKeyPublicIDEncodedLen+1+apiKeySecretEncodedLen ||
		!strings.HasPrefix(raw, APIKeyVersionPrefix) {
		return parsed, ErrInvalidAPIKey
	}

	remainder := raw[len(APIKeyVersionPrefix):]
	// Raw URL-base64 itself may contain underscores, so the separator must be
	// located by the public ID's fixed encoded length rather than searched for.
	separator := apiKeyPublicIDEncodedLen
	if remainder[separator] != '_' {
		return parsed, ErrInvalidAPIKey
	}
	publicEncoded := remainder[:separator]
	secretEncoded := remainder[separator+1:]

	publicRaw, err := base64.RawURLEncoding.Strict().DecodeString(publicEncoded)
	if err != nil || len(publicRaw) != APIKeyPublicIDBytes ||
		base64.RawURLEncoding.EncodeToString(publicRaw) != publicEncoded {
		return parsed, ErrInvalidAPIKey
	}
	secretRaw, err := base64.RawURLEncoding.Strict().DecodeString(secretEncoded)
	if err != nil || len(secretRaw) != APIKeySecretBytes ||
		base64.RawURLEncoding.EncodeToString(secretRaw) != secretEncoded {
		return parsed, ErrInvalidAPIKey
	}
	defer clear(secretRaw)

	parsed.PublicID = publicEncoded
	copy(parsed.secret[:], secretRaw)
	parsed.valid = true
	return parsed, nil
}

// Prefix returns the non-secret key prefix suitable for persistence and logs.
func (k ParsedAPIKey) Prefix() string {
	if !k.valid {
		return ""
	}
	return APIKeyVersionPrefix + k.PublicID + "_"
}

// String and GoString prevent common fmt-based logging from exposing the
// parsed secret. They are deliberately not a way to reconstruct a credential.
func (k ParsedAPIKey) String() string {
	if prefix := k.Prefix(); prefix != "" {
		return prefix + RedactedValue
	}
	return RedactedValue
}

func (k ParsedAPIKey) GoString() string { return k.String() }

func (k GeneratedAPIKey) String() string {
	if k.Prefix != "" {
		return k.Prefix + RedactedValue
	}
	return RedactedValue
}

func (k GeneratedAPIKey) GoString() string { return k.String() }

// HashAPIKey strictly parses raw and returns its peppered, domain-separated
// HMAC-SHA256 digest.
func HashAPIKey(pepper []byte, raw string) (APIKeyDigest, error) {
	parsed, err := ParseAPIKey(raw)
	if err != nil {
		return APIKeyDigest{}, err
	}
	return HashParsedAPIKey(pepper, parsed)
}

// HashParsedAPIKey returns the digest to persist for a parsed API key.
func HashParsedAPIKey(pepper []byte, key ParsedAPIKey) (APIKeyDigest, error) {
	if len(pepper) < MinimumPepperBytes {
		return APIKeyDigest{}, ErrPepperTooShort
	}
	if !key.valid {
		return APIKeyDigest{}, ErrInvalidAPIKey
	}

	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte("codex-gateway/api-key/v1\x00"))
	_, _ = mac.Write([]byte(key.PublicID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(key.secret[:])
	var digest APIKeyDigest
	copy(digest[:], mac.Sum(nil))
	return digest, nil
}

// VerifyAPIKey strictly parses raw and compares its digest in constant time.
// Malformed credentials are ordinary authentication failures (ok=false, nil
// error); an invalid server pepper is returned as a configuration error.
func VerifyAPIKey(pepper []byte, raw string, expected APIKeyDigest) (key ParsedAPIKey, ok bool, err error) {
	if len(pepper) < MinimumPepperBytes {
		return ParsedAPIKey{}, false, ErrPepperTooShort
	}
	key, err = ParseAPIKey(raw)
	if err != nil {
		return ParsedAPIKey{}, false, nil
	}
	ok, err = VerifyParsedAPIKey(pepper, key, expected)
	return key, ok, err
}

// VerifyParsedAPIKey avoids reparsing after a caller has used PublicID for the
// database lookup. The HMAC comparison remains constant time.
func VerifyParsedAPIKey(pepper []byte, key ParsedAPIKey, expected APIKeyDigest) (bool, error) {
	actual, err := HashParsedAPIKey(pepper, key)
	if err != nil {
		return false, err
	}
	return hmac.Equal(actual[:], expected[:]), nil
}

// APIKeyPrefixForLog returns the safe persisted prefix for a valid key. It
// never returns any bytes from the authentication secret.
func APIKeyPrefixForLog(raw string) (string, error) {
	key, err := ParseAPIKey(raw)
	if err != nil {
		return "", err
	}
	return key.Prefix(), nil
}
