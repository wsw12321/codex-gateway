package gatewayplugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// manage calls one management route and returns its status and JSON body.
func (f *fixture) manage(method, path string, query url.Values, body string) (int, map[string]json.RawMessage, string) {
	f.t.Helper()
	var env envelope
	var response pluginapi.ManagementResponse
	raw := f.plugin.Management(method, managementBase+path, query, []byte(body))
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK || json.Unmarshal(env.Result, &response) != nil {
		f.t.Fatalf("management did not answer with a response envelope: %s", raw)
	}
	if response.Headers.Get("Content-Type") != "application/json" || response.Headers.Get("Cache-Control") != "no-store" {
		f.t.Fatalf("unexpected response headers: %v", response.Headers)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &fields); err != nil {
		f.t.Fatalf("management body is not a JSON object: %s", response.Body)
	}
	return response.StatusCode, fields, string(response.Body)
}

func TestManagementRoutesAreExactAndAuthenticatedOnly(t *testing.T) {
	f := newFixture(t, 1)
	raw, _ := json.Marshal(f.plugin.managementRoutes())
	if strings.Contains(string(raw), "Menu") || strings.Contains(string(raw), "resources") {
		t.Fatalf("a menu or resource route would be served without the management key: %s", raw)
	}
	routes := f.plugin.managementRoutes().Routes
	// Six per account namespace, the Codex quota query, and seven credential
	// page routes per provider (quota, login start/callback/status, import,
	// refresh, delete).
	if len(routes) != 40 {
		t.Fatalf("%d routes declared, want 40", len(routes))
	}
	seen := map[string]bool{}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		// The host rejects ':' and '*', and matches the path exactly.
		if seen[key] || !strings.HasPrefix(route.Path, managementPrefix) || strings.ContainsAny(route.Path, ":*? ") {
			t.Fatalf("invalid or duplicate route %q", key)
		}
		seen[key] = true
	}
}

func TestManagementCapabilities(t *testing.T) {
	f := newFixture(t, 1)
	for namespace, anthropic := range map[string]bool{"upstream-accounts": false, "antigravity-accounts": false, "anthropic-accounts": true} {
		status, fields, body := f.manage(http.MethodGet, managementPrefix+namespace+"/capabilities", nil, "")
		if status != http.StatusOK || string(fields["protocol"]) != `"upstream_account_access_v1"` || string(fields["group_upstream_account_scope"]) != `"group_upstream_account_scope_v1"` {
			t.Fatalf("%s: %d %s", namespace, status, body)
		}
		if _, has := fields["anthropic_messages"]; has != anthropic {
			t.Fatalf("%s: anthropic_messages present = %v", namespace, has)
		}
		if string(fields["account_egress"]) != `"account_egress_v1"` {
			t.Fatalf("%s does not advertise account egress: %s", namespace, body)
		}
		if status, fields, _ = f.manage(http.MethodGet, managementPrefix+namespace+"/funding-layers", nil, ""); status != http.StatusOK || string(fields["protocol"]) != `"group_upstream_account_layers_v1"` || len(fields) != 1 {
			t.Fatalf("%s funding layers: %d %v", namespace, status, fields)
		}
	}
	// Without the management key the plugin cannot write a credential's proxy,
	// so it must not claim to enforce bindings.
	f.plugin.cfg.ManagementKey = ""
	if _, fields, body := f.manage(http.MethodGet, managementPrefix+"upstream-accounts/capabilities", nil, ""); fields["account_egress"] != nil {
		t.Fatalf("account egress advertised without the management key: %s", body)
	}
	f.plugin.cfg.ManagementKey = testManagementKey
	if status, _, _ := f.manage(http.MethodGet, managementPrefix+"unknown-accounts/capabilities", nil, ""); status != http.StatusNotFound {
		t.Fatalf("an unknown namespace answered %d", status)
	}
	f.host.listErr = os.ErrPermission
	if status, _, _ := f.manage(http.MethodGet, managementPrefix+"upstream-accounts/capabilities", nil, ""); status != http.StatusServiceUnavailable {
		t.Fatalf("capabilities answered %d while the credential registry is unreadable", status)
	}
	f.host.listErr = nil
	f.plugin.cfg.AntigravityEnabled = false
	if status, _, _ := f.manage(http.MethodGet, managementPrefix+"antigravity-accounts/capabilities", nil, ""); status != http.StatusServiceUnavailable {
		t.Fatalf("a disabled provider answered %d", status)
	}
	f.plugin.unavailable = ErrStateUnavailable
	if status, _, _ := f.manage(http.MethodGet, managementPrefix+"upstream-accounts/capabilities", nil, ""); status != http.StatusServiceUnavailable {
		t.Fatalf("an unavailable plugin answered %d", status)
	}
}

