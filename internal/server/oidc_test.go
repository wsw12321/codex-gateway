package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/identity"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestOIDCFlowStoreBrowserExpiryCapacityAndReplay(t *testing.T) {
	now := time.Now()
	browser := oidcRandom()
	flow := oidcFlow{Kind: "link", Browser: sha256.Sum256([]byte(browser)), Expires: now.Add(oidcFlowTTL)}
	flows := newOIDCFlowStore(1)
	key, ok := flows.put(flow, now)
	if !ok {
		t.Fatal("put")
	}
	if _, ok := flows.put(flow, now); ok {
		t.Fatal("capacity not enforced")
	}
	if _, ok := flows.take(key, oidcRandom(), now); ok {
		t.Fatal("other browser consumed flow")
	}
	if _, ok := flows.take(key, "", now); ok {
		t.Fatal("missing cookie consumed flow")
	}
	if result, ok := flows.take(key, browser, now); !ok || result.Kind != "link" {
		t.Fatal("original browser rejected")
	}
	if _, ok := flows.take(key, browser, now); ok {
		t.Fatal("replay accepted")
	}
	if _, ok := flows.put(flow, now); ok {
		t.Fatal("consumed flow cancellation tombstone bypassed capacity")
	}
	if _, ok := flows.take(key, browser, flow.Expires); ok {
		t.Fatal("exact expiry accepted")
	}
	flow.Expires = flow.Expires.Add(time.Minute)
	key, ok = flows.put(flow, now.Add(oidcFlowTTL))
	if !ok {
		t.Fatal("expired tombstone did not release capacity")
	}
	flows.discardBrowser(browser)
	if _, ok := flows.take(key, browser, now.Add(oidcFlowTTL)); ok {
		t.Fatal("cancelled browser survived")
	}
}

func TestOIDCConsumedFlowRetainsCancellationAcrossPreview(t *testing.T) {
	now := time.Now()
	browser := oidcRandom()
	flows := newOIDCFlowStore(1)
	key, _ := flows.put(oidcFlow{Kind: "link", Browser: sha256.Sum256([]byte(browser)), Nonce: "nonce", Verifier: "verifier", Expires: now.Add(time.Minute)}, now)
	flow, ok := flows.take(key, browser, now)
	if !ok {
		t.Fatal("take")
	}
	retained := flows.entries[flow.key]
	if retained.Nonce != "" || retained.Verifier != "" {
		t.Fatal("consumed tombstone retained exchange secrets")
	}
	flow.Kind = "confirm"
	flow.lifecycle.mu.Lock()
	confirmation, ok := flows.confirmation(flow, now)
	flow.lifecycle.mu.Unlock()
	if !ok || confirmation == key || len(flows.entries) != 1 {
		t.Fatal("preview did not rotate its key in place")
	}
	flows.discardBrowser(browser)
	if flow.active(now) {
		t.Fatal("in-flight copy survived browser cancellation")
	}
	if _, ok := flows.take(confirmation, browser, now); ok {
		t.Fatal("preview lost its original cancellation state")
	}
}

func TestOIDCCancellationSerializesWithIssuedSession(t *testing.T) {
	now := time.Now()
	browser := oidcRandom()
	flows := newOIDCFlowStore(1)
	key, _ := flows.put(oidcFlow{Kind: "login", Browser: sha256.Sum256([]byte(browser)), Expires: now.Add(time.Minute)}, now)
	flow, _ := flows.take(key, browser, now)
	flow.lifecycle.mu.Lock()
	cancelled := make(chan []oidcFlowEffects, 1)
	go func() { cancelled <- flows.discardBrowser(browser) }()
	flow.lifecycle.issuedSessionID = "issued-session-id"
	flow.lifecycle.mu.Unlock()
	sessions := <-cancelled
	if len(sessions) != 1 || sessions[0].issuedSessionID != "issued-session-id" || flow.active(now) {
		t.Fatalf("late issued session escaped cancellation: %v", sessions)
	}
}
func TestOIDCFlowStoreConcurrentTakeOnce(t *testing.T) {
	now := time.Now()
	browser := oidcRandom()
	flows := newOIDCFlowStore(3)
	key, _ := flows.put(oidcFlow{Kind: "login", Browser: sha256.Sum256([]byte(browser)), Expires: now.Add(time.Minute)}, now)
	var wg sync.WaitGroup
	var count atomic.Int32
	for range 50 {
		wg.Go(func() {
			if _, ok := flows.take(key, browser, now); ok {
				count.Add(1)
			}
		})
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("consumed %d times", count.Load())
	}
}

