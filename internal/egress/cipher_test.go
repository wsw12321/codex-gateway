package egress

import (
	"strings"
	"testing"
)

func TestCipherSupport(t *testing.T) {
	for _, method := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-128-gcm"} {
		if !SupportedCipher(method) {
			t.Errorf("rejected %s", method)
		}
	}
	for _, method := range []string{"", "none", "rc4-md5", "aes-128-cfb", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305", "AES-128-GCM"} {
		if SupportedCipher(method) {
			t.Errorf("accepted %q", method)
		}
	}
	list := Ciphers()
	list[0] = "mutated"
	if !SupportedCipher("aes-128-gcm") {
		t.Fatal("caller mutated the supported cipher list")
	}
}

func TestPasswordValidation(t *testing.T) {
	key := "AAECAwQFBgcICQoLDA0ODw==" // 16 bytes
	for _, password := range []string{key, key + ":" + "EBESExQVFhcYGRobHB0eHw=="} {
		if !ValidPassword(SS2022AES128GCM, password) {
			t.Errorf("rejected SS2022 key %q", password)
		}
	}
	for _, password := range []string{
		"", "plain password", "AAECAwQFBgcICQoLDA0ODw", "AAECAwQFBgcICQoLDA0ODxA=", // unpadded, 17 bytes
		"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", // 32 bytes
		key + ":", ":" + key, " " + key, key + "\n", "AAECAwQFBgcICQoLDA0ODw-_",
	} {
		if ValidPassword(SS2022AES128GCM, password) {
			t.Errorf("accepted SS2022 key %q", password)
		}
	}
	for _, password := range []string{" literal:'\"\\$`中文 ", "x", strings.Repeat("a", MaxPasswordBytes)} {
		if !ValidPassword("aes-256-gcm", password) {
			t.Errorf("rejected legacy password %q", password)
		}
	}
	for _, password := range []string{"", "a\nb", "a\rb", "a\x00b", strings.Repeat("a", MaxPasswordBytes+1), string([]byte{0xff})} {
		if ValidPassword("aes-256-gcm", password) {
			t.Errorf("accepted legacy password %q", password)
		}
	}
}
