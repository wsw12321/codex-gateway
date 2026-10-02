package identity

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/wsw/codex-gateway/internal/config"
	"golang.org/x/oauth2"
)

type oidcRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oidcRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type oidcFixture struct {
	t          *testing.T
	client     *OIDC
	key        crypto.Signer
	signingKey any
	algorithm  jose.SignatureAlgorithm
	rawToken   string
	kid        string
	claims     map[string]any
	metadata   map[string]any
	nonce      string
	verifier   string
	code       string
	tokenError bool
	mu         sync.Mutex
	requests   map[string]int
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	issuer := "https://project.supabase.co/auth/v1"
	publicURL, _ := url.Parse("https://gateway.example.test")
	proxyURL, _ := url.Parse("http://172.28.30.4:3128")
	client, err := NewOIDC(config.Config{
		OIDCEnabled: true, OIDCIssuer: issuer, OIDCClientID: "gateway-client", OIDCClientSecret: "backend-only-secret",
		OIDCProxyURL: proxyURL, PublicURL: publicURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &oidcFixture{
		t: t, client: client, key: key, kid: "key1", nonce: oauth2.GenerateVerifier(),
		verifier: oauth2.GenerateVerifier(), code: "one-use-code", requests: make(map[string]int),
		metadata: map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/oauth/authorize",
			"token_endpoint": issuer + "/oauth/token", "jwks_uri": issuer + "/.well-known/jwks.json",
			"id_token_signing_alg_values_supported": []string{"ES256", "RS256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
			"code_challenge_methods_supported":      []string{"S256"},
		},
	}
	f.claims = map[string]any{
		"iss": issuer, "sub": "external-user", "aud": "gateway-client", "nonce": f.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "email": "alice@example.test",
	}
	client.client.Transport.(*oidcTransport).base = oidcRoundTripFunc(f.roundTrip)
	return f
}

func (f *oidcFixture) signingAlgorithm() jose.SignatureAlgorithm {
	if f.algorithm == "" {
		return jose.ES256
	}
	return f.algorithm
}

func (f *oidcFixture) roundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[r.URL.Path]++
	var body any
	status := http.StatusOK
	switch r.URL.Path {
	case "/auth/v1/.well-known/openid-configuration":
		body = f.metadata
	case "/auth/v1/.well-known/jwks.json":
		body = jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: f.key.Public(), KeyID: f.kid, Algorithm: string(f.signingAlgorithm()), Use: "sig"}}}
	case "/auth/v1/oauth/token":
		if r.Method != http.MethodPost {
			f.t.Errorf("token request method = %s", r.Method)
		}
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != "gateway-client" || secret != "backend-only-secret" {
			f.t.Error("missing confidential client authentication")
		}
		if err := r.ParseForm(); err != nil {
			f.t.Error(err)
		}
		if r.Form.Get("code") != f.code || r.Form.Get("code_verifier") != f.verifier ||
			r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != f.client.redirectURL ||
			r.Form.Get("client_secret") != "" || r.URL.RawQuery != "" {
			status = http.StatusBadRequest
			body = map[string]string{"error": "invalid_grant"}
		} else if f.tokenError {
			status = http.StatusUnauthorized
			body = map[string]string{"error": "invalid_client", "error_description": "secret-provider-response-do-not-log"}
		} else {
			signingKey := f.signingKey
			if signingKey == nil {
				signingKey = f.key
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: f.signingAlgorithm(), Key: signingKey}, (&jose.SignerOptions{}).WithHeader("kid", f.kid))
			if err != nil {
				return nil, err
			}
			payload, _ := json.Marshal(f.claims)
			signed, err := signer.Sign(payload)
			if err != nil {
				return nil, err
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				return nil, err
			}
			if f.rawToken != "" {
				raw = f.rawToken
			}
			body = map[string]any{"access_token": "short-lived-access", "token_type": "Bearer", "id_token": raw, "refresh_token": "not-persisted"}
		}
	default:
		f.t.Errorf("unexpected outbound URL: %s", r.URL)
		return nil, errors.New("unexpected endpoint")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
}

