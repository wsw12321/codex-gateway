package gatewayplugin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func jsonMarshal(value any) ([]byte, error) { return json.Marshal(value) }

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}

func TestLoadConfig(t *testing.T) {
	files := map[string]string{"/k/internal": testInternalKey + "\n", "/k/manage": testManagementKey, "/k/short": "short", "/k/bad": strings.Repeat("!", 50)}
	read := func(path string) ([]byte, error) {
		if value, ok := files[path]; ok {
			return []byte(value), nil
		}
		return nil, os.ErrNotExist
	}
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	base := map[string]string{"CLIPROXY_API_KEY_FILE": "/k/internal", "CPA_MANAGEMENT_KEY_FILE": "/k/manage", "CPA_PLUGIN_AUTH_DIR": "/auth"}
	cfg, err := LoadConfig(env(base), read)
	if err != nil || cfg.InternalKey != testInternalKey || cfg.ManagementKey != testManagementKey || cfg.GatewayURL != defaultGatewayURL || !cfg.AntigravityEnabled || cfg.GlobalProxyURL != "" {
		t.Fatalf("unexpected config %+v, %v", cfg, err)
	}
	for _, proxy := range []string{"http://egress-allowlist:3128", "socks5h://u:p@egress:1080"} {
		withProxy := map[string]string{"CPA_PLUGIN_GLOBAL_PROXY_URL": proxy}
		for k, v := range base {
			withProxy[k] = v
		}
		if cfg, err = LoadConfig(env(withProxy), read); err != nil || cfg.GlobalProxyURL != proxy {
			t.Fatalf("global proxy %q: %+v, %v", proxy, cfg, err)
		}
	}
	for name, change := range map[string][2]string{
		"missing auth dir":       {"CPA_PLUGIN_AUTH_DIR", ""},
		"missing key":            {"CLIPROXY_API_KEY_FILE", "/k/none"},
		"short key":              {"CLIPROXY_API_KEY_FILE", "/k/short"},
		"invalid key":            {"CLIPROXY_API_KEY_FILE", "/k/bad"},
		"shared management key":  {"CPA_MANAGEMENT_KEY_FILE", "/k/internal"},
		"gateway url with path":  {"CPA_PLUGIN_GATEWAY_URL", "http://gateway:8080/x"},
		"gateway url with creds": {"CPA_PLUGIN_GATEWAY_URL", "http://u:p@gateway:8080"},
		"https gateway":          {"CPA_PLUGIN_GATEWAY_URL", "https://gateway"},
		"global proxy with path": {"CPA_PLUGIN_GLOBAL_PROXY_URL", "http://egress:3128/x"},
		"global proxy scheme":    {"CPA_PLUGIN_GLOBAL_PROXY_URL", "ftp://egress:21"},
		"global proxy no host":   {"CPA_PLUGIN_GLOBAL_PROXY_URL", "direct"},
	} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			values[change[0]] = change[1]
			if _, err := LoadConfig(env(values), read); err == nil {
				t.Fatal("an invalid configuration was accepted")
			}
		})
	}
}

