package security

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func deterministicEntropy(size int) []byte {
	value := make([]byte, size)
	for i := range value {
		value[i] = byte(i*37 + 11)
	}
	return value
}

func TestAPIKeyGenerateParseHashAndVerify(t *testing.T) {
	generated, err := GenerateAPIKeyFrom(bytes.NewReader(deterministicEntropy(APIKeyPublicIDBytes + APIKeySecretBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.Token) != len(APIKeyVersionPrefix)+16+1+43 {
		t.Fatalf("unexpected token length: %d", len(generated.Token))
	}
	if !strings.HasPrefix(generated.Token, generated.Prefix) || strings.ContainsAny(generated.Token, "+/=") {
		t.Fatalf("token is not canonical URL-safe base64: %q", generated.Token)
	}

	parsed, err := ParseAPIKey(generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PublicID != generated.PublicID || parsed.Prefix() != generated.Prefix {
		t.Fatalf("parsed lookup fields differ: %#v %#v", parsed, generated)
	}
	if got, err := APIKeyPrefixForLog(generated.Token); err != nil || got != generated.Prefix {
		t.Fatalf("log prefix = %q, %v", got, err)
	}
	if strings.Contains(fmt.Sprintf("%v %#v", generated, parsed), generated.Token[len(generated.Prefix):]) {
		t.Fatal("fmt formatting leaked the API key secret")
	}
	if _, err := HashParsedAPIKey(deterministicEntropy(MinimumPepperBytes), ParsedAPIKey{PublicID: generated.PublicID}); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("manually forged parsed key error = %v", err)
	}

	pepper := deterministicEntropy(MinimumPepperBytes)
	digest, err := HashAPIKey(pepper, generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	verified, ok, err := VerifyAPIKey(pepper, generated.Token, digest)
	if err != nil || !ok || verified.PublicID != generated.PublicID {
		t.Fatalf("verification = %#v, %v, %v", verified, ok, err)
	}
	if ok, err := VerifyParsedAPIKey(pepper, parsed, digest); err != nil || !ok {
		t.Fatalf("parsed verification = %v, %v", ok, err)
	}

	wrong := digest
	wrong[0] ^= 0xff
	if _, ok, err := VerifyAPIKey(pepper, generated.Token, wrong); err != nil || ok {
		t.Fatalf("wrong digest verification = %v, %v", ok, err)
	}
	otherPepper := append([]byte(nil), pepper...)
	otherPepper[0] ^= 0xff
	if _, ok, err := VerifyAPIKey(otherPepper, generated.Token, digest); err != nil || ok {
		t.Fatalf("wrong pepper verification = %v, %v", ok, err)
	}
	if _, _, err := VerifyAPIKey(make([]byte, MinimumPepperBytes-1), generated.Token, digest); !errors.Is(err, ErrPepperTooShort) {
		t.Fatalf("short pepper error = %v", err)
	}
}

func TestParseAPIKeyStrict(t *testing.T) {
	generated, err := GenerateAPIKeyFrom(bytes.NewReader(deterministicEntropy(APIKeyPublicIDBytes + APIKeySecretBytes)))
	if err != nil {
		t.Fatal(err)
	}
	tests := []string{
		"",
		generated.Token + " ",
		" " + generated.Token,
		strings.ToUpper(APIKeyVersionPrefix) + generated.Token[len(APIKeyVersionPrefix):],
		strings.Replace(generated.Token, "_", "-", 1),
		generated.Token + "=",
		generated.Token[:len(generated.Token)-1],
		generated.Token[:len(APIKeyVersionPrefix)+16] + "__" + generated.Token[len(APIKeyVersionPrefix)+17:],
	}
	for _, raw := range tests {
		if _, err := ParseAPIKey(raw); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("ParseAPIKey(%q) error = %v", raw, err)
		}
		if _, ok, err := VerifyAPIKey(deterministicEntropy(MinimumPepperBytes), raw, APIKeyDigest{}); err != nil || ok {
			t.Errorf("VerifyAPIKey(%q) = %v, %v", raw, ok, err)
		}
	}
}

func TestGenerateAPIKeyReaderFailures(t *testing.T) {
	if _, err := GenerateAPIKeyFrom(nil); err == nil {
		t.Fatal("nil reader was accepted")
	}
	if _, err := GenerateAPIKeyFrom(bytes.NewReader(make([]byte, APIKeyPublicIDBytes))); !errors.Is(err, io.EOF) {
		t.Fatalf("short reader error = %v", err)
	}
}

func TestOpaqueTokensArePurposeBoundAndURLSafe(t *testing.T) {
	kinds := []TokenKind{InvitationToken, SessionToken, RecoveryToken}
	generated := make([]GeneratedToken, len(kinds))
	for i, kind := range kinds {
		var err error
		generated[i], err = GenerateOpaqueTokenFrom(kind, bytes.NewReader(deterministicEntropy(OpaqueTokenBytes)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(generated[i].Token, "+/=%#&?") {
			t.Fatalf("token is unsafe in a URL fragment: %q", generated[i].Token)
		}
		if !VerifyOpaqueToken(kind, generated[i].Token, generated[i].Digest) {
			t.Fatalf("token kind %d did not verify", kind)
		}
	}
	if generated[0].Digest == generated[1].Digest || generated[1].Digest == generated[2].Digest {
		t.Fatal("purpose domains did not separate token digests")
	}
	if VerifyOpaqueToken(SessionToken, generated[0].Token, generated[0].Digest) {
		t.Fatal("invitation token verified as a session token")
	}
	if _, err := DigestOpaqueToken(TokenKind(255), generated[0].Token); !errors.Is(err, ErrInvalidTokenKind) {
		t.Fatalf("invalid kind error = %v", err)
	}
}

func TestPepperTokenDigest(t *testing.T) {
	generated, err := GenerateOpaqueTokenFrom(SessionToken, bytes.NewReader(deterministicEntropy(OpaqueTokenBytes)))
	if err != nil {
		t.Fatal(err)
	}
	pepper := deterministicEntropy(MinimumPepperBytes)
	first, err := PepperTokenDigest(pepper, generated.Digest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PepperTokenDigest(pepper, generated.Digest)
	if err != nil || first != second || first == generated.Digest {
		t.Fatalf("unexpected peppered digest: equal=%v raw=%v err=%v", first == second, first == generated.Digest, err)
	}
	if _, err := PepperTokenDigest(pepper[:MinimumPepperBytes-1], generated.Digest); !errors.Is(err, ErrTokenPepperTooShort) {
		t.Fatalf("short token pepper error = %v", err)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodesFrom(bytes.NewReader(deterministicEntropy(RecoveryCodeCount * RecoveryCodeBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes", len(codes))
	}
	seen := make(map[string]struct{}, len(codes))
	for _, generated := range codes {
		if len(generated.Code) != 29 || strings.Count(generated.Code, "-") != 5 {
			t.Fatalf("bad recovery code format: %q", generated.Code)
		}
		if _, duplicate := seen[generated.Code]; duplicate {
			t.Fatalf("duplicate recovery code: %q", generated.Code)
		}
		seen[generated.Code] = struct{}{}
		if !VerifyRecoveryCode(generated.Code, generated.Digest) {
			t.Fatalf("recovery code did not verify: %q", generated.Code)
		}
		if VerifyRecoveryCode(strings.ToLower(generated.Code), generated.Digest) {
			t.Fatal("non-canonical lowercase recovery code was accepted")
		}
	}

	if _, err := GenerateRecoveryCodesFrom(bytes.NewReader(make([]byte, RecoveryCodeBytes*2))); !errors.Is(err, ErrRandomCollision) {
		t.Fatalf("broken entropy collision error = %v", err)
	}
}

func TestRedaction(t *testing.T) {
	key, err := GenerateAPIKeyFrom(bytes.NewReader(deterministicEntropy(APIKeyPublicIDBytes + APIKeySecretBytes)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := GenerateOpaqueTokenFrom(InvitationToken, bytes.NewReader(deterministicEntropy(OpaqueTokenBytes)))
	if err != nil {
		t.Fatal(err)
	}
	codes, err := GenerateRecoveryCodesFrom(bytes.NewReader(deterministicEntropy(RecoveryCodeCount * RecoveryCodeBytes)))
	if err != nil {
		t.Fatal(err)
	}
	secretValues := []string{key.Token, token.Token, codes[0].Code, "bearer-secret", "refresh-secret"}
	text := "Authorization: Bearer bearer-secret\nurl=https://example.test/#token=" + token.Token +
		"&refresh_token=refresh-secret key=" + key.Token + " recovery=" + codes[0].Code
	redacted := RedactText(text)
	for _, secret := range secretValues {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted text leaked %q: %s", secret, redacted)
		}
	}
	if RedactText(redacted) != redacted {
		t.Fatalf("redaction is not idempotent: %q", redacted)
	}

	headers := http.Header{
		"Authorization": []string{"Bearer " + key.Token},
		"Cookie":        []string{"session=" + token.Token},
		"X-Request-ID":  []string{"safe", "embedded " + key.Token},
	}
	safe := RedactedHeaders(headers)
	if got := safe.Get("Authorization"); got != RedactedValue {
		t.Fatalf("Authorization = %q", got)
	}
	if got := safe.Get("Cookie"); got != RedactedValue {
		t.Fatalf("Cookie = %q", got)
	}
	if strings.Contains(safe["X-Request-Id"][1], key.Token) {
		t.Fatalf("embedded key leaked: %#v", safe)
	}
	if headers.Get("Authorization") == RedactedValue {
		t.Fatal("input headers were mutated")
	}
	if !IsSensitiveHeader("  aUtHoRiZaTiOn ") || IsSensitiveHeader("X-Request-ID") {
		t.Fatal("sensitive header classification is wrong")
	}
	names := SensitiveHeaderNames()
	names[0] = "Mutated"
	if IsSensitiveHeader("Mutated") {
		t.Fatal("SensitiveHeaderNames exposed mutable package state")
	}
}
