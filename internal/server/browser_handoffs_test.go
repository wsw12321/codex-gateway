package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestBrowserHandoffStoreExpiryReplayCapacityAndDigestOnly(t *testing.T) {
	now := time.Now().UTC()
	entries := newBrowserHandoffStore(2)
	entry := browserHandoff{UserID: "user", SessionID: "session", APIKeyID: "key", RememberKey: true}
	code, expires, err := entries.issue(entry, now)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(now.Add(120 * time.Second)) {
		t.Fatalf("expiry = %v", expires)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(code, browserHandoffPrefix))
	if err != nil || len(raw) != 32 {
		t.Fatal("code must contain 256 random bits")
	}
	digest := sha256.Sum256([]byte(code))
	stored, ok := entries.entries[digest]
	if !ok || stored.UserID != entry.UserID || stored.SessionID != entry.SessionID || stored.APIKeyID != entry.APIKeyID || !stored.RememberKey {
		t.Fatalf("unexpected entry: %+v", stored)
	}
	second, _, err := entries.issue(entry, now)
	if err != nil || second == code {
		t.Fatal("codes are not unique")
	}
	if _, _, err := entries.issue(entry, now); !errors.Is(err, errBrowserHandoffCapacity) {
		t.Fatalf("capacity error = %v", err)
	}
	if _, ok := entries.consume(code, now); !ok {
		t.Fatal("first exchange failed")
	}
	if _, ok := entries.consume(code, now); ok {
		t.Fatal("replay accepted")
	}
	if _, ok := entries.consume(second, expires); ok {
		t.Fatal("exact expiry accepted")
	}
	if _, _, err := entries.issue(entry, expires); err != nil {
		t.Fatal("expired entries did not free capacity")
	}
	for _, invalid := range []string{"", "bad-code", browserHandoffPrefix + strings.Repeat("=", 43), strings.Repeat("a", 4096)} {
		if _, ok := entries.consume(invalid, now); ok {
			t.Fatal("invalid code accepted")
		}
	}
}

func TestBrowserHandoffConcurrentExchangeExactlyOnce(t *testing.T) {
	s, _, _ := newBrowserHandoffTestServer(t)
	code := issueBrowserHandoffForTest(t, s, false)
	var succeeded, replayed atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := exchangeBrowserHandoffForTest(s, code, "https://ai.example.test")
			switch response.Code {
			case http.StatusOK:
				succeeded.Add(1)
			case http.StatusGone:
				replayed.Add(1)
			default:
				t.Errorf("unexpected exchange status %d", response.Code)
			}
		}()
	}
	wg.Wait()
	if succeeded.Load() != 1 || replayed.Load() != 39 {
		t.Fatalf("success=%d replay=%d", succeeded.Load(), replayed.Load())
	}
}

func TestBrowserHandoffIssueAndExchange(t *testing.T) {
	s, repo, key := newBrowserHandoffTestServer(t)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	code := issueBrowserHandoffForTest(t, s, true)
	if strings.Contains(logs.String(), code) {
		t.Fatal("issuance logged code")
	}
	// Wrong/missing origins do not consume a usable code, including disabled config.
	for _, origin := range []string{"", "https://evil.example", "https://ai.example.test/", "null"} {
		response := exchangeBrowserHandoffForTest(s, code, origin)
		if response.Code != http.StatusForbidden || response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("origin %q: %d %v", origin, response.Code, response.Header())
		}
	}
	response := exchangeBrowserHandoffForTest(s, code, "https://ai.example.test")
	if response.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, body, "base_url", "api_key", "api_key_id", "remember_key", "protocol")
	if body["base_url"] != "https://gateway.example.test/v1" || body["api_key"] != key || body["api_key_id"] != repo.key.ID || body["remember_key"] != true || body["protocol"] != "responses" {
		t.Fatalf("unexpected response: %#v", body)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "https://ai.example.test" || response.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("headers: %v", response.Header())
	}
	if strings.Contains(logs.String(), code) || strings.Contains(logs.String(), key) {
		t.Fatal("exchange logs leaked credentials")
	}
	if exchangeBrowserHandoffForTest(s, code, "https://ai.example.test").Code != http.StatusGone {
		t.Fatal("replay accepted")
	}
}

