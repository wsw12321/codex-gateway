package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	browserHandoffTTL      = 120 * time.Second
	browserHandoffCapacity = 4096
	browserHandoffPrefix   = "cgb_v1_"
)

var (
	errBrowserHandoffCapacity = errors.New("browser handoff capacity reached")
	errBrowserHandoffInvalid  = errors.New("browser handoff is unavailable")
)

// No plaintext code, API key, cookie, or session-token digest is retained.
type browserHandoff struct {
	UserID      string
	SessionID   string
	APIKeyID    string
	RememberKey bool
	ExpiresAt   time.Time
}

type browserHandoffStore struct {
	mu       sync.Mutex
	entries  map[[sha256.Size]byte]browserHandoff
	capacity int
}

func newBrowserHandoffStore(capacity int) *browserHandoffStore {
	return &browserHandoffStore{entries: make(map[[sha256.Size]byte]browserHandoff), capacity: capacity}
}

func (s *browserHandoffStore) issue(entry browserHandoff, now time.Time) (string, time.Time, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", time.Time{}, err
	}
	code := browserHandoffPrefix + base64.RawURLEncoding.EncodeToString(entropy[:])
	digest := sha256.Sum256([]byte(code))
	s.mu.Lock()
	defer s.mu.Unlock()
	for digest, existing := range s.entries {
		if !existing.ExpiresAt.After(now) {
			delete(s.entries, digest)
		}
	}
	if len(s.entries) >= s.capacity {
		return "", time.Time{}, errBrowserHandoffCapacity
	}
	entry.ExpiresAt = now.Add(browserHandoffTTL)
	s.entries[digest] = entry
	return code, entry.ExpiresAt, nil
}

// Delete before doing database work: parallel attempts can never both redeem,
// and a failure after consumption requires a fresh handoff from the gateway.
func (s *browserHandoffStore) consume(code string, now time.Time) (browserHandoff, bool) {
	if !strings.HasPrefix(code, browserHandoffPrefix) || len(code) != len(browserHandoffPrefix)+43 {
		return browserHandoff{}, false
	}
	encoded := strings.TrimPrefix(code, browserHandoffPrefix)
	entropy, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(entropy) != 32 {
		return browserHandoff{}, false
	}
	digest := sha256.Sum256([]byte(code))
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[digest]
	delete(s.entries, digest)
	return entry, ok && entry.ExpiresAt.After(now)
}

type browserHandoffRepository interface {
	GetActiveSessionByID(context.Context, string, string, time.Time) (store.Session, error)
	GetUser(context.Context, string) (store.User, error)
	GetDevice(context.Context, string, string) (store.Device, error)
	GetAPIKey(context.Context, string, string) (store.APIKey, error)
	GetAPIKeySecret(context.Context, string, string) (store.APIKeySecret, error)
}

func (s *Server) handoffStore() *browserHandoffStore {
	s.browserHandoffOnce.Do(func() {
		if s.browserHandoffs == nil {
			s.browserHandoffs = newBrowserHandoffStore(browserHandoffCapacity)
		}
	})
	return s.browserHandoffs
}

func (s *Server) handoffRepository() browserHandoffRepository {
	if s.browserHandoffRepo != nil {
		return s.browserHandoffRepo
	}
	return s.store
}

// decryptAPIKeySecret is shared with the explicit reveal flow. A decrypted
// value is usable only if its authenticated owner, public ID, prefix and HMAC
// match the persisted credential metadata.
func (s *Server) decryptAPIKeySecret(userID, keyID string, secret store.APIKeySecret) (string, error) {
	if !hmac.Equal([]byte(secret.ID), []byte(keyID)) || !hmac.Equal([]byte(secret.UserID), []byte(userID)) {
		return "", errors.New("stored API key secret owner mismatch")
	}
	plaintext, err := security.DecryptAPIKeySecret(s.config.APIKeyEncryptionKey, userID, secret.PublicID, secret.SecretCiphertext)
	if err != nil {
		return "", err
	}
	var expected security.APIKeyDigest
	if len(secret.KeyHash) != len(expected) {
		return "", errors.New("stored API key digest has invalid length")
	}
	copy(expected[:], secret.KeyHash)
	parsed, verified, err := security.VerifyAPIKey(s.config.KeyPepper, plaintext, expected)
	if err != nil {
		return "", err
	}
	if !verified || !hmac.Equal([]byte(parsed.PublicID), []byte(secret.PublicID)) ||
		!hmac.Equal([]byte(parsed.Prefix()), []byte(secret.KeyPrefix)) {
		return "", errors.New("stored API key secret failed verification")
	}
	return plaintext, nil
}

func (s *Server) browserHandoffKey(ctx context.Context, entry browserHandoff, now time.Time, requireRecent bool) (string, error) {
	repo := s.handoffRepository()
	session, err := repo.GetActiveSessionByID(ctx, entry.UserID, entry.SessionID, now)
	if err != nil || session.ID != entry.SessionID || session.UserID != entry.UserID || session.RevokedAt != nil ||
		!session.IdleExpiresAt.After(now) || !session.AbsoluteExpiresAt.After(now) {
		return "", errBrowserHandoffInvalid
	}
	if requireRecent && (session.RecentlyVerifiedAt == nil || session.RecentlyVerifiedAt.After(now) ||
		now.Sub(*session.RecentlyVerifiedAt) >= s.config.ReauthMaxAge) {
		return "", errBrowserHandoffInvalid
	}
	user, err := repo.GetUser(ctx, entry.UserID)
	if err != nil || user.ID != entry.UserID || user.Status != store.StatusActive {
		return "", errBrowserHandoffInvalid
	}
	key, err := repo.GetAPIKey(ctx, entry.UserID, entry.APIKeyID)
	if err != nil || key.ID != entry.APIKeyID || key.UserID != entry.UserID || key.Status != store.StatusActive ||
		!key.ExpiresAt.After(now) || !key.SecretAvailable {
		return "", errBrowserHandoffInvalid
	}
	device, err := repo.GetDevice(ctx, entry.UserID, key.DeviceID)
	if err != nil || device.ID != key.DeviceID || device.UserID != entry.UserID || device.Status != store.StatusActive {
		return "", errBrowserHandoffInvalid
	}
	secret, err := repo.GetAPIKeySecret(ctx, entry.UserID, entry.APIKeyID)
	if err != nil || len(secret.SecretCiphertext) == 0 || key.PublicID != secret.PublicID ||
		key.KeyPrefix != secret.KeyPrefix || !hmac.Equal(key.KeyHash, secret.KeyHash) {
		return "", errBrowserHandoffInvalid
	}
	return s.decryptAPIKeySecret(entry.UserID, entry.APIKeyID, secret)
}