func TestManagementListsAccountsInTheGatewayContract(t *testing.T) {
	f := newFixture(t, 2)
	f.host.addCodex("a-old", "acct-a", f.clock.Add(-time.Hour))
	if err := f.plugin.control.SetEnabled(f.ids["b"], false); err != nil {
		t.Fatal(err)
	}
	status, fields, body := f.manage(http.MethodGet, managementPrefix+"upstream-accounts", nil, "")
	if status != http.StatusOK || len(fields) != 1 {
		t.Fatalf("%d %s", status, body)
	}
	var accounts []map[string]string
	if err := json.Unmarshal(fields["accounts"], &accounts); err != nil {
		t.Fatalf("accounts: %v in %s", err, body)
	}
	if len(accounts) != 2 {
		t.Fatalf("%d accounts listed, want one row per real account: %s", len(accounts), body)
	}
	if accounts[0]["id"] > accounts[1]["id"] {
		t.Fatal("accounts are not sorted by id")
	}
	byID := map[string]map[string]string{}
	for _, account := range accounts {
		if len(account) != 8 {
			t.Fatalf("an account row has %d fields, the Gateway decodes exactly 8: %v", len(account), account)
		}
		byID[account["id"]] = account
	}
	a, b := byID[f.ids["a"]], byID[f.ids["b"]]
	if a["status"] != "available" || a["cliproxy_status"] != "active" || a["gateway_manual_status"] != "enabled" || a["gateway_quota_status"] != "available" {
		t.Fatalf("account a: %v", a)
	}
	if b["status"] != "unavailable" || b["gateway_manual_status"] != "manual_disabled" {
		t.Fatalf("account b: %v", b)
	}
	if a["masked_email"] != "a***@example.com" || a["plan"] != "unknown" {
		t.Fatalf("account a identity fields: %v", a)
	}
	if _, err := time.Parse(time.RFC3339, a["last_synced_at"]); err != nil {
		t.Fatalf("last_synced_at %q: %v", a["last_synced_at"], err)
	}
	if strings.Contains(body, "acct-a") || strings.Contains(body, "secret") || strings.Contains(body, "a@example.com") {
		t.Fatalf("the listing leaks credential content: %s", body)
	}
	// Other providers see none of these accounts.
	if _, fields, body = f.manage(http.MethodGet, managementPrefix+"anthropic-accounts", nil, ""); string(fields["accounts"]) != "[]" {
		t.Fatalf("Codex accounts listed under another provider: %s", body)
	}
}