func TestDecodeScope(t *testing.T) {
	a, b := "0123456789abcdef", "fedcba9876543210"
	scope, ok := decodeScope(groupsHeader(groupLayer(testGroupA, a, b), groupLayer(testGroupB, b), personalLayer))
	if !ok || scope.Restricted || len(scope.Layers) != 3 || !scope.Layers[2].All || !scope.IDs[a] || !scope.IDs[b] {
		t.Fatalf("unexpected scope %+v, %v", scope, ok)
	}
	if scope, ok = decodeScope(groupsHeader(groupLayer(testGroupA, a))); !ok || !scope.Restricted {
		t.Fatalf("a group-only scope must be restricted: %+v, %v", scope, ok)
	}
	invalid := map[string]string{
		"empty":                 ``,
		"null":                  `null`,
		"object":                `{}`,
		"personal not last":     groupsHeader(personalLayer, groupLayer(testGroupA, a)),
		"personal with ids":     `[{"group_id":"00000000-0000-0000-0000-000000000000","account_ids":[]}]`,
		"group with all":        `[{"group_id":"` + testGroupA + `","all_accounts":true}]`,
		"all_accounts false":    `[{"group_id":"00000000-0000-0000-0000-000000000000","all_accounts":false}]`,
		"duplicate group":       groupsHeader(groupLayer(testGroupA, a), groupLayer(testGroupA, b)),
		"duplicate account":     groupsHeader(groupLayer(testGroupA, a, a)),
		"uppercase account":     groupsHeader(groupLayer(testGroupA, "0123456789ABCDEF")),
		"short account":         groupsHeader(groupLayer(testGroupA, "0123")),
		"unknown field":         `[{"group_id":"` + testGroupA + `","account_ids":[],"x":1}]`,
		"duplicate key":         `[{"group_id":"` + testGroupA + `","group_id":"` + testGroupB + `","account_ids":[]}]`,
		"missing account_ids":   `[{"group_id":"` + testGroupA + `"}]`,
		"three fields":          `[{"group_id":"` + testGroupA + `","account_ids":[],"all_accounts":true}]`,
		"bad group id":          `[{"group_id":"x","account_ids":[]}]`,
		"oversized":             `[` + strings.Repeat(" ", maxGroupsHeader) + `]`,
		"trailing data in item": `[{"group_id":"` + testGroupA + `","account_ids":[]} 1]`,
	}
	for name, raw := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, ok := decodeScope(raw); ok {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	f := newFixture(t, 1)
	headers := func(key string) http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+key)
		return h
	}
	if got := f.plugin.Authenticate(headers(testInternalKey)); !got.Authenticated || got.Principal != "gateway" {
		t.Fatalf("the Gateway was not admitted: %+v", got)
	}
	withAffinity := headers(testInternalKey)
	withAffinity.Set(HeaderAffinity, b64(7))
	if got := f.plugin.Authenticate(withAffinity); !got.Authenticated || got.Principal != b64(7) {
		t.Fatalf("the caller scope did not become the principal: %+v", got)
	}
	for name, h := range map[string]http.Header{
		"management key":  headers(testManagementKey),
		"wrong key":       headers(strings.Repeat("x", 50)),
		"no header":       {},
		"two auth values": {"Authorization": {"Bearer " + testInternalKey, "Bearer " + testInternalKey}},
	} {
		if got := f.plugin.Authenticate(h); got.Authenticated || got.DenyStatusCode != http.StatusUnauthorized {
			t.Fatalf("%s was admitted: %+v", name, got)
		}
	}
	bad := headers(testInternalKey)
	bad.Set(HeaderUser, "not-a-uuid")
	if got := f.plugin.Authenticate(bad); got.Authenticated || got.DenyStatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed identity was admitted: %+v", got)
	}
}

func TestInterceptAfterClearsInternalHeaders(t *testing.T) {
	f := newFixture(t, 1)
	request := interceptRequest{}
	request.Headers = http.Header{}
	for _, name := range []string{HeaderUser, HeaderAffinity, HeaderGroups, HeaderRequest, "X-Codex-Upstream-Account", "X-Codex-Conversation-Hash", "X-Codex-Diagnostic-Run"} {
		request.Headers.Set(name, "v")
	}
	request.Headers.Set("Session_id", "keep")
	request.Headers.Set("X-Codex-Turn-Metadata", "keep")
	raw, _ := json.Marshal(request)
	var env envelope
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(f.plugin.Handle("request.intercept_after", raw), &env); err != nil || !env.OK || json.Unmarshal(env.Result, &response) != nil {
		t.Fatalf("unexpected response: %v", err)
	}
	cleared := map[string]bool{}
	for _, name := range response.ClearHeaders {
		cleared[http.CanonicalHeaderKey(name)] = true
	}
	if len(cleared) != 7 || cleared["Session_id"] || cleared["X-Codex-Turn-Metadata"] {
		t.Fatalf("cleared %v, want exactly the seven internal headers", response.ClearHeaders)
	}
}

