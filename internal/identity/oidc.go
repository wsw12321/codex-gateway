package identity

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/wsw/codex-gateway/internal/config"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

const (
	oidcRequestTimeout = 10 * time.Second
	oidcResponseLimit  = 1 << 20
)

// These errors intentionally exclude provider bodies, URLs, authorization codes,
// tokens and credentials, so callers can safely log an operation's outcome.
var (
	ErrOIDCUnavailable = errors.New("identity provider is unavailable")
	ErrOIDCRejected    = errors.New("external authentication was rejected")
)

// OIDCIdentity contains only verified stable identifiers and a display hint.
// Access and refresh tokens never leave the exchange operation.
type OIDCIdentity struct {
	Issuer      string
	Subject     string
	MaskedEmail string
}

// OIDC is a fixed-issuer confidential client. Construction is offline; discovery
// is fetched once on demand. The provider retains a concurrent, rotating JWKS cache.
type OIDC struct {
	issuer           *url.URL
	authorizationURL string
	clientID         string
	clientSecret     string
	redirectURL      string
	client           *http.Client

	mu          sync.Mutex
	provider    *oidc.Provider
	verifier    *oidc.IDTokenVerifier
	oauth       *oauth2.Config
	retryAfter  time.Time
	discovering singleflight.Group
}

func NewOIDC(cfg config.Config) (*OIDC, error) {
	if !cfg.OIDCEnabled {
		return nil, nil
	}
	if err := cfg.ValidateOIDC(); err != nil {
		return nil, err
	}
	issuer, _ := url.Parse(cfg.OIDCIssuer)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(cfg.OIDCProxyURL)
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.ResponseHeaderTimeout = 5 * time.Second
	transport.MaxResponseHeaderBytes = 32 << 10
	transport.MaxConnsPerHost = 8
	transport.MaxIdleConnsPerHost = 2
	return &OIDC{
		issuer: issuer, clientID: cfg.OIDCClientID, clientSecret: cfg.OIDCClientSecret,
		authorizationURL: cfg.OIDCAuthorizationURL,
		redirectURL:      cfg.PublicURL.Scheme + "://" + cfg.PublicURL.Host + "/auth/oidc/callback",
		client: &http.Client{
			Transport: &oidcTransport{issuer: issuer, base: transport},
			Timeout:   oidcRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("OIDC redirects are forbidden")
			},
		},
	}, nil
}

// AuthorizationURL must receive fresh cryptographically random state, nonce and
// verifier from the caller's purpose-bound, one-use browser transaction.
func (o *OIDC) AuthorizationURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	if !validOIDCRandom(state, 32) || !validOIDCRandom(nonce, 32) || !validOIDCRandom(verifier, 43) {
		return "", ErrOIDCRejected
	}
	if err := o.discover(ctx); err != nil {
		return "", err
	}
	return o.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// Exchange is called only after the caller atomically consumes and validates its
// browser transaction. It sends one code request with no auth-method retry.
func (o *OIDC) Exchange(ctx context.Context, code, nonce, verifier string) (OIDCIdentity, error) {
	if code == "" || len(code) > 8192 || strings.ContainsAny(code, "\r\n\x00") ||
		!validOIDCRandom(nonce, 32) || !validOIDCRandom(verifier, 43) {
		return OIDCIdentity{}, ErrOIDCRejected
	}
	if err := o.discover(ctx); err != nil {
		return OIDCIdentity{}, err
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, o.client), oidcRequestTimeout)
	defer cancel()
	token, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return OIDCIdentity{}, ErrOIDCRejected
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" || len(raw) > 64<<10 {
		return OIDCIdentity{}, ErrOIDCRejected
	}
	id, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return OIDCIdentity{}, ErrOIDCRejected
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
		CodeHash        string `json:"c_hash"`
		Email           string `json:"email"`
	}
	if err := id.Claims(&claims); err != nil {
		return OIDCIdentity{}, ErrOIDCRejected
	}
	now := time.Now()
	if id.Subject == "" || len(id.Subject) > 255 || strings.IndexFunc(id.Subject, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 ||
		subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 ||
		(len(id.Audience) > 1 && claims.AuthorizedParty == "") ||
		(claims.AuthorizedParty != "" && claims.AuthorizedParty != o.clientID) ||
		id.IssuedAt.IsZero() || id.IssuedAt.After(now.Add(time.Minute)) ||
		id.IssuedAt.Before(now.Add(-11*time.Minute)) || !id.Expiry.After(id.IssuedAt) {
		return OIDCIdentity{}, ErrOIDCRejected
	}
	if id.AccessTokenHash != "" && (token.AccessToken == "" || id.VerifyAccessToken(token.AccessToken) != nil) {
		return OIDCIdentity{}, ErrOIDCRejected
	}
	// Both explicitly supported algorithms use SHA-256 for OIDC hash claims.
	if claims.CodeHash != "" {
		hash := sha256.Sum256([]byte(code))
		expected := base64.RawURLEncoding.EncodeToString(hash[:16])
		if subtle.ConstantTimeCompare([]byte(claims.CodeHash), []byte(expected)) != 1 {
			return OIDCIdentity{}, ErrOIDCRejected
		}
	}
	return OIDCIdentity{Issuer: id.Issuer, Subject: id.Subject, MaskedEmail: maskOIDCEmail(claims.Email)}, nil
}