func TestOIDCCodeFlowAndLazyDiscovery(t *testing.T) {
	f := newOIDCFixture(t)
	if len(f.requests) != 0 {
		t.Fatal("constructor performed network IO")
	}
	state := oauth2.GenerateVerifier()
	authURL, err := f.client.AuthorizationURL(context.Background(), state, f.nonce, f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("state") != state || q.Get("nonce") != f.nonce || q.Get("response_type") != "code" ||
		q.Get("scope") != "openid email" || q.Get("client_id") != "gateway-client" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(f.verifier) ||
		q.Get("redirect_uri") != "https://gateway.example.test/auth/oidc/callback" || q.Get("client_secret") != "" {
		t.Fatalf("authorization request is missing security parameters: %s", u)
	}
	id, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "external-user" || id.Issuer != f.client.issuer.String() || id.MaskedEmail != "a***@example.test" {
		t.Fatalf("unexpected verified identity: %+v", id)
	}
	if _, err := f.client.AuthorizationURL(context.Background(), state, f.nonce, f.verifier); err != nil {
		t.Fatal(err)
	}
	if f.requests["/auth/v1/.well-known/openid-configuration"] != 1 {
		t.Fatal("discovery was not cached")
	}
}

func TestOIDCRejectsInvalidClaims(t *testing.T) {
	tests := map[string]func(*oidcFixture){
		"issuer":            func(f *oidcFixture) { f.claims["iss"] = "https://attacker.test/auth/v1" },
		"audience":          func(f *oidcFixture) { f.claims["aud"] = "different-client" },
		"nonce":             func(f *oidcFixture) { f.claims["nonce"] = oauth2.GenerateVerifier() },
		"missing nonce":     func(f *oidcFixture) { delete(f.claims, "nonce") },
		"empty subject":     func(f *oidcFixture) { f.claims["sub"] = "" },
		"oversized subject": func(f *oidcFixture) { f.claims["sub"] = strings.Repeat("x", 256) },
		"expired":           func(f *oidcFixture) { f.claims["exp"] = time.Now().Add(-time.Minute).Unix() },
		"future iat":        func(f *oidcFixture) { f.claims["iat"] = time.Now().Add(2 * time.Minute).Unix() },
		"stale iat":         func(f *oidcFixture) { f.claims["iat"] = time.Now().Add(-12 * time.Minute).Unix() },
		"missing iat":       func(f *oidcFixture) { delete(f.claims, "iat") },
		"expiry before issuance": func(f *oidcFixture) {
			f.claims["iat"] = time.Now().Add(50 * time.Second).Unix()
			f.claims["exp"] = time.Now().Add(20 * time.Second).Unix()
		},
		"future nbf":                     func(f *oidcFixture) { f.claims["nbf"] = time.Now().Add(10 * time.Minute).Unix() },
		"wrong azp":                      func(f *oidcFixture) { f.claims["azp"] = "another-client" },
		"multiple audiences without azp": func(f *oidcFixture) { f.claims["aud"] = []string{"gateway-client", "another-client"} },
		"bad access token hash":          func(f *oidcFixture) { f.claims["at_hash"] = "bad-hash" },
		"bad code hash":                  func(f *oidcFixture) { f.claims["c_hash"] = "bad-hash" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newOIDCFixture(t)
			mutate(f)
			id, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier)
			if !errors.Is(err, ErrOIDCRejected) || id != (OIDCIdentity{}) {
				t.Fatalf("accepted invalid claims: %+v, %v", id, err)
			}
		})
	}
}