func TestBrowserHandoffRejectsInvalidReferencesAtIssueAndExchange(t *testing.T) {
	changes := map[string]func(*browserHandoffTestRepository){
		"logged out":         func(r *browserHandoffTestRepository) { now := time.Now(); r.session.RevokedAt = &now },
		"session expired":    func(r *browserHandoffTestRepository) { r.session.IdleExpiresAt = time.Now().Add(-time.Second) },
		"absolute expiry":    func(r *browserHandoffTestRepository) { r.session.AbsoluteExpiresAt = time.Now().Add(-time.Second) },
		"session ownership":  func(r *browserHandoffTestRepository) { r.session.UserID = "other" },
		"account disabled":   func(r *browserHandoffTestRepository) { r.user.Status = store.StatusDisabled },
		"account ownership":  func(r *browserHandoffTestRepository) { r.user.ID = "other" },
		"key disabled":       func(r *browserHandoffTestRepository) { r.key.Status = store.StatusDisabled },
		"key expired":        func(r *browserHandoffTestRepository) { r.key.ExpiresAt = time.Now().Add(-time.Second) },
		"key ownership":      func(r *browserHandoffTestRepository) { r.key.UserID = "other" },
		"key missing":        func(r *browserHandoffTestRepository) { r.key.ID = "other" },
		"secret unavailable": func(r *browserHandoffTestRepository) { r.key.SecretAvailable = false },
		"device disabled":    func(r *browserHandoffTestRepository) { r.device.Status = store.StatusDisabled },
		"device ownership":   func(r *browserHandoffTestRepository) { r.device.UserID = "other" },
		"device mismatch":    func(r *browserHandoffTestRepository) { r.device.ID = "other" },
		"secret ownership":   func(r *browserHandoffTestRepository) { r.secret.UserID = "other" },
		"secret ID":          func(r *browserHandoffTestRepository) { r.secret.ID = "other" },
		"secret prefix":      func(r *browserHandoffTestRepository) { r.secret.KeyPrefix = "other" },
		"secret public ID":   func(r *browserHandoffTestRepository) { r.secret.PublicID = "other" },
		"secret hash":        func(r *browserHandoffTestRepository) { r.secret.KeyHash = make([]byte, 32) },
		"secret corrupt":     func(r *browserHandoffTestRepository) { r.secret.SecretCiphertext = []byte("bad") },
		"database error":     func(r *browserHandoffTestRepository) { r.err = errors.New("db unavailable") },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, repo, plaintext := newBrowserHandoffTestServer(t)
			code := issueBrowserHandoffForTest(t, s, false)
			originalUser, originalSession := repo.user, repo.session
			change(repo)
			response := exchangeBrowserHandoffForTest(s, code, "https://ai.example.test")
			if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), plaintext) {
				t.Fatalf("exchange: %d %s", response.Code, response.Body.String())
			}
			if exchangeBrowserHandoffForTest(s, code, "https://ai.example.test").Code != http.StatusGone {
				t.Fatal("failed exchange did not consume code")
			}
			request := newBrowserHandoffIssueRequest(originalUser, originalSession, repo.key.ID, false)
			recorder := httptest.NewRecorder()
			s.createBrowserHandoff(recorder, request)
			if recorder.Code != http.StatusForbidden && recorder.Code != http.StatusBadRequest {
				t.Fatalf("issue accepted invalid refs: %d", recorder.Code)
			}
		})
	}
}

func TestBrowserHandoffRecentVerificationAndDisabled(t *testing.T) {
	s, repo, _ := newBrowserHandoffTestServer(t)
	for _, age := range []time.Duration{-time.Minute, 6 * time.Minute} {
		verified := time.Now().Add(-age)
		repo.session.RecentlyVerifiedAt = &verified
		response := httptest.NewRecorder()
		s.createBrowserHandoff(response, newBrowserHandoffIssueRequest(repo.user, repo.session, repo.key.ID, false))
		if response.Code != http.StatusForbidden {
			t.Fatalf("age %v allowed", age)
		}
	}
	repo.session.RecentlyVerifiedAt = nil
	response := httptest.NewRecorder()
	s.createBrowserHandoff(response, newBrowserHandoffIssueRequest(repo.user, repo.session, repo.key.ID, false))
	if response.Code != http.StatusForbidden {
		t.Fatal("missing verification allowed")
	}
	s.config.BrowserClientURL = nil
	response = httptest.NewRecorder()
	s.createBrowserHandoff(response, newBrowserHandoffIssueRequest(repo.user, repo.session, repo.key.ID, false))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("disabled launch should not issue a code")
	}
}

