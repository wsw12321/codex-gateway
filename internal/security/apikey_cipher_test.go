package security

import (
	"bytes"
	"errors"
	"testing"
)

func TestAPIKeySecretEncryptionRoundTrip(t *testing.T) {
	generated, err := GenerateAPIKeyFrom(bytes.NewReader(bytes.Repeat(
		[]byte{0x42}, APIKeyPublicIDBytes+APIKeySecretBytes,
	)))
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x7a}, APIKeyEncryptionKeyBytes)
	ciphertext, err := EncryptAPIKeySecretFrom(
		bytes.NewReader(bytes.Repeat([]byte{0x11}, 12)),
		key, "user-id", generated.PublicID, generated.Token,
	)
	if err != nil {
		t.Fatalf("EncryptAPIKeySecretFrom: %v", err)
	}
	if len(ciphertext) <= len(generated.Token) || ciphertext[0] != apiKeyCipherVersion {
		t.Fatalf("unexpected ciphertext envelope: %x", ciphertext)
	}
	plaintext, err := DecryptAPIKeySecret(key, "user-id", generated.PublicID, ciphertext)
	if err != nil {
		t.Fatalf("DecryptAPIKeySecret: %v", err)
	}
	if plaintext != generated.Token {
		t.Fatalf("plaintext mismatch: got %q", plaintext)
	}
}

func TestAPIKeySecretEncryptionRejectsInvalidInputs(t *testing.T) {
	generated, err := GenerateAPIKeyFrom(bytes.NewReader(bytes.Repeat(
		[]byte{0x24}, APIKeyPublicIDBytes+APIKeySecretBytes,
	)))
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x5c}, APIKeyEncryptionKeyBytes)

	if _, err := EncryptAPIKeySecret(key[:31], "user-id", generated.PublicID, generated.Token); !errors.Is(err, ErrInvalidAPIKeyEncryptionKey) {
		t.Fatalf("short encryption key error = %v", err)
	}
	if _, err := EncryptAPIKeySecret(key, "user-id", "different-public-id", generated.Token); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("mismatched public ID error = %v", err)
	}
	if _, err := EncryptAPIKeySecret(key, "user-id", generated.PublicID, "not-an-api-key"); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("invalid plaintext error = %v", err)
	}
}

func TestAPIKeySecretDecryptionFailsClosed(t *testing.T) {
	generated, err := GenerateAPIKeyFrom(bytes.NewReader(bytes.Repeat(
		[]byte{0x63}, APIKeyPublicIDBytes+APIKeySecretBytes,
	)))
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x18}, APIKeyEncryptionKeyBytes)
	ciphertext, err := EncryptAPIKeySecretFrom(
		bytes.NewReader(bytes.Repeat([]byte{0x92}, 12)),
		key, "user-id", generated.PublicID, generated.Token,
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		key        []byte
		userID     string
		publicID   string
		ciphertext []byte
	}{
		{name: "wrong key", key: bytes.Repeat([]byte{0x19}, APIKeyEncryptionKeyBytes), userID: "user-id", publicID: generated.PublicID, ciphertext: ciphertext},
		{name: "wrong user AAD", key: key, userID: "other-user", publicID: generated.PublicID, ciphertext: ciphertext},
		{name: "wrong public ID AAD", key: key, userID: "user-id", publicID: "other-public-id", ciphertext: ciphertext},
		{name: "truncated", key: key, userID: "user-id", publicID: generated.PublicID, ciphertext: ciphertext[:8]},
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0x80
	tests = append(tests, struct {
		name       string
		key        []byte
		userID     string
		publicID   string
		ciphertext []byte
	}{name: "tampered", key: key, userID: "user-id", publicID: generated.PublicID, ciphertext: tampered})
	badVersion := append([]byte(nil), ciphertext...)
	badVersion[0]++
	tests = append(tests, struct {
		name       string
		key        []byte
		userID     string
		publicID   string
		ciphertext []byte
	}{name: "unknown version", key: key, userID: "user-id", publicID: generated.PublicID, ciphertext: badVersion})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecryptAPIKeySecret(test.key, test.userID, test.publicID, test.ciphertext); !errors.Is(err, ErrInvalidAPIKeyCiphertext) {
				t.Fatalf("DecryptAPIKeySecret error = %v", err)
			}
		})
	}
}
