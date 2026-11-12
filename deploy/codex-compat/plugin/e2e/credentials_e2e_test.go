//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const credentialPage = "/v0/management/gateway/gateway-management/codex"

// credentialFor finds the credential file of a ChatGPT account and returns
// its name and stored proxy.
func (h *harness) credentialFor(accountID string) (name, proxy string) {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.authDir, "*.json"))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var document struct {
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
			ProxyURL  string `json:"proxy_url"`
		}
		if json.Unmarshal(raw, &document) == nil && (document.AccountID == accountID || accountFromToken(document.IDToken) == accountID) {
			return filepath.Base(file), document.ProxyURL
		}
	}
	return "", ""
}

func accountFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.Auth.AccountID
}

func basicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// tokenCalls lists the calls CPA made to ChatGPT's token endpoint.
func (h *harness) tokenCalls() []upstreamCall {
	var out []upstreamCall
	for _, call := range h.provider.seen() {
		if call.Host == "auth.openai.com" {
			out = append(out, call)
		}
	}
	return out
}

// TestCredentialOperations drives the credential page's import, refresh,
// delete and login against a real CPA process. CPA builds every provider
// request; the plugin only fixes the egress beforehand and binds the
// resulting account. Each token exchange must leave through the egress the
// Owner chose, never the global one.
func TestCredentialOperations(t *testing.T) {
	h := newHarness(t)
	h.addAccount("a", nil)
	h.start()
	const (
		importRegistration = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		loginRegistration  = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	importRoute := h.route("reg-import", "secret-reg-import")
	importCommitted := h.route("acct-imp-route", "secret-imp")
	loginRoute := h.route("reg-login", "secret-reg-login")
	loginCommitted := h.route("acct-login-route", "secret-login")
	h.gateway.mu.Lock()
	h.gateway.registrations = map[string]string{importRegistration: importCommitted, loginRegistration: loginCommitted}
	h.gateway.mu.Unlock()

	// Import: the tokens are stored with the chosen egress and CPA refreshes
	// them through it.
	importID := stableIndex("acct-imp")
	h.resetObservations()
	status, body := h.management(http.MethodPost, credentialPage+"/credentials", managementKey,
		`{"refresh_token":"rt-imp","registration_id":"`+importRegistration+`","egress_proxy_url":"`+importRoute+`"}`)
	if status != http.StatusOK || body != `{"account_id":"`+importID+`","status":"ok"}` {
		t.Fatalf("import: %d %s", status, body)
	}
	// CPA may refresh the new file on its own as well as on request; every
	// refresh leaves through the chosen egress.
	calls := h.tokenCalls()
	if len(calls) == 0 || calls[0].Form.Get("grant_type") != "refresh_token" || calls[0].Form.Get("refresh_token") != "rt-imp" {
		t.Fatalf("the import was not refreshed with its token: %+v", calls)
	}
	for _, call := range calls {
		if call.Via != "account-egress" || call.ProxyAuth != basicAuth("reg-import", "secret-reg-import") {
			t.Fatalf("the import's refresh did not use the chosen egress: %+v", call)
		}
	}
	if name, proxy := h.credentialFor("acct-imp"); !strings.HasPrefix(name, "codex-import-") || proxy != importCommitted {
		t.Fatalf("imported credential %q carries %q, want the committed route", name, proxy)
	}
	h.gateway.mu.Lock()
	committed := append([]string(nil), h.gateway.committed...)
	h.gateway.mu.Unlock()
	if len(committed) != 1 || committed[0] != importRegistration+"="+importID {
		t.Fatalf("egress registration %v", committed)
	}
	// The account serves through its committed route.
	h.gateway.prefer = []string{importID}
	h.resetObservations()
	if got := h.do(call{}); got.status != http.StatusOK {
		t.Fatalf("request on the imported account: %d %s", got.status, got.body)
	}
	if seen := h.provider.seen(); len(seen) != 1 || seen[0].Bearer != "at-imp" || seen[0].ProxyAuth != basicAuth("acct-imp-route", "secret-imp") {
		t.Fatalf("imported account's request: %+v", seen)
	}

	// Refresh: CPA may skip a credential it refreshed moments ago; whatever
	// it sends leaves through the account's route.
	h.resetObservations()
	if status, body = h.management(http.MethodPost, credentialPage+"/accounts/refresh?id="+importID, managementKey, "{}"); status != http.StatusOK || body != `{"status":"ok"}` {
		t.Fatalf("refresh: %d %s", status, body)
	}
	for _, call := range h.tokenCalls() {
		if call.ProxyAuth != basicAuth("acct-imp-route", "secret-imp") {
			t.Fatalf("refresh did not use the account's route: %+v", call)
		}
	}

	// Delete: only a disabled, idle account; the file goes, the listing too.
	if status, body = h.management(http.MethodDelete, credentialPage+"/accounts?id="+importID, managementKey, "{}"); status != http.StatusConflict {
		t.Fatalf("an enabled account was deleted: %d %s", status, body)
	}
	if status, body = h.management(http.MethodPut, "/v0/management/gateway/upstream-accounts/status?id="+importID, managementKey, `{"enabled":false}`); status != http.StatusOK {
		t.Fatalf("disable: %d %s", status, body)
	}
	if status, body = h.management(http.MethodDelete, credentialPage+"/accounts?id="+importID, managementKey, "{}"); status != http.StatusOK || body != `{"status":"deleted"}` {
		t.Fatalf("delete: %d %s", status, body)
	}
	if name, _ := h.credentialFor("acct-imp"); name != "" {
		t.Fatalf("the credential file %s survived its deletion", name)
	}
	h.eventually("the deleted account leaves the listing", func() bool {
		_, listing := h.management(http.MethodGet, "/v0/management/gateway/upstream-accounts", managementKey, "")
		return !strings.Contains(listing, importID)
	})

	// Login: CPA exchanges the code through the chosen egress, saves the
	// credential with it, and the account is bound to its committed route.
	h.resetObservations()
	status, body = h.management(http.MethodPost, credentialPage+"/oauth", managementKey,
		`{"registration_id":"`+loginRegistration+`","egress_proxy_url":"`+loginRoute+`"}`)
	var started struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &started) != nil {
		t.Fatalf("login start: %d %s", status, body)
	}
	authorize, err := url.Parse(started.URL)
	if err != nil || authorize.Host != "auth.openai.com" || authorize.Query().Get("state") != started.State || started.State == "" {
		t.Fatalf("authorization URL %q", started.URL)
	}
	poll := func() (int, string) {
		return h.management(http.MethodPost, credentialPage+"/oauth/status", managementKey, `{"state":"`+started.State+`"}`)
	}
	if status, body = poll(); status != http.StatusOK || body != `{"status":"wait"}` {
		t.Fatalf("login before the callback: %d %s", status, body)
	}
	if len(h.tokenCalls()) != 0 {
		t.Fatal("a provider was contacted before the Owner authorized")
	}
	if status, body = h.management(http.MethodPost, credentialPage+"/oauth/callback", managementKey, `{"state":"`+started.State+`","code":"code-login"}`); status != http.StatusOK {
		t.Fatalf("callback: %d %s", status, body)
	}
	loginID := stableIndex("acct-login")
	h.eventually("the login completes", func() bool {
		status, body = poll()
		return body == `{"account_id":"`+loginID+`","status":"ok"}`
	})
	// The code exchange, and any refresh CPA follows it with, leave through
	// the chosen egress.
	calls = h.tokenCalls()
	if len(calls) == 0 || calls[0].Form.Get("grant_type") != "authorization_code" || calls[0].Form.Get("code") != "code-login" {
		t.Fatalf("the code was not exchanged: %+v", calls)
	}
	for _, call := range calls {
		if call.Via != "account-egress" || call.ProxyAuth != basicAuth("reg-login", "secret-reg-login") {
			t.Fatalf("the login did not use the chosen egress: %+v", call)
		}
	}
	if _, proxy := h.credentialFor("acct-login"); proxy != loginCommitted {
		t.Fatalf("the logged-in credential carries %q, want the committed route", proxy)
	}
	for _, call := range h.provider.seen() {
		if call.Via == "global" {
			t.Fatalf("a credential operation used the global egress: %+v", call)
		}
	}
	// Another state is not this plugin's login.
	if status, _ = h.management(http.MethodPost, credentialPage+"/oauth/status", managementKey, `{"state":"unknown-state-0123456789"}`); status != http.StatusNotFound {
		t.Fatalf("an unknown login answered %d", status)
	}
	// Only the management key reaches these routes.
	if status, _ = h.management(http.MethodPost, credentialPage+"/credentials", internalKey, `{"refresh_token":"rt-x"}`); status != http.StatusUnauthorized {
		t.Fatalf("the data-plane key reached the import: %d", status)
	}
}