func TestBrowserHandoffCORSExactEndpointsMethodsHeadersAndErrors(t *testing.T) {
	s, _, _ := newBrowserHandoffTestServer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, 401, "authentication_error", "test", "test")
	})
	handler := s.browserClientCORS(next)
	for _, test := range []struct {
		path, origin, method, requested, headers string
		status                                   int
		cors                                     bool
	}{
		{"/browser-handoffs/exchange", "https://ai.example.test", "OPTIONS", "POST", "content-type", 204, true},
		{"/v1/models", "https://ai.example.test", "OPTIONS", "GET", "authorization", 204, true},
		{"/v1/responses", "https://ai.example.test", "OPTIONS", "POST", "content-type,authorization", 204, true},
		{"/v1/models", "https://ai.example.test", "GET", "", "", 401, true},
		{"/v1/responses", "https://ai.example.test", "POST", "", "", 401, true},
		{"/v1/models", "", "GET", "", "", 401, false},
		{"/v1/models", "https://evil.example", "OPTIONS", "GET", "authorization", 403, false},
		{"/v1/models", "https://ai.example.test.evil", "GET", "", "", 403, false},
		{"/v1/models", "https://ai.example.test", "OPTIONS", "POST", "authorization", 403, true},
		{"/v1/models", "https://ai.example.test", "OPTIONS", "GET", "cookie", 403, true},
		{"/browser-handoffs/exchange", "https://ai.example.test", "OPTIONS", "POST", "authorization", 403, true},
		{"/admin/state", "https://ai.example.test", "GET", "", "", 401, false},
		{"/admin/browser-handoffs", "https://ai.example.test", "OPTIONS", "POST", "content-type", 401, false},
		{"/auth/password/login", "https://ai.example.test", "OPTIONS", "POST", "content-type", 401, false},
		{"/v1/responses/compact", "https://ai.example.test", "OPTIONS", "POST", "authorization", 401, false},
		{"/v1/models/", "https://ai.example.test", "OPTIONS", "GET", "authorization", 401, false},
	} {
		t.Run(test.method+test.path+test.origin+test.requested+test.headers, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Access-Control-Request-Method", test.requested)
			request.Header.Set("Access-Control-Request-Headers", test.headers)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || (response.Header().Get("Access-Control-Allow-Origin") != "") != test.cors {
				t.Fatalf("status=%d headers=%v", response.Code, response.Header())
			}
			if response.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Fatal("CORS must not allow cookies")
			}
			if strings.Contains(test.path, "browser-handoffs") && response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("handoff error can be cached")
			}
		})
	}
	// Authentication and panic errors both remain readable to the trusted client.
	s.mux.HandleFunc("GET /v1/models", func(http.ResponseWriter, *http.Request) { panic("test") })
	request := httptest.NewRequest("GET", "/v1/models", nil)
	request.Header.Set("Origin", "https://ai.example.test")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != 500 || response.Header().Get("Access-Control-Allow-Origin") != "https://ai.example.test" {
		t.Fatal("panic recovery lost CORS")
	}
}

func TestBrowserHandoffRoutesRequireSessionAndManagementOrigin(t *testing.T) {
	harness := newResponsesWebSocketTestHarness(t)
	for _, origin := range []string{"https://gateway.example.test", "https://ai.example.test"} {
		harness.server.config.RPOrigins = []string{"https://gateway.example.test"}
		// routes captured the original RPOrigins; mount freshly for this check.
		handler := harness.server.browserOrigin(harness.server.requireRecentVerification(http.HandlerFunc(harness.server.createBrowserHandoff)))
		request := httptest.NewRequest("POST", "/admin/browser-handoffs", strings.NewReader(`{}`))
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := http.StatusUnauthorized
		if origin != "https://gateway.example.test" {
			want = http.StatusForbidden
		}
		if response.Code != want {
			t.Fatalf("origin=%s status=%d", origin, response.Code)
		}
	}
}

func TestBrowserHandoffCodeAndKeyRedaction(t *testing.T) {
	s, _, key := newBrowserHandoffTestServer(t)
	code := issueBrowserHandoffForTest(t, s, false)
	for _, raw := range []string{code, key, "embedded_" + code + "_" + key, `{"code":"` + code + `","api_key":"` + key + `"}`, "https://ai.example.test/#handoff_version=1&code=" + code} {
		redacted := security.RedactText(raw)
		if strings.Contains(redacted, code) || strings.Contains(redacted, key) {
			t.Fatal("redaction leaked handoff or API credential")
		}
	}
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	response := httptest.NewRecorder()
	s.accessLog(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest("GET", "/"+code+"/"+key, nil))
	if strings.Contains(logs.String(), code) || strings.Contains(logs.String(), key) {
		t.Fatal("access logs leaked credentials in path")
	}
}

type browserHandoffTestRepository struct {
	session store.Session
	user    store.User
	key     store.APIKey
	device  store.Device
	secret  store.APIKeySecret
	err     error
}

func (r *browserHandoffTestRepository) GetActiveSessionByID(context.Context, string, string, time.Time) (store.Session, error) {
	return r.session, r.err
}
func (r *browserHandoffTestRepository) GetUser(context.Context, string) (store.User, error) {
	return r.user, r.err
}
func (r *browserHandoffTestRepository) GetDevice(context.Context, string, string) (store.Device, error) {
	return r.device, r.err
}
func (r *browserHandoffTestRepository) GetAPIKey(context.Context, string, string) (store.APIKey, error) {
	return r.key, r.err
}
func (r *browserHandoffTestRepository) GetAPIKeySecret(context.Context, string, string) (store.APIKeySecret, error) {
	return r.secret, r.err
}