func TestOIDCHashClaimsAndAuthorizedParty(t *testing.T) {
	f := newOIDCFixture(t)
	accessHash := sha256.Sum256([]byte("short-lived-access"))
	codeHash := sha256.Sum256([]byte(f.code))
	f.claims["at_hash"] = base64.RawURLEncoding.EncodeToString(accessHash[:16])
	f.claims["c_hash"] = base64.RawURLEncoding.EncodeToString(codeHash[:16])
	f.claims["aud"] = []string{"gateway-client", "additional-audience"}
	f.claims["azp"] = "gateway-client"
	if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCKeyRotation(t *testing.T) {
	f := newOIDCFixture(t)
	if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.key, f.kid = key, "rotated-key"
	f.mu.Unlock()
	if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests["/auth/v1/.well-known/jwks.json"] != 2 {
		t.Fatal("unknown kid did not trigger one JWKS refresh")
	}
}

func TestOIDCNoExchangeRetryOrProviderErrorLeak(t *testing.T) {
	f := newOIDCFixture(t)
	f.tokenError = true
	_, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier)
	if err != ErrOIDCRejected || strings.Contains(err.Error(), "secret-provider") {
		t.Fatalf("provider error leaked: %v", err)
	}
	if f.requests["/auth/v1/oauth/token"] != 1 {
		t.Fatal("failed exchange was retried")
	}
}

func TestOIDCPKCEFailure(t *testing.T) {
	f := newOIDCFixture(t)
	if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, oauth2.GenerateVerifier()); err != ErrOIDCRejected {
		t.Fatalf("wrong verifier was accepted: %v", err)
	}
}

func TestOIDCRejectsUnsafeDiscovery(t *testing.T) {
	for name, value := range map[string]any{
		"issuer": "https://attacker.test/auth/v1", "jwks_uri": "https://project.supabase.co/rest/v1/data",
		"token_endpoint": "https://attacker.test/token", "authorization_endpoint": "http://project.supabase.co/auth/v1/authorize",
		"id_token_signing_alg_values_supported": []string{"HS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
		"code_challenge_methods_supported":      []string{"plain"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOIDCFixture(t)
			f.metadata[name] = value
			if _, err := f.client.AuthorizationURL(context.Background(), oauth2.GenerateVerifier(), f.nonce, f.verifier); err != ErrOIDCUnavailable {
				t.Fatalf("accepted unsafe discovery metadata: %v", err)
			}
		})
	}
}

func TestOIDCTransportRestrictions(t *testing.T) {
	issuer, _ := url.Parse("https://project.supabase.co/auth/v1")
	for _, raw := range []string{
		"http://project.supabase.co/auth/v1/token", "https://attacker.test/auth/v1/token",
		"https://project.supabase.co:8443/auth/v1/token", "https://user:password@project.supabase.co/auth/v1/token",
		"https://project.supabase.co/auth/v1/../token", "https://project.supabase.co/auth/v1/%2e%2e/token",
		"https://project.supabase.co/auth/v1/%2ftoken", "https://project.supabase.co/auth/v10/token",
		"https://project.supabase.co/auth/v1/%252e%252e/token", "https://project.supabase.co/auth/v1/%252ftoken",
		"https://project.supabase.co/auth/v1/token?client_secret=x", "https://project.supabase.co/auth/v1/token#fragment",
	} {
		t.Run(raw, func(t *testing.T) {
			transport := &oidcTransport{issuer: issuer, base: oidcRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unsafe destination reached transport")
				return nil, nil
			})}
			req, _ := http.NewRequest(http.MethodGet, raw, nil)
			if _, err := transport.RoundTrip(req); err == nil {
				t.Fatal("unsafe destination allowed")
			}
		})
	}
	for name, response := range map[string]*http.Response{
		"redirect":      {StatusCode: 302, Header: http.Header{"Location": []string{"https://attacker.test/"}}, Body: io.NopCloser(strings.NewReader(""))},
		"large length":  {StatusCode: 200, ContentLength: oidcResponseLimit + 1, Body: io.NopCloser(strings.NewReader(""))},
		"large chunked": {StatusCode: 200, ContentLength: -1, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", oidcResponseLimit+1)))},
	} {
		t.Run(name, func(t *testing.T) {
			transport := &oidcTransport{issuer: issuer, base: oidcRoundTripFunc(func(*http.Request) (*http.Response, error) { return response, nil })}
			req, _ := http.NewRequest(http.MethodGet, issuer.String()+"/token", nil)
			if _, err := transport.RoundTrip(req); err == nil {
				t.Fatal("unsafe response allowed")
			}
		})
	}
}

