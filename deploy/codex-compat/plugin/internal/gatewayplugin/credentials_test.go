package gatewayplugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testLoginState   = "state-0123456789abcdef"
	testRegistration = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

// fakeCPAManagement stands in for CPA's own management API.
type fakeCPAManagement struct {
	t     *testing.T
	host  *fakeHost
	clock time.Time
	mu    sync.Mutex
	// calls records "METHOD path?query body" for each call.
	calls []string
	// authURL is the authorization URL the login start answers with.
	authURL string
	// authStatus is what get-auth-status answers.
	authStatus string
	// onRefresh rewrites a credential document the way CPA's refresh does.
	onRefresh  func(name string, document map[string]any)
	refreshErr bool
	uploadErr  bool
}

func newFakeCPAManagement(f *fixture) *fakeCPAManagement {
	cpa := &fakeCPAManagement{t: f.t, host: f.host, clock: f.clock,
		authURL:    "https://auth.openai.com/oauth/authorize?client_id=x&state=" + testLoginState,
		authStatus: `{"status":"wait"}`}
	f.plugin.cpaManagement = cpa.call
	return cpa
}

func (c *fakeCPAManagement) call(_ context.Context, method, path string, query url.Values, body []byte) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := method + " " + path
	if len(query) > 0 {
		entry += "?" + query.Encode()
	}
	if body != nil {
		entry += " " + string(body)
	}
	c.calls = append(c.calls, entry)
	switch method + " " + path {
	case "POST /v8/management/oauth/auth-url":
		return 200, []byte(`{"status":"ok","url":` + jsonString(c.authURL) + `,"state":"` + testLoginState + `"}`), nil
	case "DELETE /v8/management/oauth/session":
		return 200, []byte(`{"status":"ok"}`), nil
	case "POST /v0/management/oauth-callback":
		return 200, []byte(`{"status":"ok"}`), nil
	case "GET /v0/management/get-auth-status":
		return 200, []byte(c.authStatus), nil
	case "POST /v0/management/auth-files":
		if c.uploadErr {
			return 400, []byte(`{"error":"invalid auth file"}`), nil
		}
		var document map[string]any
		if json.Unmarshal(body, &document) != nil {
			return 400, nil, nil
		}
		provider, _ := document["type"].(string)
		c.host.addEntry(strings.TrimSuffix(query.Get("name"), ".json"), provider, document, c.clock)
		c.host.renameLast(query.Get("name"))
		return 200, []byte(`{"status":"ok"}`), nil
	case "POST /v0/management/auth-files/refresh":
		var request struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &request)
		if c.refreshErr || !c.host.has(request.Name) {
			return 500, []byte(`{"error":"refresh failed: provider said secret things"}`), nil
		}
		if c.onRefresh != nil {
			c.host.edit(request.Name, c.onRefresh)
		}
		return 200, []byte(`{"ok":true,"auth":{"access_token":"fresh-secret"}}`), nil
	case "DELETE /v0/management/auth-files":
		c.host.remove(query.Get("name"))
		return 200, []byte(`{"status":"ok"}`), nil
	}
	c.t.Errorf("unexpected CPA management call %s", entry)
	return 404, nil, nil
}

func (c *fakeCPAManagement) seen(prefix string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, call := range c.calls {
		if strings.HasPrefix(call, prefix) {
			out = append(out, call)
		}
	}
	return out
}

func jsonString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// fakeIDToken is an unsigned ID token carrying a ChatGPT account claim.
func fakeIDToken(account string) string {
	payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	return "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
}

// renameLast gives the most recently added entry the credential file name CPA
// would use, keeping its auth index.
func (h *fakeHost) renameLast(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	last := &h.entries[len(h.entries)-1]
	last.ID, last.Name = name, name
}

