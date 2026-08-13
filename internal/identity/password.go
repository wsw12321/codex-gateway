package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemory      uint32 = 64 * 1024
	passwordIterations  uint32 = 3
	passwordParallelism uint8  = 2
	passwordSaltBytes          = 16
	passwordHashBytes   uint32 = 32
	maxPasswordBytes           = 512
	minPasswordRunes           = 8
	maxPasswordRunes           = 128
	maxPHCMemory        uint32 = 128 * 1024
	maxPHCIterations    uint32 = 10
	maxPHCParallelism   uint8  = 8
	maxPHCSaltBytes            = 64
	maxPHCHashBytes            = 64
)

var (
	ErrInvalidPassword = errors.New("invalid password")
	ErrInvalidHash     = errors.New("invalid password hash")
	ErrHashBusy        = errors.New("password hashing capacity exhausted")
	passwordSlots      = make(chan struct{}, 2)
)

type passwordParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	salt        []byte
	hash        []byte
}

func validatePassword(password string) error {
	if !utf8.ValidString(password) || len(password) > maxPasswordBytes {
		return ErrInvalidPassword
	}
	runes := utf8.RuneCountInString(password)
	if runes < minPasswordRunes || runes > maxPasswordRunes {
		return ErrInvalidPassword
	}
	return nil
}

func acquirePasswordSlot() (func(), error) {
	select {
	case passwordSlots <- struct{}{}:
		return func() { <-passwordSlots }, nil
	default:
		return nil, ErrHashBusy
	}
}

func hashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	release, err := acquirePasswordSlot()
	if err != nil {
		return "", err
	}
	defer release()
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, passwordIterations, passwordMemory, passwordParallelism, passwordHashBytes)
	return encodePasswordHash(passwordParams{
		memory: passwordMemory, iterations: passwordIterations, parallelism: passwordParallelism,
		salt: salt, hash: hash,
	}), nil
}

func verifyPassword(password, encoded string) (valid, needsRehash bool, err error) {
	params, err := parsePasswordHash(encoded)
	if err != nil {
		return false, false, err
	}
	release, err := acquirePasswordSlot()
	if err != nil {
		return false, false, err
	}
	defer release()
	actual := argon2.IDKey([]byte(password), params.salt, params.iterations, params.memory, params.parallelism, uint32(len(params.hash)))
	valid = subtle.ConstantTimeCompare(actual, params.hash) == 1
	needsRehash = valid && (params.memory != passwordMemory || params.iterations != passwordIterations ||
		params.parallelism != passwordParallelism || len(params.salt) != passwordSaltBytes || len(params.hash) != int(passwordHashBytes))
	return valid, needsRehash, nil
}

func encodePasswordHash(params passwordParams) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		params.memory, params.iterations, params.parallelism,
		b64.EncodeToString(params.salt), b64.EncodeToString(params.hash))
}

func parsePasswordHash(encoded string) (passwordParams, error) {
	if len(encoded) < 64 || len(encoded) > 512 {
		return passwordParams{}, ErrInvalidHash
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return passwordParams{}, ErrInvalidHash
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 || !strings.HasPrefix(parameters[0], "m=") ||
		!strings.HasPrefix(parameters[1], "t=") || !strings.HasPrefix(parameters[2], "p=") {
		return passwordParams{}, ErrInvalidHash
	}
	memory, memoryErr := strconv.ParseUint(strings.TrimPrefix(parameters[0], "m="), 10, 32)
	iterations, iterationsErr := strconv.ParseUint(strings.TrimPrefix(parameters[1], "t="), 10, 32)
	parallel, parallelErr := strconv.ParseUint(strings.TrimPrefix(parameters[2], "p="), 10, 8)
	if memoryErr != nil || iterationsErr != nil || parallelErr != nil ||
		memory < 8*1024 || memory > uint64(maxPHCMemory) || iterations == 0 || iterations > uint64(maxPHCIterations) ||
		parallel == 0 || parallel > uint64(maxPHCParallelism) {
		return passwordParams{}, ErrInvalidHash
	}
	p := passwordParams{memory: uint32(memory), iterations: uint32(iterations), parallelism: uint8(parallel)}
	b64 := base64.RawStdEncoding
	var err error
	if p.salt, err = b64.DecodeString(parts[4]); err != nil || len(p.salt) < 8 || len(p.salt) > maxPHCSaltBytes {
		return passwordParams{}, ErrInvalidHash
	}
	if p.hash, err = b64.DecodeString(parts[5]); err != nil || len(p.hash) < 16 || len(p.hash) > maxPHCHashBytes {
		return passwordParams{}, ErrInvalidHash
	}
	return p, nil
}

func hashPasswordContext(ctx context.Context, password string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hashPassword(password)
}