func TestOIDCConcurrentDiscovery(t *testing.T) {
	f := newOIDCFixture(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := f.client.AuthorizationURL(context.Background(), oauth2.GenerateVerifier(), f.nonce, f.verifier); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests["/auth/v1/.well-known/openid-configuration"] != 1 {
		t.Fatal("concurrent discovery was not coalesced")
	}
}

func TestOIDCDiscoveryTimeoutAndCooldown(t *testing.T) {
	f := newOIDCFixture(t)
	f.client.client.Timeout = 20 * time.Millisecond
	requests := 0
	f.client.client.Transport.(*oidcTransport).base = oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	start := time.Now()
	for range 2 {
		if _, err := f.client.AuthorizationURL(context.Background(), oauth2.GenerateVerifier(), f.nonce, f.verifier); err != ErrOIDCUnavailable {
			t.Fatal(err)
		}
	}
	if time.Since(start) > time.Second || requests != 1 {
		t.Fatalf("timeout/cooldown failed: elapsed %s, requests %d", time.Since(start), requests)
	}
}

func TestOIDCRSAAndInvalidSignatures(t *testing.T) {
	t.Run("RS256", func(t *testing.T) {
		f := newOIDCFixture(t)
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		f.key, f.algorithm = key, jose.RS256
		if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("wrong signature", func(t *testing.T) {
		f := newOIDCFixture(t)
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		f.signingKey = key
		if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != ErrOIDCRejected {
			t.Fatalf("wrong signature accepted: %v", err)
		}
	})
	t.Run("symmetric algorithm", func(t *testing.T) {
		f := newOIDCFixture(t)
		f.signingKey, f.algorithm = []byte(strings.Repeat("x", 32)), jose.HS256
		if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != ErrOIDCRejected {
			t.Fatalf("symmetric algorithm accepted: %v", err)
		}
	})
	t.Run("unsigned", func(t *testing.T) {
		f := newOIDCFixture(t)
		payload, _ := json.Marshal(f.claims)
		f.rawToken = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
		if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != ErrOIDCRejected {
			t.Fatalf("unsigned token accepted: %v", err)
		}
	})
}

func TestOIDCJWKSTimeout(t *testing.T) {
	f := newOIDCFixture(t)
	f.client.client.Timeout = 20 * time.Millisecond
	f.client.client.Transport.(*oidcTransport).base = oidcRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/auth/v1/.well-known/jwks.json" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return f.roundTrip(r)
	})
	start := time.Now()
	if _, err := f.client.Exchange(context.Background(), f.code, f.nonce, f.verifier); err != ErrOIDCRejected {
		t.Fatalf("expected bounded JWKS failure: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("background JWKS fetch ignored HTTP client timeout")
	}
}

func TestOIDCProductionTransportUsesOnlyDedicatedProxy(t *testing.T) {
	f := newOIDCFixture(t)
	cfg := config.Config{
		OIDCEnabled: true, OIDCIssuer: f.client.issuer.String(), OIDCClientID: "client", OIDCClientSecret: "secret",
		OIDCProxyURL: mustOIDCURL(t, "http://172.28.30.4:3128"), PublicURL: mustOIDCURL(t, "https://gateway.example.test"),
	}
	t.Setenv("HTTPS_PROXY", "http://unexpected-proxy.test:3128")
	client, err := NewOIDC(cfg)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.client.Transport.(*oidcTransport).base.(*http.Transport)
	req, _ := http.NewRequest(http.MethodGet, cfg.OIDCIssuer+"/oauth/token", nil)
	proxy, err := transport.Proxy(req)
	if err != nil || proxy.String() != cfg.OIDCProxyURL.String() {
		t.Fatalf("OIDC client inherited a global proxy: %v, %v", proxy, err)
	}
	if client.client.Timeout != oidcRequestTimeout || transport.TLSHandshakeTimeout <= 0 || transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("OIDC client lacks request deadlines")
	}
}

func mustOIDCURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
