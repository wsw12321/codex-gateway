package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	// APIKeyEncryptionKeyBytes is the exact AES-256 key size accepted for
	// persisted API key secrets.
	APIKeyEncryptionKeyBytes = 32

	apiKeyCipherVersion byte = 1
)

var (
	ErrInvalidAPIKeyEncryptionKey = fmt.Errorf("API key encryption key must contain exactly %d bytes", APIKeyEncryptionKeyBytes)
	ErrInvalidAPIKeyCiphertext    = errors.New("invalid API key ciphertext")
)

// EncryptAPIKeySecret encrypts a canonical API key for later user-authorized
// reveal. The versioned envelope contains a random GCM nonce followed by the
// authenticated ciphertext. User and public IDs are bound as AAD and are not
// stored in the envelope.
func EncryptAPIKeySecret(key []byte, userID, publicID, token string) ([]byte, error) {
	return EncryptAPIKeySecretFrom(rand.Reader, key, userID, publicID, token)
}

// EncryptAPIKeySecretFrom is EncryptAPIKeySecret with an injectable source of
// nonce entropy for deterministic tests.
func EncryptAPIKeySecretFrom(random io.Reader, key []byte, userID, publicID, token string) ([]byte, error) {
	if len(key) != APIKeyEncryptionKeyBytes {
		return nil, ErrInvalidAPIKeyEncryptionKey
	}
	if random == nil {
		return nil, errors.New("encrypt API key secret: nil randomness source")
	}
	parsed, err := ParseAPIKey(token)
	if err != nil || userID == "" || publicID == "" ||
		!hmac.Equal([]byte(parsed.PublicID), []byte(publicID)) {
		return nil, ErrInvalidAPIKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encrypt API key secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt API key secret: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, fmt.Errorf("encrypt API key secret nonce: %w", err)
	}
	envelope := make([]byte, 1+len(nonce), 1+len(nonce)+len(token)+gcm.Overhead())
	envelope[0] = apiKeyCipherVersion
	copy(envelope[1:], nonce)
	envelope = gcm.Seal(envelope, nonce, []byte(token), apiKeySecretAAD(userID, publicID))
	return envelope, nil
}

// DecryptAPIKeySecret authenticates and decrypts a persisted API key secret.
// Callers must additionally compare the parsed public ID and HMAC digest with
// the active credential row before returning the plaintext.
func DecryptAPIKeySecret(key []byte, userID, publicID string, envelope []byte) (string, error) {
	if len(key) != APIKeyEncryptionKeyBytes {
		return "", ErrInvalidAPIKeyEncryptionKey
	}
	if userID == "" || publicID == "" || len(envelope) == 0 || envelope[0] != apiKeyCipherVersion {
		return "", ErrInvalidAPIKeyCiphertext
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("decrypt API key secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt API key secret: %w", err)
	}
	if len(envelope) < 1+gcm.NonceSize()+gcm.Overhead() {
		return "", ErrInvalidAPIKeyCiphertext
	}
	nonce := envelope[1 : 1+gcm.NonceSize()]
	plaintext, err := gcm.Open(nil, nonce, envelope[1+gcm.NonceSize():], apiKeySecretAAD(userID, publicID))
	if err != nil {
		return "", ErrInvalidAPIKeyCiphertext
	}
	defer clear(plaintext)
	token := string(plaintext)
	parsed, err := ParseAPIKey(token)
	if err != nil || !hmac.Equal([]byte(parsed.PublicID), []byte(publicID)) {
		return "", ErrInvalidAPIKeyCiphertext
	}
	return token, nil
}

func apiKeySecretAAD(userID, publicID string) []byte {
	value := make([]byte, 0, len(userID)+len(publicID)+48)
	value = append(value, "codex-gateway/api-key-secret/v1\x00"...)
	value = append(value, userID...)
	value = append(value, 0)
	value = append(value, publicID...)
	return value
}
