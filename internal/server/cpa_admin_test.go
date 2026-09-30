package server

import (
	"context"
	"encoding/json"
	"io"
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
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestCPAManagementRoutesRequireOwnerSessionAndOrigin(t *testing.T) {
	s := &Server{config: config.Config{RPOrigins: []string{"https://gateway.example"}}, mux: http.NewServeMux()}
	s.routes()
	for _, path := range []string{"/admin/cpa/", "/admin/cpa/assets/panel.js", "/admin/cpa/api/codex/accounts", "/admin/cpa/api/oauth/abc/status", "/admin/cpa/api/config.yaml", "/admin/cpa/api/api-call", "/admin/cpa/api/auth-files/download"} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s status=%d", path, w.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		path := "/admin/cpa/api/codex/credentials"
		if method == http.MethodPut {
			path = "/admin/cpa/api/codex/accounts/0123456789abcdef/status"
		}
		if method == http.MethodDelete {
			path = "/admin/cpa/api/codex/accounts/0123456789abcdef"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://attacker.example")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s cross origin status=%d", method, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), userContextKey, store.User{Role: store.UserRoleMember}))
	w := httptest.NewRecorder()
	s.ownerOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member status=%d", w.Code)
	}
}

func TestCPAManagementOAuthURLAllowlist(t *testing.T) {
	state := strings.Repeat("a", 32)
	for _, test := range []struct {
		provider, url string
		valid         bool
	}{
		{"codex", "https://auth.openai.com/authorize?state=" + state, true},
		{"antigravity", "https://accounts.google.com/o/oauth2/auth?state=" + state, true},
		{"codex", "https://accounts.google.com/authorize?state=" + state, false},
		{"antigravity", "https://accounts.google.com.attacker.example/?state=" + state, false},
		{"codex", "https://secret@auth.openai.com/?state=" + state, false},
		{"codex", "http://auth.openai.com/?state=" + state, false},
		{"codex", "https://auth.openai.com:8443/?state=" + state, false},
		{"codex", "https://auth.openai.com/?state=wrong", false},
		{"codex", "javascript:alert(1)", false},
	} {
		if got := validCPAOAuthURL(test.provider, test.url, state); got != test.valid {
			t.Errorf("%s valid=%v", test.url, got)
		}
	}
}

func TestCPAManagementOAuthBoundExpiringAndSingleUse(t *testing.T) {
	now := time.Now()
	s := &cpaAdminState{attempts: map[string]cpaOAuthAttempt{"flow": {SessionID: "owner-session", State: "state", ExpiresAt: now.Add(time.Minute)}}}
	if _, ok := s.oauthAttempt("flow", "other-owner-session", true, now); ok {
		t.Fatal("cross-session state consumed")
	}
	if _, ok := s.oauthAttempt("flow", "owner-session", false, now.Add(time.Hour)); ok {
		t.Fatal("expired state accepted")
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.oauthAttempt("flow", "owner-session", true, now); ok {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("callback accepted %d times", accepted.Load())
	}
	if _, ok := s.oauthAttempt("flow", "owner-session", false, now); !ok {
		t.Fatal("submitted flow cannot poll")
	}
}

func TestCPAManagementImportRejectsPolicyAndMissingRefresh(t *testing.T) {
	s := &Server{}
	for _, body := range []string{`{"refresh_token":"secret","proxy_url":"http://attacker"}`, `{"refresh_token":"secret","priority":99}`, `{"refresh_token":"secret","google_subject":"spoofed"}`, `{"refresh_token":"secret","disabled":false}`, `{"access_token":"still-valid"}`, `{"refresh_token":"secret"} {}`, `{"refresh_token":"secret","refresh_token":"other"}`, `{"refresh_token":"secret","Refresh_Token":"other"}`, `null`} {
		r := httptest.NewRequest(http.MethodPost, "/admin/cpa/api/antigravity/credentials", strings.NewReader(body))
		r.SetPathValue("provider", "antigravity")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.cpaImport(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status=%d body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatal("credential echoed")
		}
	}
}

func TestCPAManagementDeleteMissingConcurrencyFailsClosed(t *testing.T) {
	for _, missing := range []bool{true, false} {
		var disabled, deleted bool
		base, _ := url.Parse("http://sidecar.invalid")
		client := &http.Client{Transport: quotaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body []byte
			switch r.URL.Path {
			case "/internal/upstream-accounts/0123456789abcdef/status":
				data, _ := io.ReadAll(r.Body)
				if string(data) != `{"enabled":false}` {
					t.Errorf("delete did not disable: %s", data)
				}
				disabled = true
				body = []byte(`{"id":"0123456789abcdef","status":"unavailable","cliproxy_status":"active","gateway_manual_status":"manual_disabled","gateway_quota_status":"available"}`)
			case "/internal/upstream-accounts/concurrency":
				if !disabled {
					t.Error("sample before disable")
				}
				accounts := []gatewayproxy.UpstreamAccountConcurrency{}
				if !missing {
					accounts = append(accounts, gatewayproxy.UpstreamAccountConcurrency{ID: "0123456789abcdef", ActiveRequests: 0})
				}
				body, _ = json.Marshal(gatewayproxy.UpstreamConcurrency{SampledAt: time.Now().UTC(), Accounts: accounts})
			case "/internal/gateway-management/codex/accounts/0123456789abcdef":
				deleted = true
				body = []byte(`{"status":"deleted"}`)
			default:
				t.Errorf("unexpected request: %s", r.URL.Path)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})}
		s := &Server{config: config.Config{SidecarURL: base, CPAManagementToken: "server-only"}, upstream: gatewayproxy.NewWithHTTPClient(base, "data-plane", client), cpaAdmin: &cpaAdminState{client: client}}
		r := httptest.NewRequest(http.MethodDelete, "/admin/cpa/api/codex/accounts/0123456789abcdef", strings.NewReader(`{}`))
		r.SetPathValue("provider", "codex")
		r.SetPathValue("id", "0123456789abcdef")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.cpaDelete(w, r)
		if !disabled || deleted == missing {
			t.Errorf("missing=%v disabled=%v deleted=%v", missing, disabled, deleted)
		}
		if missing && w.Code != 502 || !missing && w.Code != 200 {
			t.Errorf("missing=%v status=%d", missing, w.Code)
		}
	}
}

func TestCPAManagementClientPinsRouteKeyAndDoesNotFollowRedirect(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/gateway-management/codex/oauth" || r.URL.RawQuery != "" {
			t.Errorf("unsafe URL %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer management-only" || r.Header.Get("Cookie") != "" {
			t.Error("wrong authentication")
		}
		http.Redirect(w, r, target.URL, 307)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	s := &Server{config: config.Config{SidecarURL: base, CPAManagementToken: "management-only"}, mux: http.NewServeMux()}
	s.cpaAdminRoutes()
	if err := s.cpaCall(context.Background(), http.MethodPost, "codex/oauth", struct{}{}, nil); err == nil {
		t.Fatal("redirect accepted")
	}
	if forwarded.Load() != 0 {
		t.Fatal("management credential followed redirect")
	}
}

func TestCPAManagementLegacyRollbackCannotRefreshGoogleCredentials(t *testing.T) {
	base, _ := url.Parse("http://sidecar.invalid")
	s := &Server{config: config.Config{SidecarURL: base, CPAManagementToken: "server-only", AntigravityTransport: "legacy-bridge"}, cpaAdmin: &cpaAdminState{client: &http.Client{Transport: quotaRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("inactive CPA received credential operation")
		return nil, nil
	})}}}
	for _, path := range []string{"antigravity/oauth", "antigravity/credentials", "antigravity/accounts/0123456789abcdef/refresh", "antigravity/accounts/0123456789abcdef"} {
		if err := s.cpaCall(context.Background(), http.MethodPost, path, struct{}{}, nil); err == nil {
			t.Errorf("legacy mode accepted %s", path)
		}
	}
}

func TestCPAManagementAssetsPreserveCSPAndRestrictedSurface(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	httpx.SecurityHeaders(http.HandlerFunc(s.cpaPage)).ServeHTTP(w, httptest.NewRequest("GET", "/admin/cpa/", nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("page or CSP unavailable")
	}
	if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<style>") {
		t.Fatal("inline executable assets violate CSP")
	}
	for _, name := range []string{"panel.js", "panel.css"} {
		if data, err := cpaAssets.ReadFile("assets/cpa/" + name); err != nil || len(data) == 0 {
			t.Fatalf("missing built asset %s", name)
		}
	}
	data, _ := cpaAssets.ReadFile("assets/cpa/panel.js")
	for _, forbidden := range []string{"localStorage", "/v0/management", "/v8/management", "config.yaml", "auth-files/download", "api-call", "managementKey"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("forbidden panel capability %s", forbidden)
		}
	}
}
