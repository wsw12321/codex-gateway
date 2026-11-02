package security

import (
	"bytes"
	"testing"
)

func TestEgressPasswordAuthenticatedEncryption(t *testing.T) {
	key := bytes.Repeat([]byte{0x67}, 32)
	password := "ss-密钥 '\"\\$` "
	id := "738e8416-6e60-40ea-a35a-9155f6d975e7"
	first, err := EncryptEgressPassword(key, id, password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncryptEgressPassword(key, id, password)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || bytes.Contains(first, []byte(password)) {
		t.Fatal("credential was not independently encrypted")
	}
	recovered, err := DecryptEgressPassword(key, id, first)
	if err != nil || recovered != password {
		t.Fatalf("round trip: %v", err)
	}
	tampered := append([]byte(nil), first...)
	tampered[len(tampered)-1] ^= 1
	for _, input := range []struct {
		key        []byte
		id         string
		ciphertext []byte
	}{{key, "different-node", first}, {bytes.Repeat([]byte{0x68}, 32), id, first}, {key, id, tampered}, {key, id, first[:8]}, {key[:31], id, first}} {
		if _, err := DecryptEgressPassword(input.key, input.id, input.ciphertext); err == nil {
			t.Fatal("unauthenticated credential decrypted")
		}
	}
}