func TestManagementSetAccountStatus(t *testing.T) {
	f := newFixture(t, 1)
	id := url.Values{"id": {f.ids["a"]}}
	// A conversation is idle on the account when the Owner disables it.
	if _, err := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded); err != nil {
		t.Fatal(err)
	}
	status, fields, body := f.manage(http.MethodPut, managementPrefix+"upstream-accounts/status", id, `{"enabled":false}`)
	if status != http.StatusOK || len(fields) != 5 || string(fields["id"]) != `"`+f.ids["a"]+`"` || string(fields["status"]) != `"unavailable"` || string(fields["gateway_manual_status"]) != `"manual_disabled"` {
		t.Fatalf("%d %s", status, body)
	}
	if f.active("a") != 0 {
		t.Fatal("disabling did not drop the idle hold")
	}
	if _, err := f.pick(pickOptions{}); err == nil {
		t.Fatal("a disabled account was still allocated")
	}
	status, fields, body = f.manage(http.MethodPut, managementPrefix+"upstream-accounts/status", id, ` {"enabled":true} `)
	if status != http.StatusOK || string(fields["status"]) != `"available"` {
		t.Fatalf("%d %s", status, body)
	}
	if len(f.host.resets) != 1 || f.host.resets[0] != "idx-a" {
		t.Fatalf("re-enabling did not clear the credential's cooldown: %v", f.host.resets)
	}
	if _, err := f.pick(pickOptions{}); err != nil {
		t.Fatalf("a re-enabled account was not allocated: %v", err)
	}

	for name, test := range map[string]struct {
		query url.Values
		body  string
		want  int
	}{
		"unknown account":  {url.Values{"id": {"0123456789abcdef"}}, `{"enabled":true}`, http.StatusNotFound},
		"malformed id":     {url.Values{"id": {"XYZ"}}, `{"enabled":true}`, http.StatusNotFound},
		"missing id":       {nil, `{"enabled":true}`, http.StatusNotFound},
		"two ids":          {url.Values{"id": {f.ids["a"], f.ids["a"]}}, `{"enabled":true}`, http.StatusNotFound},
		"extra parameter":  {url.Values{"id": {f.ids["a"]}, "x": {"1"}}, `{"enabled":true}`, http.StatusNotFound},
		"empty body":       {id, ``, http.StatusBadRequest},
		"string value":     {id, `{"enabled":"true"}`, http.StatusBadRequest},
		"null value":       {id, `{"enabled":null}`, http.StatusBadRequest},
		"extra field":      {id, `{"enabled":true,"x":1}`, http.StatusBadRequest},
		"duplicate field":  {id, `{"enabled":true,"enabled":false}`, http.StatusBadRequest},
		"wrong case":       {id, `{"Enabled":true}`, http.StatusBadRequest},
		"trailing value":   {id, `{"enabled":true}{}`, http.StatusBadRequest},
		"oversized":        {id, `{"enabled":true}` + strings.Repeat(" ", maxStatusRequestBytes), http.StatusBadRequest},
		"another provider": {id, `{"enabled":true}`, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			namespace := "upstream-accounts"
			if name == "another provider" {
				namespace = "anthropic-accounts"
			}
			if status, _, body := f.manage(http.MethodPut, managementPrefix+namespace+"/status", test.query, test.body); status != test.want {
				t.Fatalf("status %d, want %d: %s", status, test.want, body)
			}
		})
	}

	// A change that cannot be made durable is refused, and never enables.
	f.plugin.control.write = func(string, []byte) error { return os.ErrPermission }
	status, _, body = f.manage(http.MethodPut, managementPrefix+"upstream-accounts/status", id, `{"enabled":true}`)
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "account_status_persistence_failed") {
		t.Fatalf("%d %s", status, body)
	}
	if !f.plugin.control.Blocked(f.ids["a"]) {
		t.Fatal("an unpersisted enable left the account routable")
	}
}

func TestManagementConcurrencySnapshot(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.limits[f.ids["a"]] = 3
	f.alloc.prefer = []string{f.ids["a"]}
	f.mustPick(pickOptions{session: "codex:s1"})
	f.mustPick(pickOptions{session: "codex:s2"})
	status, fields, body := f.manage(http.MethodGet, managementPrefix+"upstream-accounts/concurrency", nil, "")
	if status != http.StatusOK || len(fields) != 2 {
		t.Fatalf("%d %s", status, body)
	}
	var sampled time.Time
	if err := json.Unmarshal(fields["sampled_at"], &sampled); err != nil || !sampled.Equal(f.clock) {
		t.Fatalf("sampled_at %s: %v", fields["sampled_at"], err)
	}
	var rows []AccountConcurrency
	if err := json.Unmarshal(fields["accounts"], &rows); err != nil || len(rows) != 2 {
		t.Fatalf("accounts: %v in %s", err, body)
	}
	for _, row := range rows {
		want := int64(0)
		if row.ID == f.ids["a"] {
			want = 2
		}
		if row.ActiveRequests != want || row.ConcurrentLimit < 1 {
			t.Fatalf("unexpected row %+v", row)
		}
	}
	if _, fields, body = f.manage(http.MethodGet, managementPrefix+"antigravity-accounts/concurrency", nil, ""); string(fields["accounts"]) != "[]" {
		t.Fatalf("another provider sees Codex conversations: %s", body)
	}
}

func TestMaskEmail(t *testing.T) {
	for email, want := range map[string]string{
		"alice@example.com":      "a***@example.com",
		" Bob@Sub.Example.org ":  "B***@Sub.Example.org",
		"":                       "u***@unknown.invalid",
		"no-at-sign":             "u***@unknown.invalid",
		"two@@example.com":       "u***@unknown.invalid",
		"@example.com":           "u***@unknown.invalid",
		"_x@example.com":         "u***@unknown.invalid",
		"a@-bad.example":         "u***@unknown.invalid",
		"a@exa mple.com":         "u***@unknown.invalid",
		"a@example..com":         "u***@unknown.invalid",
		"a@x":                    "u***@unknown.invalid",
		"a@<script>.example.com": "u***@unknown.invalid",
		"用户@example.com":         "u***@unknown.invalid",
	} {
		if got := maskEmail(email); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", email, got, want)
		}
	}
}
