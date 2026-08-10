package security

import (
	"bytes"
	"testing"
)

func FuzzParseAPIKey(f *testing.F) {
	generated, err := GenerateAPIKeyFrom(bytes.NewReader(deterministicEntropy(APIKeyPublicIDBytes + APIKeySecretBytes)))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(generated.Token)
	f.Add("")
	f.Add("cgk_v1_bad_bad")
	f.Fuzz(func(t *testing.T, raw string) {
		parsed, err := ParseAPIKey(raw)
		if err != nil {
			return
		}
		if len(raw) != len(APIKeyVersionPrefix)+16+1+43 || parsed.Prefix() != raw[:len(APIKeyVersionPrefix)+16+1] {
			t.Fatalf("accepted non-canonical token %q", raw)
		}
		digest, err := HashParsedAPIKey(make([]byte, MinimumPepperBytes), parsed)
		if err != nil {
			t.Fatal(err)
		}
		verified, ok, err := VerifyAPIKey(make([]byte, MinimumPepperBytes), raw, digest)
		if err != nil || !ok || verified.PublicID != parsed.PublicID {
			t.Fatalf("valid parse did not round-trip: %v, %v", ok, err)
		}
	})
}

func FuzzOpaqueTokenParsing(f *testing.F) {
	generated, err := GenerateOpaqueTokenFrom(InvitationToken, bytes.NewReader(deterministicEntropy(OpaqueTokenBytes)))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(uint8(InvitationToken), generated.Token)
	f.Add(uint8(SessionToken), "")
	f.Fuzz(func(t *testing.T, rawKind uint8, token string) {
		kind := TokenKind(rawKind)
		digest, err := DigestOpaqueToken(kind, token)
		if err == nil && !VerifyOpaqueToken(kind, token, digest) {
			t.Fatal("successfully parsed token did not verify")
		}
	})
}

func FuzzRedactTextIsIdempotent(f *testing.F) {
	f.Add("Authorization: Bearer secret")
	f.Add("https://example.test/#access_token=secret")
	f.Add("plain metadata")
	f.Fuzz(func(t *testing.T, value string) {
		redacted := RedactText(value)
		if again := RedactText(redacted); again != redacted {
			t.Fatalf("redaction was not idempotent: %q then %q", redacted, again)
		}
	})
}
