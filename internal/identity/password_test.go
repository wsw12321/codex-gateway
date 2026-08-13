package identity

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestValidatePasswordBoundaries(t *testing.T) {
	for _, value := range []string{"12345678", "        ", strings.Repeat("密", 128), " abcdef "} {
		if err := validatePassword(value); err != nil {
			t.Errorf("valid password %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"1234567", strings.Repeat("a", 129), strings.Repeat("密", 129), string([]byte{0xff})} {
		if err := validatePassword(value); !errors.Is(err, ErrInvalidPassword) {
			t.Errorf("invalid password accepted: %q", value)
		}
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	first, err := hashPassword(" 密码 pass 123 ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashPassword(" 密码 pass 123 ")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("password hashes reused a salt")
	}
	valid, rehash, err := verifyPassword(" 密码 pass 123 ", first)
	if err != nil || !valid || rehash {
		t.Fatalf("verify = %v, %v, %v", valid, rehash, err)
	}
	valid, _, err = verifyPassword("密码 pass 123", first)
	if err != nil || valid {
		t.Fatalf("trimmed password verify = %v, %v", valid, err)
	}
}

func TestPasswordHashParserRejectsUnsafeValues(t *testing.T) {
	values := []string{
		"", "$argon2i$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=16$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=1048576,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=99,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=99$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for _, value := range values {
		if _, err := parsePasswordHash(value); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("unsafe PHC accepted: %q (%v)", value, err)
		}
	}
}

func TestPasswordVerificationRequestsParameterUpgrade(t *testing.T) {
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte("upgrade password"), salt, 1, 8*1024, 1, passwordHashBytes)
	encoded := encodePasswordHash(passwordParams{
		memory: 8 * 1024, iterations: 1, parallelism: 1, salt: salt, hash: hash,
	})
	valid, rehash, err := verifyPassword("upgrade password", encoded)
	if err != nil || !valid || !rehash {
		t.Fatalf("verify old parameters = %v, %v, %v", valid, rehash, err)
	}
}

func TestDummyPasswordHashHasCurrentCost(t *testing.T) {
	params, err := parsePasswordHash(dummyPasswordHash)
	if err != nil {
		t.Fatal(err)
	}
	if params.memory != passwordMemory || params.iterations != passwordIterations || params.parallelism != passwordParallelism {
		t.Fatalf("dummy parameters = m=%d,t=%d,p=%d", params.memory, params.iterations, params.parallelism)
	}
	valid, _, err := verifyPassword("not the dummy value", dummyPasswordHash)
	if err != nil || valid {
		t.Fatalf("dummy verify = %v, %v", valid, err)
	}
}

func TestPasswordHashSlotsFailFast(t *testing.T) {
	passwordSlots <- struct{}{}
	passwordSlots <- struct{}{}
	defer func() { <-passwordSlots; <-passwordSlots }()
	if _, err := hashPassword("12345678"); !errors.Is(err, ErrHashBusy) {
		t.Fatalf("hash error = %v", err)
	}
	if _, _, err := verifyPassword("12345678", dummyPasswordHash); !errors.Is(err, ErrHashBusy) {
		t.Fatalf("verify error = %v", err)
	}
}