func (h *fakeHost) has(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

func (h *fakeHost) remove(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.entries[:0]
	for _, entry := range h.entries {
		if entry.Name != name {
			kept = append(kept, entry)
		}
	}
	h.entries = kept
}

func (h *fakeHost) edit(name string, change func(string, map[string]any)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.entries {
		if h.entries[i].Name != name {
			continue
		}
		var document map[string]any
		_ = json.Unmarshal([]byte(h.docs[h.entries[i].AuthIndex]), &document)
		change(name, document)
		raw, _ := json.Marshal(document)
		h.docs[h.entries[i].AuthIndex] = string(raw)
		h.entries[i].Size = int64(len(raw))
	}
}

func facadePath(provider, action string) string {
	return managementPrefix + facadeNamespace + "/" + provider + "/" + action
}

func TestLoginStartsInCPAWithTheChosenEgress(t *testing.T) {
	f := newFixture(t, 1)
	cpa := newFakeCPAManagement(f)

	status, fields, body := f.manage(http.MethodPost, facadePath("codex", "oauth"), nil,
		`{"registration_id":"`+testRegistration+`","egress_proxy_url":"`+testRouteA+`"}`)
	if status != http.StatusOK || string(fields["state"]) != `"`+testLoginState+`"` || !strings.Contains(string(fields["url"]), "auth.openai.com") {
		t.Fatalf("login start: %d %s", status, body)
	}
	if calls := cpa.seen("POST /v8/management/oauth/auth-url"); len(calls) != 1 ||
		!strings.HasSuffix(calls[0], `{"provider":"codex","proxy_url":"`+testRouteA+`"}`) {
		t.Fatalf("CPA login start: %v", calls)
	}
	if strings.Contains(body, "secret-a") {
		t.Fatalf("the route credential reached the Gateway: %s", body)
	}

	// Without a chosen node the global egress is written out, never omitted.
	f.plugin.oauth = newOAuthSessions()
	if status, _, body = f.manage(http.MethodPost, facadePath("codex", "oauth"), nil, `{}`); status != http.StatusOK {
		t.Fatalf("login start without a node: %d %s", status, body)
	}
	if calls := cpa.seen("POST /v8/management/oauth/auth-url"); !strings.HasSuffix(calls[len(calls)-1], `"proxy_url":"`+testGlobalProxy+`"}`) {
		t.Fatalf("CPA login start without a node: %v", calls)
	}

	// A route outside the account listener, a field the Gateway never sends,
	// or a malformed registration never reach CPA.
	before := len(cpa.seen("POST /v8/management/oauth/auth-url"))
	for name, request := range map[string]string{
		"foreign proxy":       `{"egress_proxy_url":"http://u:p@attacker.example:3129"}`,
		"proxy without route": `{"egress_proxy_url":"http://egress-allowlist:3129"}`,
		"socks proxy":         `{"egress_proxy_url":"socks5://u:p@egress-allowlist:3129"}`,
		"unknown field":       `{"proxy_url":"http://x"}`,
		"bad registration":    `{"registration_id":"not-a-uuid"}`,
		"not an object":       `[]`,
	} {
		if status, _, _ = f.manage(http.MethodPost, facadePath("codex", "oauth"), nil, request); status < 400 {
			t.Fatalf("%s was accepted: %d", name, status)
		}
	}
	if after := len(cpa.seen("POST /v8/management/oauth/auth-url")); after != before {
		t.Fatalf("a refused login start reached CPA")
	}
	// CPA's URL must point at the provider's login host and carry the state.
	cpa.authURL = "https://evil.example/oauth/authorize?state=" + testLoginState
	f.plugin.oauth = newOAuthSessions()
	if status, _, _ = f.manage(http.MethodPost, facadePath("codex", "oauth"), nil, `{}`); status != http.StatusBadGateway {
		t.Fatalf("a foreign authorization URL was passed on: %d", status)
	}
}

func TestLoginCompletesWithIdentityAndEgressBinding(t *testing.T) {
	f := newFixture(t, 1)
	cpa := newFakeCPAManagement(f)
	if status, _, body := f.manage(http.MethodPost, facadePath("codex", "oauth"), nil,
		`{"registration_id":"`+testRegistration+`","egress_proxy_url":"`+testRouteA+`"}`); status != http.StatusOK {
		t.Fatalf("login start: %d %s", status, body)
	}
	// The callback is CPA's own.
	if status, _, _ := f.manage(http.MethodPost, facadePath("codex", "oauth/callback"), nil, `{"state":"other-state-0123456789","code":"c"}`); status != http.StatusNotFound {
		t.Fatalf("a foreign state was accepted: %d", status)
	}
	if status, _, _ := f.manage(http.MethodPost, facadePath("anthropic", "oauth/callback"), nil, `{"state":"`+testLoginState+`","code":"c"}`); status != http.StatusNotFound {
		t.Fatalf("another provider's callback was accepted: %d", status)
	}
	if status, _, body := f.manage(http.MethodPost, facadePath("codex", "oauth/callback"), nil, `{"state":"`+testLoginState+`","code":"auth-code"}`); status != http.StatusOK {
		t.Fatalf("callback: %d %s", status, body)
	}
	if calls := cpa.seen("POST /v0/management/oauth-callback"); len(calls) != 1 ||
		!strings.HasSuffix(calls[0], `{"code":"auth-code","provider":"codex","state":"`+testLoginState+`"}`) {
		t.Fatalf("CPA callback: %v", calls)
	}

	status := func() (int, map[string]json.RawMessage, string) {
		return f.manage(http.MethodPost, facadePath("codex", "oauth/status"), nil, `{"state":"`+testLoginState+`"}`)
	}
	if code, fields, body := status(); code != http.StatusOK || string(fields["status"]) != `"wait"` {
		t.Fatalf("waiting login: %d %s", code, body)
	}
	// CPA reports the saved credential before listing it: still waiting.
	cpa.authStatus = `{"status":"ok","auth_id":"codex-new.json"}`
	if code, fields, body := status(); code != http.StatusOK || string(fields["status"]) != `"wait"` {
		t.Fatalf("unlisted credential: %d %s", code, body)
	}
	// CPA saved the credential with the session's egress.
	f.host.addCodex("codex-new.json", "acct-new", f.clock)
	f.host.setProxy("idx-codex-new.json", testRouteA, f.clock)
	id := CodexAccountIndex("acct-new")
	// The Gateway's commit binds the account to the chosen node.
	f.alloc.routes = map[string]Egress{id: {ProxyURL: testRouteA2, Revision: "r2"}}
	code, fields, body := status()
	if code != http.StatusOK || string(fields["status"]) != `"ok"` || string(fields["account_id"]) != `"`+id+`"` {
		t.Fatalf("completed login: %d %s", code, body)
	}
	if len(f.alloc.registered) != 1 || f.alloc.registered[0] != testRegistration+"="+id {
		t.Fatalf("egress registration %v", f.alloc.registered)
	}
	if f.host.proxyOf("idx-codex-new.json") != testRouteA2 {
		t.Fatalf("the committed route was not written: %q", f.host.proxyOf("idx-codex-new.json"))
	}
	// Polling again answers the same without asking CPA or the Gateway.
	calls := len(cpa.seen("GET /v0/management/get-auth-status"))
	if code, fields, _ = status(); string(fields["account_id"]) != `"`+id+`"` || len(cpa.seen("GET /v0/management/get-auth-status")) != calls || len(f.alloc.registered) != 1 {
		t.Fatalf("a repeated poll changed something: %d", code)
	}
}

func TestLoginFailuresFailClosed(t *testing.T) {
	start := func(t *testing.T, f *fixture, provider string) *fakeCPAManagement {
		cpa := newFakeCPAManagement(f)
		cpa.authURL = map[string]string{
			"codex":       "https://auth.openai.com/oauth/authorize?state=" + testLoginState,
			"anthropic":   "https://claude.ai/oauth/authorize?state=" + testLoginState,
			"antigravity": "https://accounts.google.com/o/oauth2/v2/auth?state=" + testLoginState,
		}[provider]
		if status, _, body := f.manage(http.MethodPost, facadePath(provider, "oauth"), nil, `{"registration_id":"`+testRegistration+`"}`); status != http.StatusOK {
			t.Fatalf("login start: %d %s", status, body)
		}
		return cpa
	}
	poll := func(f *fixture, provider string) string {
		_, fields, _ := f.manage(http.MethodPost, facadePath(provider, "oauth/status"), nil, `{"state":"`+testLoginState+`"}`)
		return string(fields["status"])
	}

	t.Run("credential without identity is removed", func(t *testing.T) {
		f := newFixture(t, 1)
		cpa := start(t, f, "anthropic")
		f.host.addEntry("claude-x", ProviderClaude, map[string]any{"type": "claude", "email": "x@example.com", "refresh_token": "r"}, f.clock)
		cpa.authStatus = `{"status":"ok","auth_id":"claude-x"}`
		if got := poll(f, "anthropic"); got != `"error"` {
			t.Fatalf("status %s", got)
		}
		if calls := cpa.seen("DELETE /v0/management/auth-files"); len(calls) != 1 || !strings.Contains(calls[0], "name=claude-x") {
			t.Fatalf("unidentified credential was kept: %v", calls)
		}
		if len(f.alloc.registered) != 0 {
			t.Fatal("an unidentified credential was bound to an egress")
		}
	})
	t.Run("failed egress registration disables the account", func(t *testing.T) {
		f := newFixture(t, 1)
		cpa := start(t, f, "codex")
		f.host.addCodex("codex-new.json", "acct-new", f.clock)
		cpa.authStatus = `{"status":"ok","auth_id":"codex-new.json"}`
		f.alloc.registerErr = ErrUnavailable
		if got := poll(f, "codex"); got != `"error"` {
			t.Fatalf("status %s", got)
		}
		if manual, _ := f.plugin.control.Statuses(CodexAccountIndex("acct-new")); manual != "manual_disabled" {
			t.Fatalf("account left in service without its egress: %s", manual)
		}
	})
	t.Run("CPA reports an error", func(t *testing.T) {
		f := newFixture(t, 1)
		cpa := start(t, f, "codex")
		cpa.authStatus = `{"status":"error","error":"Failed to exchange token: secret detail"}`
		if got := poll(f, "codex"); got != `"error"` {
			t.Fatalf("status %s", got)
		}
		// The session is gone: a later poll is refused.
		if status, _, _ := f.manage(http.MethodPost, facadePath("codex", "oauth/status"), nil, `{"state":"`+testLoginState+`"}`); status != http.StatusNotFound {
			t.Fatalf("a failed login stayed pollable: %d", status)
		}
	})
	t.Run("a second credential of the same Claude account is removed", func(t *testing.T) {
		f := newFixture(t, 1)
		cpa := start(t, f, "anthropic")
		identity := map[string]any{"type": "claude", "account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "organization_uuid": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "refresh_token": "r"}
		f.host.addEntry("claude-old", ProviderClaude, identity, f.clock)
		if _, err := f.plugin.inventory.Register(ProviderClaude, mustJSON(identity)); err != nil {
			t.Fatal(err)
		}
		f.host.addEntry("claude-new", ProviderClaude, identity, f.clock)
		cpa.authStatus = `{"status":"ok","auth_id":"claude-new"}`
		if got := poll(f, "anthropic"); got != `"error"` {
			t.Fatalf("status %s", got)
		}
		if calls := cpa.seen("DELETE /v0/management/auth-files"); len(calls) != 1 || !strings.Contains(calls[0], "name=claude-new") {
			t.Fatalf("the conflicting credential was not removed: %v", calls)
		}
	})
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func TestCPAStoredIdentities(t *testing.T) {
	f := newFixture(t, 1)
	// Claude: CPA's account and organization UUIDs; a subject the patched
	// image recorded must agree with them.
	claude := map[string]any{"type": "claude", "account_uuid": "AAAAAAAA-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "organization_uuid": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
	id, err := f.plugin.inventory.Register(ProviderClaude, mustJSON(claude))
	if err != nil || !validAccountID(id) {
		t.Fatalf("claude: %q %v", id, err)
	}
	claude["gateway_anthropic_subject"] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if again, errAgain := f.plugin.inventory.Register(ProviderClaude, mustJSON(claude)); errAgain != nil || again != id {
		t.Fatalf("a recorded subject changed the account: %q %v", again, errAgain)
	}
	claude["gateway_anthropic_subject"] = "cccccccc-cccc-4ccc-8ccc-cccccccccccc:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err = f.plugin.inventory.Register(ProviderClaude, mustJSON(claude)); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("a conflicting recorded subject was accepted: %v", err)
	}

	// Antigravity: the patched image's Google subject when present, otherwise
	// the e-mail address CPA recorded, kept in its own registry file.
	byEmail, err := f.plugin.inventory.Register(ProviderAntigravity, mustJSON(map[string]any{"type": "antigravity", "email": " Someone@Example.COM "}))
	if err != nil || !validAccountID(byEmail) {
		t.Fatalf("antigravity by e-mail: %q %v", byEmail, err)
	}
	if again, _ := f.plugin.inventory.Register(ProviderAntigravity, mustJSON(map[string]any{"email": "someone@example.com"})); again != byEmail {
		t.Fatalf("the same address got another account: %q", again)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, AntigravityEmailIdentityFile))
	if err != nil || !strings.Contains(string(raw), `"someone@example.com"`) {
		t.Fatalf("e-mail registry %s, %v", raw, err)
	}
	if _, err = os.Stat(filepath.Join(f.dir, AntigravityIdentityFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an e-mail key reached the subject registry the patched image reads: %v", err)
	}
	bySubject, err := f.plugin.inventory.Register(ProviderAntigravity, mustJSON(map[string]any{"google_subject": "109876543210", "email": "someone@example.com"}))
	if err != nil || bySubject == byEmail {
		t.Fatalf("antigravity by subject: %q %v", bySubject, err)
	}
	for name, document := range map[string]map[string]any{
		"malformed subject":  {"google_subject": "bad subject", "email": "someone@example.com"},
		"no address":         {"email": ""},
		"two addresses":      {"email": "a@example.com,b@example.com"},
		"address with space": {"email": "a b@example.com"},
		"no domain dot":      {"email": "a@localhost"},
	} {
		if _, err = f.plugin.inventory.Register(ProviderAntigravity, mustJSON(document)); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("%s was registered: %v", name, err)
		}
	}
	// A credential is identified only once registered.
	f.host.addEntry("g", ProviderAntigravity, map[string]any{"type": "antigravity", "email": "other@example.com"}, f.clock)
	if _, found, _ := f.plugin.inventory.Find(ProviderAntigravity, mustRegister(t, f, "other@example.com")); !found {
		t.Fatal("a registered e-mail identity was not resolved")
	}
}

func mustRegister(t *testing.T, f *fixture, email string) string {
	t.Helper()
	id, err := f.plugin.inventory.Register(ProviderAntigravity, mustJSON(map[string]any{"email": email}))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestImportStoresTokensWithTheirEgressAndLetsCPARefresh(t *testing.T) {
	f := newFixture(t, 1)
	cpa := newFakeCPAManagement(f)
	cpa.onRefresh = func(_ string, document map[string]any) {
		document["id_token"] = fakeIDToken("acct-imported")
		document["access_token"] = "fresh"
	}
	id := CodexAccountIndex("acct-imported")
	f.alloc.routes = map[string]Egress{id: {ProxyURL: testRouteA, Revision: "r1"}}
	status, fields, body := f.manage(http.MethodPost, facadePath("codex", "credentials"), nil,
		`{"refresh_token":"rt-secret","registration_id":"`+testRegistration+`","egress_proxy_url":"`+testRouteA+`"}`)
	if status != http.StatusOK || string(fields["status"]) != `"ok"` || string(fields["account_id"]) != `"`+id+`"` {
		t.Fatalf("import: %d %s", status, body)
	}
	if strings.Contains(body, "secret") || strings.Contains(body, "fresh") {
		t.Fatalf("token material reached the Gateway: %s", body)
	}
	uploads := cpa.seen("POST /v0/management/auth-files?")
	if len(uploads) != 1 || !strings.Contains(uploads[0], "name=codex-import-") ||
		!strings.HasSuffix(uploads[0], `{"proxy_url":"`+testRouteA+`","refresh_token":"rt-secret","type":"codex"}`) {
		t.Fatalf("upload %v", uploads)
	}
	if refreshes := cpa.seen("POST /v0/management/auth-files/refresh"); len(refreshes) != 1 {
		t.Fatalf("refresh %v", refreshes)
	}
	if len(f.alloc.registered) != 1 || f.alloc.registered[0] != testRegistration+"="+id {
		t.Fatalf("registration %v", f.alloc.registered)
	}

	for name, test := range map[string]struct {
		provider, body string
		setup          func(*fakeCPAManagement)
		status         int
		removed        bool
	}{
		"antigravity":           {provider: "antigravity", body: `{"refresh_token":"r"}`, status: 400},
		"no refresh token":      {provider: "codex", body: `{"access_token":"a"}`, status: 400},
		"token with whitespace": {provider: "codex", body: `{"refresh_token":"a b"}`, status: 400},
		"unknown field":         {provider: "codex", body: `{"refresh_token":"r","proxy_url":"http://x"}`, status: 400},
		"foreign egress":        {provider: "codex", body: `{"refresh_token":"r","egress_proxy_url":"http://u:p@attacker.example:3129"}`, status: 503},
		"upload refused":        {provider: "codex", body: `{"refresh_token":"r"}`, setup: func(c *fakeCPAManagement) { c.uploadErr = true }, status: 502},
		"refresh fails":         {provider: "codex", body: `{"refresh_token":"r"}`, setup: func(c *fakeCPAManagement) { c.refreshErr = true }, status: 502, removed: true},
		"no identity after it":  {provider: "anthropic", body: `{"refresh_token":"r"}`, status: 502, removed: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 1)
			cpa := newFakeCPAManagement(f)
			if test.setup != nil {
				test.setup(cpa)
			}
			status, _, body := f.manage(http.MethodPost, facadePath(test.provider, "credentials"), nil, test.body)
			if status != test.status || strings.Contains(body, "secret") {
				t.Fatalf("got %d %s, want %d", status, body, test.status)
			}
			if removed := len(cpa.seen("DELETE /v0/management/auth-files")) == 1; removed != test.removed {
				t.Fatalf("credential removed = %v, want %v", removed, test.removed)
			}
		})
	}
}

func TestRefreshAndDeleteGoThroughCPA(t *testing.T) {
	f := newFixture(t, 2)
	cpa := newFakeCPAManagement(f)
	id := f.ids["a"]
	query := url.Values{"id": {id}}
	f.alloc.routes = map[string]Egress{id: {ProxyURL: testRouteA, Revision: "r1"}}

	// Refresh writes the current route first, so CPA refreshes through it.
	status, fields, body := f.manage(http.MethodPost, facadePath("codex", "accounts/refresh"), query, `{}`)
	if status != http.StatusOK || string(fields["status"]) != `"ok"` || strings.Contains(body, "fresh-secret") {
		t.Fatalf("refresh: %d %s", status, body)
	}
	if f.host.proxyOf("idx-a") != testRouteA || len(cpa.seen("POST /v0/management/auth-files/refresh")) != 1 {
		t.Fatalf("refresh did not write the route first: %q", f.host.proxyOf("idx-a"))
	}
	cpa.refreshErr = true
	if status, _, body = f.manage(http.MethodPost, facadePath("codex", "accounts/refresh"), query, `{}`); status != http.StatusBadGateway || strings.Contains(body, "secret") {
		t.Fatalf("failed refresh: %d %s", status, body)
	}
	f.alloc.egressErr = map[string]error{id: ErrUnavailable}
	refreshes := len(cpa.seen("POST /v0/management/auth-files/refresh"))
	if status, _, _ = f.manage(http.MethodPost, facadePath("codex", "accounts/refresh"), query, `{}`); status != http.StatusServiceUnavailable ||
		len(cpa.seen("POST /v0/management/auth-files/refresh")) != refreshes {
		t.Fatalf("a refresh without a confirmed route reached CPA: %d", status)
	}
	f.alloc.egressErr = nil
	if status, _, _ = f.manage(http.MethodPost, facadePath("codex", "accounts/refresh"), url.Values{"id": {"0123456789abcdef"}}, `{}`); status != http.StatusNotFound {
		t.Fatalf("unknown account: %d", status)
	}

	// Delete only an account the Owner disabled and that has no request.
	if status, fields, _ = f.manage(http.MethodDelete, facadePath("codex", "accounts"), query, `{}`); status != http.StatusConflict || string(fields["error"]) != `"upstream_account_not_disabled"` {
		t.Fatalf("enabled account: %d %v", status, fields)
	}
	if err := f.plugin.control.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	release, ok := f.plugin.concurrency.Acquire(id, "root")
	if !ok {
		t.Fatal("no slot")
	}
	if status, fields, _ = f.manage(http.MethodDelete, facadePath("codex", "accounts"), query, `{}`); status != http.StatusConflict || string(fields["error"]) != `"upstream_account_draining"` {
		t.Fatalf("busy account: %d %v", status, fields)
	}
	release(SlotDrop)
	if status, fields, body = f.manage(http.MethodDelete, facadePath("codex", "accounts"), query, `{}`); status != http.StatusOK || string(fields["status"]) != `"deleted"` {
		t.Fatalf("delete: %d %s", status, body)
	}
	if calls := cpa.seen("DELETE /v0/management/auth-files"); len(calls) != 1 || !strings.Contains(calls[0], "name=a") || f.host.has("a") {
		t.Fatalf("delete call %v", calls)
	}
	// The control state stays: a later login of the same account is disabled.
	if manual, _ := f.plugin.control.Statuses(id); manual != "manual_disabled" {
		t.Fatalf("control state was lost: %s", manual)
	}
}

func TestCredentialRoutesRespectTheAntigravityRollback(t *testing.T) {
	f := newFixture(t, 1)
	cpa := newFakeCPAManagement(f)
	f.plugin.cfg.AntigravityEnabled = false
	for _, action := range []string{"oauth", "oauth/status", "credentials", "accounts/refresh"} {
		if status, _, _ := f.manage(http.MethodPost, facadePath("antigravity", action), nil, `{}`); status != http.StatusServiceUnavailable {
			t.Fatalf("%s answered %d during a bridge rollback", action, status)
		}
	}
	if len(cpa.seen("")) != 0 {
		t.Fatal("a rollback-time request reached CPA")
	}
}