func (o *OIDC) discover(ctx context.Context) error {
	o.mu.Lock()
	ready := o.provider != nil
	cooling := time.Now().Before(o.retryAfter)
	o.mu.Unlock()
	if ready {
		return nil
	}
	if cooling {
		return ErrOIDCUnavailable
	}
	// Shared discovery has its own deadline: one disconnected browser must not
	// cancel other callers, and neither discovery nor detached JWKS can hang.
	result := o.discovering.DoChan("discovery", func() (any, error) {
		o.mu.Lock()
		ready, cooling := o.provider != nil, time.Now().Before(o.retryAfter)
		o.mu.Unlock()
		if ready {
			return nil, nil
		}
		if cooling {
			return nil, ErrOIDCUnavailable
		}
		discoveryCtx, cancel := context.WithTimeout(oidc.ClientContext(context.Background(), o.client), oidcRequestTimeout)
		defer cancel()
		provider, err := oidc.NewProvider(discoveryCtx, o.issuer.String())
		if err == nil {
			err = o.configureProvider(provider)
		}
		if err != nil {
			o.mu.Lock()
			o.retryAfter = time.Now().Add(5 * time.Second)
			o.mu.Unlock()
			return nil, ErrOIDCUnavailable
		}
		return nil, nil
	})
	select {
	case <-ctx.Done():
		return ErrOIDCUnavailable
	case value := <-result:
		return value.Err
	}
}

func (o *OIDC) configureProvider(provider *oidc.Provider) error {
	var metadata struct {
		JWKSURL     string   `json:"jwks_uri"`
		Algorithms  []string `json:"id_token_signing_alg_values_supported"`
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
		PKCEMethods []string `json:"code_challenge_methods_supported"`
	}
	if provider.Claims(&metadata) != nil {
		return ErrOIDCUnavailable
	}
	endpoint := provider.Endpoint()
	for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL, metadata.JWKSURL} {
		u, err := url.Parse(raw)
		if err != nil || !allowedOIDCURL(o.issuer, u) {
			return ErrOIDCUnavailable
		}
	}
	algorithms := make([]string, 0, 2)
	for _, alg := range []string{oidc.RS256, oidc.ES256} {
		if slices.Contains(metadata.Algorithms, alg) {
			algorithms = append(algorithms, alg)
		}
	}
	if len(algorithms) == 0 ||
		(len(metadata.AuthMethods) > 0 && !slices.Contains(metadata.AuthMethods, "client_secret_basic")) ||
		(len(metadata.PKCEMethods) > 0 && !slices.Contains(metadata.PKCEMethods, "S256")) {
		return ErrOIDCUnavailable
	}
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	// The account center may proxy browser authorization. Its deployment-owned
	// URL changes only the redirect destination, after validating discovery;
	// token exchange and JWKS remain restricted to the trusted issuer.
	if o.authorizationURL != "" {
		endpoint.AuthURL = o.authorizationURL
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: o.clientID, SupportedSigningAlgs: algorithms})
	o.mu.Lock()
	defer o.mu.Unlock()
	o.oauth = &oauth2.Config{
		ClientID: o.clientID, ClientSecret: o.clientSecret, RedirectURL: o.redirectURL,
		Endpoint: endpoint, Scopes: []string{oidc.ScopeOpenID, "email"},
	}
	o.verifier = verifier
	o.provider = provider
	return nil
}

func validOIDCRandom(s string, minimum int) bool {
	if len(s) < minimum || len(s) > 128 {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._~", r))
	}) < 0
}

func maskOIDCEmail(value string) string {
	if len(value) > 320 {
		return ""
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value {
		return ""
	}
	local, domain, ok := strings.Cut(value, "@")
	if !ok || local == "" || domain == "" || strings.ContainsAny(value, "<>\r\n") {
		return ""
	}
	first := []rune(local)
	if len(first) == 1 {
		return "***@" + domain
	}
	return string(first[0]) + "***@" + domain
}

// oidcTransport enforces the same destination policy for discovery, token and
// JWKS requests, including requests that the verifier makes in the background.
type oidcTransport struct {
	issuer *url.URL
	base   http.RoundTripper
}

func allowedOIDCURL(issuer, u *url.URL) bool {
	return u != nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), issuer.Hostname()) &&
		(u.Port() == "" || u.Port() == "443") && u.User == nil && u.Opaque == "" &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawFragment == "" && u.RawPath == "" &&
		path.Clean(u.Path) == u.Path && strings.HasPrefix(u.Path, strings.TrimSuffix(issuer.Path, "/")+"/") &&
		!strings.ContainsAny(u.Path, "%\\") && u.EscapedPath() == u.Path
}

func (t *oidcTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !allowedOIDCURL(t.issuer, req.URL) || (req.Method != http.MethodGet && req.Method != http.MethodPost) {
		return nil, ErrOIDCUnavailable
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, ErrOIDCUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 || response.ContentLength > oidcResponseLimit {
		return nil, ErrOIDCUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, oidcResponseLimit+1))
	if err != nil || len(body) > oidcResponseLimit {
		return nil, ErrOIDCUnavailable
	}
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	response.ContentLength = int64(len(body))
	return response, nil
}