func TestControlStateDocument(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadControlState(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef"
	if state.Blocked(id) {
		t.Fatal("missing state must mean healthy")
	}
	if err = state.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, AccountStateFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	reloaded, err := LoadControlState(dir)
	if err != nil || !reloaded.Blocked(id) {
		t.Fatalf("the manual disable did not persist: %v", err)
	}
	if manual, quota := reloaded.Statuses(id); manual != "manual_disabled" || quota != "available" {
		t.Fatalf("statuses = %s, %s", manual, quota)
	}
	// The schema is the patched image's: it must read a document it wrote.
	legacy := `{"version":1,"accounts":{"` + id + `":{"enabled":false,"manual_disabled":false,"quota_exhausted":true,"version":3,"updated_at":"2026-11-01T00:00:00Z"}}}`
	if err = os.WriteFile(filepath.Join(dir, AccountStateFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if reloaded, err = LoadControlState(dir); err != nil || reloaded.Version(id) != 3 {
		t.Fatalf("a legacy document was not read: %v", err)
	}
	if _, quota := reloaded.Statuses(id); quota != "quota_exhausted" {
		t.Fatalf("quota status = %s", quota)
	}
	for name, doc := range map[string]string{
		"garbage":            `not json`,
		"wrong version":      `{"version":2,"accounts":{}}`,
		"extra field":        `{"version":1,"accounts":{},"x":1}`,
		"duplicate key":      `{"version":1,"version":1,"accounts":{}}`,
		"bad account id":     `{"version":1,"accounts":{"XYZ":{"enabled":true,"manual_disabled":false,"quota_exhausted":false,"version":1,"updated_at":"2026-11-01T00:00:00Z"}}}`,
		"inconsistent flags": `{"version":1,"accounts":{"` + id + `":{"enabled":true,"manual_disabled":true,"quota_exhausted":false,"version":1,"updated_at":"2026-11-01T00:00:00Z"}}}`,
		"zero version":       `{"version":1,"accounts":{"` + id + `":{"enabled":true,"manual_disabled":false,"quota_exhausted":false,"version":0,"updated_at":"2026-11-01T00:00:00Z"}}}`,
		"non-boolean flag":   `{"version":1,"accounts":{"` + id + `":{"enabled":1,"manual_disabled":false,"quota_exhausted":false,"version":1,"updated_at":"2026-11-01T00:00:00Z"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, AccountStateFile), []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadControlState(dir); err == nil {
				t.Fatal("a malformed control document was treated as usable")
			}
		})
	}
}

func TestControlStateWriteFailureIsConservative(t *testing.T) {
	state, err := LoadControlState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef"
	state.write = func(string, []byte) error { return os.ErrPermission }
	if err = state.SetEnabled(id, true); err == nil {
		t.Fatal("an unpersisted enable was acknowledged")
	}
	if !state.Blocked(id) {
		t.Fatal("a failed enable must not route")
	}
}

func TestCodexIdentity(t *testing.T) {
	claims := func(account string) string {
		payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
		return "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
	}
	cases := []struct {
		name string
		doc  string
		want string
		ok   bool
	}{
		{"top level", `{"account_id":"acct-1"}`, "acct-1", true},
		{"claim only", `{"id_token":"` + claims("acct-2") + `"}`, "acct-2", true},
		{"both agree", `{"account_id":"acct-3","id_token":"` + claims("acct-3") + `"}`, "acct-3", true},
		{"conflict", `{"account_id":"acct-3","id_token":"` + claims("acct-4") + `"}`, "", false},
		{"missing", `{"email":"a@example.com"}`, "", false},
		{"invalid characters", `{"account_id":"acct 1"}`, "", false},
		{"malformed token", `{"account_id":"acct-1","id_token":"not-a-jwt"}`, "", false},
		{"non-string", `{"account_id":7}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var document map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.doc), &document); err != nil {
				t.Fatal(err)
			}
			got, ok := codexIdentity(document)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	// The index algorithm is shared with the patched image and the Gateway
	// database, so it must never change. These values were produced by the
	// patched CPA v8.0.59 implementation (CodexOAuthAccountIndex).
	for accountID, want := range map[string]string{
		"acct-0000-1111":  "661ca22870de22ce",
		"acct-a":          "a1842660dc9065d8",
		"org-AbC_123.x:y": "c600419eed5f1763",
	} {
		if got := CodexAccountIndex(accountID); got != want {
			t.Fatalf("CodexAccountIndex(%q) = %s, want %s", accountID, got, want)
		}
	}
}

func TestInventoryReadsDocumentsOnlyWhenChanged(t *testing.T) {
	f := newFixture(t, 2)
	if _, err := f.plugin.inventory.Resolve([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if f.host.gets != 2 {
		t.Fatalf("read %d documents, want 2", f.host.gets)
	}
	f.advance(time.Minute)
	if _, err := f.plugin.inventory.Resolve([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if f.host.gets != 2 {
		t.Fatalf("unchanged credentials were read again (%d reads)", f.host.gets)
	}
	// A credential added later is usable on the next pick.
	f.ids["c"] = f.host.addCodex("c", "acct-c", f.clock)
	f.advance(2 * time.Second)
	resolved, err := f.plugin.inventory.Resolve([]string{"c"})
	if err != nil || resolved["c"] == nil || resolved["c"].ID != f.ids["c"] {
		t.Fatalf("a new credential was not resolved: %v", err)
	}
}

func TestIdentityRegistry(t *testing.T) {
	dir := t.TempDir()
	registry, err := loadIdentityRegistry(dir, AntigravityIdentityFile, antigravityIdentityDomain, validGoogleSubject)
	if err != nil {
		t.Fatal(err)
	}
	id, err := registry.register("1234567890")
	if err != nil || !validAccountID(id) {
		t.Fatalf("register = %q, %v", id, err)
	}
	if again, _ := registry.register("1234567890"); again != id {
		t.Fatal("a subject changed its index")
	}
	reloaded, err := loadIdentityRegistry(dir, AntigravityIdentityFile, antigravityIdentityDomain, validGoogleSubject)
	if got, ok := reloaded.lookup("1234567890"); err != nil || !ok || got != id {
		t.Fatalf("the mapping did not persist: %v", err)
	}
	if _, err = registry.register("bad subject"); err == nil {
		t.Fatal("an invalid subject was registered")
	}
	// A registry readable by others is refused rather than trusted.
	if err = os.Chmod(filepath.Join(dir, AntigravityIdentityFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = loadIdentityRegistry(dir, AntigravityIdentityFile, antigravityIdentityDomain, validGoogleSubject); err == nil {
		t.Fatal("a registry with loose permissions was accepted")
	}
}

func TestRootRegistry(t *testing.T) {
	registry := NewRootRegistry()
	clock := time.Unix(0, 0)
	registry.now = func() time.Time { return clock }
	if got := registry.Resolve("s", "child", "root"); got != "root" {
		t.Fatalf("root = %q", got)
	}
	// The relation is remembered for later requests that omit the parent.
	if got := registry.Resolve("s", "child", ""); got != "root" {
		t.Fatalf("remembered root = %q", got)
	}
	if got := registry.Resolve("s", "grandchild", "child"); got != "root" {
		t.Fatalf("transitive root = %q", got)
	}
	if got := registry.Resolve("other", "child", ""); got != "child" {
		t.Fatalf("a relation leaked across caller scopes: %q", got)
	}
	if got := registry.Resolve("s", "a:agent:b:agent:c", ""); got != "a" {
		t.Fatalf("hierarchy root = %q", got)
	}
	// A cycle resolves to the leaf and is forgotten.
	registry.Resolve("s", "x", "y")
	if got := registry.Resolve("s", "y", "x"); got != "y" {
		t.Fatalf("cycle resolved to %q", got)
	}
	clock = clock.Add(2 * time.Hour)
	if got := registry.Resolve("s", "child", ""); got != "child" {
		t.Fatalf("an expired relation was still used: %q", got)
	}
	if registry.Resolve("s", "bad\x00id", "") != "" {
		t.Fatal("a control character was accepted in a session id")
	}
}

func TestConversationHashShape(t *testing.T) {
	hash := ConversationHash("scope", "root")
	if len(hash) != 37 || !strings.HasPrefix(hash, "conv-") || hash != strings.ToLower(hash) {
		t.Fatalf("unexpected hash %q", hash)
	}
	if hash == ConversationHash("other", "root") || hash == ConversationHash("scope", "other") {
		t.Fatal("the hash ignores one of its inputs")
	}
	if ConversationHash("scope", "") != "" {
		t.Fatal("a request without a session got a conversation hash")
	}
}

func TestConcurrencySnapshotAndDisableDrain(t *testing.T) {
	c := NewConcurrency(5 * time.Minute)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }
	c.SetLimit("acct", 2)
	release, ok := c.Acquire("acct", "root:one")
	overlap, _ := c.Acquire("acct", "root:one")
	if !ok {
		t.Fatal("acquire failed")
	}
	held, _ := c.Acquire("acct", "root:two")
	held(SlotHold)
	rows := c.Snapshot([]string{"acct", "idle"})
	if len(rows) != 2 || rows[0].ID != "acct" || rows[0].ActiveRequests != 2 || rows[0].ConcurrentLimit != 2 || rows[1].ActiveRequests != 0 {
		t.Fatalf("unexpected snapshot %+v", rows)
	}
	// Disabling drops idle holds at once; in-flight executions finish.
	c.DropIdleHolds("acct")
	if rows = c.Snapshot(nil); len(rows) != 1 || rows[0].ActiveRequests != 1 {
		t.Fatalf("after disable %+v, want only the in-flight root", rows)
	}
	release(SlotHold)
	release(SlotHold)
	if rows = c.Snapshot(nil); rows[0].ActiveRequests != 1 {
		t.Fatalf("an overlapping execution was still running, snapshot %+v", rows)
	}
	overlap(SlotDrop)
	if rows = c.Snapshot(nil); len(rows) != 0 {
		t.Fatalf("a dropped slot is still occupied: %+v", rows)
	}
}

func TestBindingsExpireAndEvict(t *testing.T) {
	b := NewBindings(time.Hour)
	clock := time.Unix(0, 0)
	b.now = func() time.Time { return clock }
	b.capacity = 2
	k1, k2, k3 := newBindingKey("s", "codex", "one", "M"), newBindingKey("s", "codex", "two", "m"), newBindingKey("s", "codex", "three", "m")
	b.Set(k1, "a")
	b.Set(k2, "b")
	if got, ok := b.Get(newBindingKey("s", "codex", "one", " m ")); !ok || got != "a" {
		t.Fatal("model keys must be case- and space-insensitive")
	}
	b.Set(k3, "c") // evicts the least recently used: k2
	if _, ok := b.Get(k2); ok {
		t.Fatal("the least recently used binding survived eviction")
	}
	b.Delete(k1, "other")
	if _, ok := b.Get(k1); !ok {
		t.Fatal("a delete for another account removed the binding")
	}
	clock = clock.Add(61 * time.Minute)
	if _, ok := b.Get(k1); ok {
		t.Fatal("an expired binding was returned")
	}
}