func (s *Server) createBrowserHandoff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.config.BrowserClientURL == nil {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "invalid_request_error", "browser_client_disabled", "网页工作台接入尚未启用")
		return
	}
	var input struct {
		APIKeyID    string `json:"api_key_id"`
		RememberKey bool   `json:"remember_key"`
	}
	if err := decodeJSON(w, r, &input, 4<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	keyID, err := uuid.Parse(input.APIKeyID)
	if err != nil || keyID.String() != input.APIKeyID {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_api_key", "请选择有效的 API Key")
		return
	}
	entry := browserHandoff{UserID: userFrom(r.Context()).ID, SessionID: sessionFrom(r.Context()).ID,
		APIKeyID: input.APIKeyID, RememberKey: input.RememberKey}
	now := time.Now().UTC()
	if _, err := s.browserHandoffKey(r.Context(), entry, now, true); err != nil {
		httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "browser_handoff_unavailable", "会话、设备或密钥不可用，请刷新后重新接入")
		return
	}
	code, expiresAt, err := s.handoffStore().issue(entry, now)
	if errors.Is(err, errBrowserHandoffCapacity) {
		w.Header().Set("Retry-After", "120")
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "browser_handoff_capacity", "接入请求过多，请稍后重试")
		return
	}
	if err != nil {
		internalError(s, w, r, "issue browser handoff", err)
		return
	}
	launchURL := *s.config.BrowserClientURL
	launchURL.Fragment = "handoff_version=1&code=" + code
	writeJSON(w, http.StatusCreated, map[string]any{"launch_url": launchURL.String(), "expires_at": expiresAt})
}

func (s *Server) exchangeBrowserHandoff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Check before consumption so a request from another Origin cannot burn a
	// valid code. This endpoint uses the code, never browser session cookies.
	if s.config.BrowserClientURL == nil || r.Header.Get("Origin") != s.browserClientOrigin() {
		httpx.WriteError(w, r, http.StatusForbidden, "invalid_request_error", "invalid_origin", "请求来源无效")
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(w, r, &input, 4<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	entry, ok := s.handoffStore().consume(input.Code, time.Now().UTC())
	if !ok {
		httpx.WriteError(w, r, http.StatusGone, "authentication_error", "browser_handoff_expired", "连接码已过期或已使用，请返回网关重新接入")
		return
	}
	plaintext, err := s.browserHandoffKey(r.Context(), entry, time.Now().UTC(), false)
	if err != nil {
		httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "browser_handoff_unavailable", "会话、设备或密钥不可用，请返回网关重新接入")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url": strings.TrimRight(s.config.PublicURL.String(), "/") + "/v1",
		"api_key":  plaintext, "api_key_id": entry.APIKeyID, "remember_key": entry.RememberKey, "protocol": "responses",
	})
}

func (s *Server) browserClientOrigin() string {
	if s.config.BrowserClientURL == nil {
		return ""
	}
	u := s.config.BrowserClientURL
	host := strings.ToLower(u.Host)
	// Browser Origin serializes DNS names in lowercase and omits default ports.
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	return u.Scheme + "://" + host
}

// Only these three exact endpoints accept cross-origin browser requests.
// Wrap recovery and authentication as well so all failures carry CORS headers.
func (s *Server) browserClientCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/admin/browser-handoffs" || path == "/browser-handoffs/exchange" {
			w.Header().Set("Cache-Control", "no-store")
		}
		method := ""
		switch path {
		case "/browser-handoffs/exchange", "/v1/responses":
			method = http.MethodPost
		case "/v1/models":
			method = http.MethodGet
		}
		if method == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if origin == "" && path != "/browser-handoffs/exchange" {
			next.ServeHTTP(w, r)
			return
		}
		if origin == "" || s.browserClientOrigin() == "" || origin != s.browserClientOrigin() {
			httpx.WriteError(w, r, http.StatusForbidden, "invalid_request_error", "invalid_origin", "请求来源无效")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
		if r.Method == http.MethodOptions {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			if r.Header.Get("Access-Control-Request-Method") != method {
				httpx.WriteError(w, r, http.StatusForbidden, "invalid_request_error", "invalid_cors_method", "请求方法无效")
				return
			}
			for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
				header = strings.ToLower(strings.TrimSpace(header))
				if header != "" && header != "content-type" && !(header == "authorization" && path != "/browser-handoffs/exchange") {
					httpx.WriteError(w, r, http.StatusForbidden, "invalid_request_error", "invalid_cors_header", "请求头无效")
					return
				}
			}
			w.Header().Set("Access-Control-Allow-Methods", method)
			allowedHeaders := "Content-Type"
			if path != "/browser-handoffs/exchange" {
				allowedHeaders += ", Authorization"
			}
			w.Header().Set("Access-Control-Allow-Headers", allowedHeaders)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