func TestOIDCCancelOriginalStateAfterChoiceAndVerification(t *testing.T) {
	now := time.Now()
	browser := oidcRandom()
	flows := newOIDCFlowStore(2)
	key, _ := flows.put(oidcFlow{Kind: "login", Browser: sha256.Sum256([]byte(browser)), Expires: now.Add(time.Minute)}, now)
	flow, _ := flows.take(key, browser, now)
	flow.Kind = "register"
	flow.lifecycle.mu.Lock()
	choice, ok := flows.confirmation(flow, now)
	flow.lifecycle.mu.Unlock()
	if !ok {
		t.Fatal("rotate")
	}
	if _, ok := flows.cancel(key, oidcRandom(), now); ok {
		t.Fatal("wrong browser cancelled flow")
	}
	if _, ok := flows.cancel(key, browser, now); !ok {
		t.Fatal("original state lost after rotation")
	}
	if _, ok := flows.take(choice, browser, now); ok {
		t.Fatal("cancelled choice remained usable")
	}
	newKey, _ := flows.put(oidcFlow{Kind: "reauth", Browser: sha256.Sum256([]byte(browser)), Expires: now.Add(time.Minute)}, now)
	newFlow, _ := flows.take(newKey, browser, now)
	newFlow.lifecycle.verifiedUserID, newFlow.lifecycle.verifiedSessionID, newFlow.lifecycle.verifiedAt = "user", "original-session", now
	if _, ok := flows.cancel(key, browser, now); !ok || !newFlow.active(now) {
		t.Fatal("old cancellation affected newer flow")
	}
	effects, ok := flows.cancel(newKey, browser, now)
	if !ok || effects.issuedSessionID != "" || effects.verifiedSessionID != "original-session" || effects.verifiedAt != now {
		t.Fatalf("reauth cancellation tried to revoke original session: %+v", effects)
	}
}

func TestOIDCReauthenticationCancellationOutlivesCeremonyDeadline(t *testing.T) {
	now := time.Now()
	browser := oidcRandom()
	flows := newOIDCFlowStore(1)
	key, _ := flows.put(oidcFlow{Kind: "reauth", UserID: "user", SessionID: "session", Browser: sha256.Sum256([]byte(browser)), Expires: now.Add(10 * time.Minute)}, now)
	flow, _ := flows.take(key, browser, now.Add(9*time.Minute))
	verifiedAt := now.Add(9 * time.Minute)
	flow.lifecycle.verifiedUserID, flow.lifecycle.verifiedSessionID, flow.lifecycle.verifiedAt = "user", "session", verifiedAt
	flows.retainVerificationCancellation(flow, verifiedAt.Add(oidcVerificationAge))
	later := now.Add(11 * time.Minute)
	if _, ok := flows.take(key, browser, later); ok {
		t.Fatal("retention extended consumption deadline")
	}
	if _, ok := flows.put(oidcFlow{Expires: later.Add(time.Minute)}, later); ok {
		t.Fatal("capacity pruned live cancellation metadata")
	}
	if effects, ok := flows.cancel(key, browser, later); !ok || effects.verifiedAt != verifiedAt {
		t.Fatal("late cancellation lost its verification marker")
	}
	if effects := flows.cancelSessionReauthentication(key, store.Session{ID: "session", UserID: "user"}, later); len(effects) != 1 {
		t.Fatal("original session cannot cancel late response")
	}
	if _, ok := flows.put(oidcFlow{Expires: now.Add(16 * time.Minute)}, now.Add(15*time.Minute)); !ok {
		t.Fatal("expired proof retained capacity")
	}
}
func TestOIDCStateWhitelist(t *testing.T) {
	now := time.Now()
	raw, _ := json.Marshal(externalIdentityView(true, store.ExternalIdentity{ID: "private-id", UserID: "private-user", Issuer: "private-issuer", Subject: "private-subject", MaskedEmail: "a***@example.test", LinkedAt: now}))
	for _, secret := range []string{"private-", "subject", "issuer", "user_id"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("leak: %s", raw)
		}
	}
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	assertJSONKeys(t, value, "enabled", "linked", "masked_email", "linked_at")
}
func TestOIDCCallbackGETIsInertAndNoStore(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "https://gateway.example/auth/oidc/callback?code=secret-code&state=secret-state", nil)
	w := httptest.NewRecorder()
	s.oidcCallback(w, r)
	if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "secret-") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("GET callback handled identity data")
	}
	if !strings.Contains(w.Body.String(), `src="/static/oidc-callback.js"`) {
		t.Fatal("missing same-origin landing script")
	}
}
func TestOIDCDisabledAndOriginGuards(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), attempts: newAttemptLimiter()}
	s.config.RPOrigins = []string{"https://gateway.example"}
	s.oidcRoutes()
	for _, path := range []string{"/auth/oidc/login", "/auth/oidc/complete", "/auth/oidc/register", "/auth/oidc/register/cancel", "/auth/oidc/cancel", "/auth/oidc/reauth/begin", "/admin/identity-link/begin", "/admin/identity-link/confirm", "/admin/identity-link/cancel"} {
		r := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("origin accepted for %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/auth/oidc/login", "/auth/oidc/complete", "/auth/oidc/register", "/auth/oidc/register/cancel", "/auth/oidc/cancel"} {
		r := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		r.Header.Set("Origin", s.config.RPOrigins[0])
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "oidc_disabled") {
			t.Fatal(w.Body.String())
		}
	}
}

type testOIDCProvider struct {
	external       identity.OIDCIdentity
	exchanges      int
	exchangeError  error
	beforeExchange func()
}

func (p *testOIDCProvider) AuthorizationURL(_ context.Context, state, nonce, verifier string) (string, error) {
	return "https://auth.example/authorize?state=" + state, nil
}
func (p *testOIDCProvider) Exchange(_ context.Context, code, nonce, verifier string) (identity.OIDCIdentity, error) {
	p.exchanges++
	if p.beforeExchange != nil {
		p.beforeExchange()
	}
	return p.external, p.exchangeError
}