func newBrowserHandoffTestServer(t *testing.T) (*Server, *browserHandoffTestRepository, string) {
	t.Helper()
	now := time.Now().UTC()
	verified := now.Add(-time.Second)
	generated, err := security.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	pepper := bytes.Repeat([]byte{0x31}, 32)
	encryption := bytes.Repeat([]byte{0x42}, 32)
	digest, err := security.HashAPIKey(pepper, generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	userID := "00000000-0000-4000-8000-000000000001"
	keyID := "00000000-0000-4000-8000-000000000002"
	deviceID := "00000000-0000-4000-8000-000000000003"
	ciphertext, err := security.EncryptAPIKeySecret(encryption, userID, generated.PublicID, generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	repo := &browserHandoffTestRepository{
		user:    store.User{ID: userID, Status: store.StatusActive},
		session: store.Session{ID: "00000000-0000-4000-8000-000000000004", UserID: userID, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour), RecentlyVerifiedAt: &verified},
		key:     store.APIKey{ID: keyID, UserID: userID, DeviceID: deviceID, PublicID: generated.PublicID, KeyPrefix: generated.Prefix, KeyHash: digest[:], Status: store.StatusActive, ExpiresAt: now.Add(time.Hour), SecretAvailable: true},
		device:  store.Device{ID: deviceID, UserID: userID, Status: store.StatusActive},
		secret:  store.APIKeySecret{ID: keyID, UserID: userID, PublicID: generated.PublicID, KeyPrefix: generated.Prefix, KeyHash: digest[:], SecretCiphertext: ciphertext},
	}
	browserURL, _ := url.Parse("https://ai.example.test/workbench")
	publicURL, _ := url.Parse("https://gateway.example.test")
	s := &Server{config: config.Config{PublicURL: publicURL, BrowserClientURL: browserURL, KeyPepper: pepper, APIKeyEncryptionKey: encryption, ReauthMaxAge: 5 * time.Minute}, browserHandoffRepo: repo, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /browser-handoffs/exchange", s.exchangeBrowserHandoff)
	return s, repo, generated.Token
}

func newBrowserHandoffIssueRequest(user store.User, session store.Session, keyID string, remember bool) *http.Request {
	body, _ := json.Marshal(map[string]any{"api_key_id": keyID, "remember_key": remember})
	request := httptest.NewRequest("POST", "/admin/browser-handoffs", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(request.Context(), userContextKey, user)
	ctx = context.WithValue(ctx, sessionContextKey, session)
	return request.WithContext(ctx)
}

func issueBrowserHandoffForTest(t *testing.T, s *Server, remember bool) string {
	t.Helper()
	repo := s.browserHandoffRepo.(*browserHandoffTestRepository)
	response := httptest.NewRecorder()
	s.createBrowserHandoff(response, newBrowserHandoffIssueRequest(repo.user, repo.session, repo.key.ID, remember))
	if response.Code != http.StatusCreated {
		t.Fatalf("issue failed: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		LaunchURL string    `json:"launch_url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	launch, err := url.Parse(result.LaunchURL)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Scheme != s.config.BrowserClientURL.Scheme || launch.Host != s.config.BrowserClientURL.Host || launch.Path != s.config.BrowserClientURL.Path || launch.RawQuery != "" {
		t.Fatal("untrusted launch URL")
	}
	values, err := url.ParseQuery(launch.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values.Get("handoff_version") != "1" || values.Get("code") == "" || strings.Contains(result.LaunchURL, "cgk_v1_") {
		t.Fatal("invalid launch fragment")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("issuance can be cached")
	}
	return values.Get("code")
}
func exchangeBrowserHandoffForTest(s *Server, code, origin string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"code": code})
	request := httptest.NewRequest("POST", "/browser-handoffs/exchange", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	return response
}

func TestBrowserHandoffOriginUsesBrowserSerialization(t *testing.T) {
	for _, test := range []struct{ target, origin string }{
		{"https://AI.example.test:443/workbench", "https://ai.example.test"},
		{"http://LOCALHOST:80/workbench", "http://localhost"},
		{"http://LOCALHOST:4174/workbench", "http://localhost:4174"},
		{"https://[::1]:443/workbench", "https://[::1]"},
	} {
		t.Run(test.target, func(t *testing.T) {
			s, _, _ := newBrowserHandoffTestServer(t)
			s.config.BrowserClientURL, _ = url.Parse(test.target)
			code := issueBrowserHandoffForTest(t, s, false)
			response := exchangeBrowserHandoffForTest(s, code, test.origin)
			if response.Code != http.StatusOK || response.Header().Get("Access-Control-Allow-Origin") != test.origin {
				t.Fatalf("origin normalization failed: %d %v", response.Code, response.Header())
			}
		})
	}
}
