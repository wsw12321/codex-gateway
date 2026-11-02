package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
)

var ErrInvalidEgressCiphertext = errors.New("invalid egress ciphertext")

func egressCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrInvalidEgressCiphertext
	}
	derived, err := hkdf.Key(sha256.New, key, nil, "codex-gateway/egress-encryption/v1", 32)
	if err != nil {
		return nil, ErrInvalidEgressCiphertext
	}
	defer clear(derived)
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, ErrInvalidEgressCiphertext
	}
	return cipher.NewGCM(block)
}

// Egress passwords have an independent derived key and authenticated object ID.
func EncryptEgressPassword(key []byte, nodeID, password string) ([]byte, error) {
	a, err := egressCipher(key)
	if err != nil {
		return nil, err
	}
	if nodeID == "" || password == "" {
		return nil, ErrInvalidEgressCiphertext
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	envelope := append([]byte{1}, nonce...)
	return a.Seal(envelope, nonce, []byte(password), []byte("codex-gateway/egress-password/v1\x00"+nodeID)), nil
}

func DecryptEgressPassword(key []byte, nodeID string, envelope []byte) (string, error) {
	a, err := egressCipher(key)
	if err != nil {
		return "", err
	}
	if nodeID == "" || len(envelope) < 1+a.NonceSize()+a.Overhead() || envelope[0] != 1 {
		return "", ErrInvalidEgressCiphertext
	}
	plaintext, err := a.Open(nil, envelope[1:1+a.NonceSize()], envelope[1+a.NonceSize():], []byte("codex-gateway/egress-password/v1\x00"+nodeID))
	if err != nil {
		return "", ErrInvalidEgressCiphertext
	}
	defer clear(plaintext)
	return string(plaintext), nil
}
