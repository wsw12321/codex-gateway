package gatewayplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAccessToken = "synthetic-access-token"
	// codexUsageDocument is a ChatGPT usage answer with every field the
	// normalizer reads, plus fields it must ignore.
	codexUsageDocument = `{"plan_type":"plus","email":"someone@example.com",` +
		`"rate_limit":{"allowed":true,"limit_reached":false,` +
		`"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1794200000},` +
		`"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1794700000}},` +
		`"additional_rate_limits":[{"limit_name":"Code review","metered_feature":"code_review",` +
		`"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1794700000},"secondary_window":null}}],` +
		`"rate_limit_reached_type":{"type":"rate_limit_reached"},"credits":{"has_credits":false}}`
)

// fakeQuotaHTTP records each outbound quota request and answers from a script.
type fakeQuotaHTTP struct {
	mu     sync.Mutex
	calls  []quotaCall
	status int
	body   string
	err    error
}

type quotaCall struct {
	Proxy  string
	Method string
	URL    string
	Header http.Header
	Body   string
}

func (q *fakeQuotaHTTP) do(_ context.Context, proxyURL string, request *http.Request) (*http.Response, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
	}
	q.calls = append(q.calls, quotaCall{Proxy: proxyURL, Method: request.Method, URL: request.URL.String(), Header: request.Header.Clone(), Body: string(body)})
	if q.err != nil {
		return nil, q.err
	}
	status := q.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(q.body))}, nil
}

func (q *fakeQuotaHTTP) seen() []quotaCall {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]quotaCall(nil), q.calls...)
}

// quotaFixture builds a plugin with one Codex account that holds an access
// token, a global egress and a scripted provider.
func quotaFixture(t *testing.T) (*fixture, *fakeQuotaHTTP) {
	t.Helper()
	f := newFixture(t, 1)
	f.host.setDocField("idx-a", "access_token", testAccessToken)
	upstream := &fakeQuotaHTTP{body: codexUsageDocument}
	f.plugin.quotaHTTP = upstream.do
	f.plugin.cfg.GlobalProxyURL = testGlobalProxy
	return f, upstream
}

func (h *fakeHost) setDocField(authIndex, field string, value any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var document map[string]any
	_ = json.Unmarshal([]byte(h.docs[authIndex]), &document)
	if value == nil {
		delete(document, field)
	} else {
		document[field] = value
	}
	raw, _ := json.Marshal(document)
	h.docs[authIndex] = string(raw)
}

// addEntry registers a credential document of any provider.
func (h *fakeHost) addEntry(name, provider string, document map[string]any, modified time.Time) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.docs == nil {
		h.docs = map[string]string{}
	}
	raw, _ := json.Marshal(document)
	index := "idx-" + name
	h.entries = append(h.entries, AuthEntry{
		ID: name, AuthIndex: index, Name: name, Provider: provider, Status: "active", Source: "file",
		Size: int64(len(raw)), ModTime: modified, UpdatedAt: modified, LastRefresh: modified, AccountType: "oauth",
	})
	h.docs[index] = string(raw)
	return index
}

const rpcBody = `{"method":"account/rateLimits/read","id":6}`

func TestDecodeQuotaRPCRequest(t *testing.T) {
	for _, body := range []string{rpcBody, `{"id":6,"method":"account/rateLimits/read"}`, " " + rpcBody + "\n"} {
		if id, err := decodeQuotaRPCRequest([]byte(body)); err != nil || id != quotaRPCID {
			t.Fatalf("%s was refused: %d %v", body, id, err)
		}
	}
	for name, body := range map[string]string{
		"empty":            ``,
		"null":             `null`,
		"array":            `[]`,
		"wrong method":     `{"method":"account/read","id":6}`,
		"wrong id":         `{"method":"account/rateLimits/read","id":7}`,
		"fractional id":    `{"method":"account/rateLimits/read","id":6.0}`,
		"exponent id":      `{"method":"account/rateLimits/read","id":6e0}`,
		"string id":        `{"method":"account/rateLimits/read","id":"6"}`,
		"params null":      `{"method":"account/rateLimits/read","id":6,"params":null}`,
		"unknown field":    `{"method":"account/rateLimits/read","id":6,"future":true}`,
		"trailing JSON":    rpcBody + `{}`,
		"duplicate method": `{"method":"account/rateLimits/read","method":"account/rateLimits/read","id":6}`,
		"duplicate id":     `{"method":"account/rateLimits/read","id":6,"id":6}`,
		"missing id":       `{"method":"account/rateLimits/read"}`,
		"wrong case":       `{"Method":"account/rateLimits/read","id":6}`,
	} {
		if _, err := decodeQuotaRPCRequest([]byte(body)); !errors.Is(err, errQuotaRequestInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	oversized := `{"method":"account/rateLimits/read","id":6,"padding":"` + strings.Repeat("x", maxQuotaRequestBytes) + `"}`
	if _, err := decodeQuotaRPCRequest([]byte(oversized)); !errors.Is(err, errQuotaRequestTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
}

func TestNormalizeCodexUsageMatchesGolden(t *testing.T) {
	result, err := normalizeCodexUsage([]byte(codexUsageDocument))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.MarshalIndent(quotaRPCResponse{ID: quotaRPCID, Result: result}, "", "  ")
	golden := filepath.Join("testdata", "codex_quota_rpc.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err = os.WriteFile(golden, append(got, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	// The Gateway's strict parser is tested against this same file.
	if string(want) != string(got)+"\n" {
		t.Fatalf("normalized quota changed:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(string(got), "someone@example.com") || strings.Contains(string(got), "plus") {
		t.Fatalf("provider fields leaked: %s", got)
	}
}

func TestNormalizeCodexUsageShapes(t *testing.T) {
	window := func(fields string) string { return `{"rate_limit":{"primary_window":{` + fields + `}}}` }
	for name, test := range map[string]struct {
		body string
		code string
	}{
		"explicit null limit":       {`{"rate_limit":null}`, ""},
		"only additional limits":    {`{"additional_rate_limits":[]}`, ""},
		"null windows":              {`{"rate_limit":{"primary_window":null,"secondary_window":null}}`, ""},
		"window without optionals":  {window(`"used_percent":100`), ""},
		"plan only":                 {`{"plan_type":"plus"}`, "quota_rate_limit_missing"},
		"empty object":              {`{}`, "quota_rate_limit_missing"},
		"percent missing":           {window(`"limit_window_seconds":60`), "quota_percent_missing"},
		"percent above range":       {window(`"used_percent":101`), "quota_percent_out_of_range"},
		"percent below range":       {window(`"used_percent":-1`), "quota_percent_out_of_range"},
		"percent fractional":        {window(`"used_percent":12.5`), "quota_percent_fractional"},
		"window zero":               {window(`"used_percent":1,"limit_window_seconds":0`), "quota_window_invalid"},
		"window too long":           {window(`"used_percent":1,"limit_window_seconds":2678401`), "quota_window_invalid"},
		"reset too early":           {window(`"used_percent":1,"reset_at":946684799`), "quota_reset_invalid"},
		"reset too late":            {window(`"used_percent":1,"reset_at":4102444801`), "quota_reset_invalid"},
		"percent as string":         {window(`"used_percent":"1"`), "quota_field_type_invalid"},
		"limit as string":           {`{"rate_limit":"full"}`, "quota_field_type_invalid"},
		"truncated":                 {`{"rate_limit":`, "quota_json_invalid"},
		"trailing value":            {`{"rate_limit":null} {}`, "quota_json_invalid"},
		"uppercase limit id":        {`{"additional_rate_limits":[{"metered_feature":"Review"}]}`, "quota_limit_id_invalid"},
		"long limit id":             {`{"additional_rate_limits":[{"metered_feature":"` + strings.Repeat("a", 33) + `"}]}`, "quota_limit_id_invalid"},
		"limit id shadowing codex":  {`{"additional_rate_limits":[{"metered_feature":"codex"}]}`, "quota_limit_id_duplicate"},
		"duplicate additional id":   {`{"additional_rate_limits":[{"metered_feature":"a"},{"metered_feature":"a"}]}`, "quota_limit_id_duplicate"},
		"too many additional":       {`{"additional_rate_limits":[` + strings.TrimSuffix(strings.Repeat(`{"metered_feature":"x"},`, 33), ",") + `]}`, "quota_limits_excessive"},
		"additional window invalid": {`{"additional_rate_limits":[{"metered_feature":"a","rate_limit":{"primary_window":{}}}]}`, "quota_percent_missing"},
	} {
		_, err := normalizeCodexUsage([]byte(test.body))
		if test.code == "" && err != nil || test.code != "" && quotaSchemaCode(err) != test.code {
			t.Errorf("%s: got %v, want %q", name, err, test.code)
		}
	}
	// Only the known reached types are reported; anything else is withheld.
	for value, want := range map[string]string{"workspace_member_usage_limit_reached": "workspace_member_usage_limit_reached", "something_new": "", "": ""} {
		result, err := normalizeCodexUsage([]byte(`{"rate_limit":null,"rate_limit_reached_type":{"type":"` + value + `"}}`))
		got := ""
		if err == nil && result.RateLimits.RateLimitReachedType != nil {
			got = *result.RateLimits.RateLimitReachedType
		}
		if err != nil || got != want {
			t.Errorf("reached type %q: got %q, %v", value, got, err)
		}
	}
}

func TestCodexQuotaQueryUsesTheAccountRoute(t *testing.T) {
	f, upstream := quotaFixture(t)
	id := f.ids["a"]
	query := url.Values{"id": {id}}

	status, fields, body := f.manage(http.MethodPost, managementPrefix+"upstream-accounts/quota", query, rpcBody)
	if status != http.StatusOK || string(fields["id"]) != "6" {
		t.Fatalf("quota query: %d %s", status, body)
	}
	golden, _ := os.ReadFile(filepath.Join("testdata", "codex_quota_rpc.json"))
	var compact bytes.Buffer
	_ = json.Compact(&compact, golden)
	if body != compact.String() {
		t.Fatalf("quota answer differs from the golden contract:\n%s", body)
	}
	calls := upstream.seen()
	if len(calls) != 1 {
		t.Fatalf("%d upstream calls", len(calls))
	}
	call := calls[0]
	if call.Proxy != testGlobalProxy || call.Method != http.MethodGet || call.URL != codexUsageURL || call.Body != "" {
		t.Fatalf("unbound account did not use the global egress: %+v", call)
	}
	// Exactly these headers: nothing from the credential file or the caller.
	if len(call.Header) != 3 || call.Header.Get("Accept") != "application/json" ||
		call.Header.Get("Authorization") != "Bearer "+testAccessToken || call.Header.Get("Chatgpt-Account-Id") != "acct-a" {
		t.Fatalf("unexpected upstream headers: %v", call.Header)
	}

	bound := "http://route-a:secret@egress-allowlist:3129"
	f.alloc.routes = map[string]Egress{id: {ProxyURL: bound, Revision: "route:1"}}
	if status, _, body = f.manage(http.MethodPost, managementPrefix+"upstream-accounts/quota", query, rpcBody); status != http.StatusOK {
		t.Fatalf("bound quota query: %d %s", status, body)
	}
	if calls = upstream.seen(); calls[len(calls)-1].Proxy != bound {
		t.Fatalf("bound account did not use its own egress: %+v", calls[len(calls)-1])
	}
}

func TestCodexQuotaQueryNeverConnectsDirectly(t *testing.T) {
	f, upstream := quotaFixture(t)
	query := url.Values{"id": {f.ids["a"]}}
	f.plugin.cfg.GlobalProxyURL = ""
	status, fields, _ := f.manage(http.MethodPost, managementPrefix+"upstream-accounts/quota", query, rpcBody)
	if status != http.StatusBadGateway || string(fields["error"]) != `"quota_upstream_unavailable"` {
		t.Fatalf("no global egress: %d %v", status, fields)
	}
	f.plugin.cfg.GlobalProxyURL = testGlobalProxy
	f.alloc.egressErr = map[string]error{f.ids["a"]: ErrUnavailable}
	if status, fields, _ = f.manage(http.MethodPost, managementPrefix+"upstream-accounts/quota", query, rpcBody); status != http.StatusBadGateway || string(fields["error"]) != `"quota_upstream_unavailable"` {
		t.Fatalf("unresolvable route: %d %v", status, fields)
	}
	if calls := upstream.seen(); len(calls) != 0 {
		t.Fatalf("a query left without a confirmed route: %+v", calls)
	}
}

func TestCodexQuotaQueryErrors(t *testing.T) {
	for name, test := range map[string]struct {
		setup  func(*fixture, *fakeQuotaHTTP)
		query  func(*fixture) url.Values
		body   string
		status int
		code   string
		calls  int
	}{
		"invalid request":      {body: `{"method":"account/rateLimits/read","id":7}`, status: 400, code: "quota_request_invalid"},
		"oversized request":    {body: `{"method":"x","pad":"` + strings.Repeat("x", 300) + `"}`, status: 413, code: "quota_request_too_large"},
		"missing id":           {query: func(*fixture) url.Values { return url.Values{} }, status: 404, code: "upstream_account_not_found"},
		"two ids":              {query: func(f *fixture) url.Values { return url.Values{"id": {f.ids["a"], f.ids["a"]}} }, status: 404, code: "upstream_account_not_found"},
		"extra parameter":      {query: func(f *fixture) url.Values { return url.Values{"id": {f.ids["a"]}, "x": {"1"}} }, status: 404, code: "upstream_account_not_found"},
		"unknown account":      {query: func(*fixture) url.Values { return url.Values{"id": {"0123456789abcdef"}} }, status: 404, code: "upstream_account_not_found"},
		"registry unavailable": {setup: func(f *fixture, _ *fakeQuotaHTTP) { f.host.listErr = errors.New("down") }, status: 503, code: "account_registry_unavailable"},
		"disabled credential": {setup: func(f *fixture, _ *fakeQuotaHTTP) {
			f.host.entries[0].Disabled = true
		}, status: 409, code: "upstream_account_disabled"},
		"no access token": {setup: func(f *fixture, _ *fakeQuotaHTTP) {
			f.host.setDocField("idx-a", "access_token", nil)
		}, status: 502, code: "quota_credential_unavailable"},
		"unsafe access token": {setup: func(f *fixture, _ *fakeQuotaHTTP) {
			f.host.setDocField("idx-a", "access_token", "bad token\r\nX-Injected: 1")
		}, status: 502, code: "quota_credential_unavailable"},
		"credential replaced by another account": {setup: func(f *fixture, _ *fakeQuotaHTTP) {
			// The inventory still maps the file to account a; the document now
			// names another ChatGPT account.
			_, _, _ = f.plugin.inventory.Find(ProviderCodex, f.ids["a"])
			f.host.mu.Lock()
			f.host.docs["idx-a"] = strings.Replace(f.host.docs["idx-a"], `"acct-a"`, `"acct-z"`, 1)
			f.host.mu.Unlock()
		}, status: 502, code: "quota_account_identity_unavailable"},
		"transport failure":    {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.err = errors.New("dial secret-host") }, status: 502, code: "quota_upstream_unavailable", calls: 1},
		"transport timeout":    {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.err = context.DeadlineExceeded }, status: 504, code: "quota_upstream_timeout", calls: 1},
		"provider 401":         {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status = 401 }, status: 401, code: "quota_reauthentication_required", calls: 1},
		"provider 403":         {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status = 403 }, status: 403, code: "quota_reauthentication_required", calls: 1},
		"provider 429":         {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status = 429 }, status: 429, code: "quota_upstream_rate_limited", calls: 1},
		"provider 408":         {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status = 408 }, status: 504, code: "quota_upstream_timeout", calls: 1},
		"provider 504":         {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status = 504 }, status: 504, code: "quota_upstream_timeout", calls: 1},
		"provider 500":         {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status = 500 }, status: 502, code: "quota_upstream_error", calls: 1},
		"provider redirect":    {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status = 302 }, status: 502, code: "quota_upstream_error", calls: 1},
		"oversized answer":     {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.body = strings.Repeat(" ", maxQuotaResponseBytes+1) }, status: 502, code: "quota_response_invalid", calls: 1},
		"schema change":        {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.body = `{"plan_type":"plus"}` }, status: 502, code: "quota_rate_limit_missing", calls: 1},
		"provider text in 500": {setup: func(_ *fixture, q *fakeQuotaHTTP) { q.status, q.body = 500, `{"error":"secret-provider-text"}` }, status: 502, code: "quota_upstream_error", calls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f, upstream := quotaFixture(t)
			if test.setup != nil {
				test.setup(f, upstream)
			}
			query := url.Values{"id": {f.ids["a"]}}
			if test.query != nil {
				query = test.query(f)
			}
			body := test.body
			if body == "" {
				body = rpcBody
			}
			status, fields, raw := f.manage(http.MethodPost, managementPrefix+"upstream-accounts/quota", query, body)
			if status != test.status || len(fields) != 1 || string(fields["error"]) != `"`+test.code+`"` {
				t.Fatalf("got %d %s, want %d %s", status, raw, test.status, test.code)
			}
			if strings.Contains(raw, "secret") || strings.Contains(raw, testAccessToken) {
				t.Fatalf("provider or credential text leaked: %s", raw)
			}
			if calls := len(upstream.seen()); calls != test.calls {
				t.Fatalf("%d upstream calls, want %d", calls, test.calls)
			}
		})
	}
}

func TestQuotaRoutesOnlyForCodexNamespace(t *testing.T) {
	f, upstream := quotaFixture(t)
	for _, namespace := range []string{"antigravity-accounts", "anthropic-accounts"} {
		if status, _, _ := f.manage(http.MethodPost, managementPrefix+namespace+"/quota", url.Values{"id": {f.ids["a"]}}, rpcBody); status != http.StatusNotFound {
			t.Fatalf("%s/quota answered %d", namespace, status)
		}
	}
	if status, _, _ := f.manage(http.MethodGet, managementPrefix+"upstream-accounts/quota", url.Values{"id": {f.ids["a"]}}, ""); status != http.StatusNotFound {
		t.Fatalf("GET quota answered %d", status)
	}
	if len(upstream.seen()) != 0 {
		t.Fatal("an unrouted request reached the provider")
	}
}

func TestCodexQuotaFacade(t *testing.T) {
	f, upstream := quotaFixture(t)
	query := url.Values{"id": {f.ids["a"]}}
	path := managementPrefix + facadeNamespace + "/codex/accounts/quota"
	for _, body := range []string{"{}", ""} {
		status, fields, raw := f.manage(http.MethodPost, path, query, body)
		if status != http.StatusOK || string(fields["status"]) != `"ok"` ||
			string(fields["quota"]) != `[{"model":"code_review","remaining_fraction":1},{"model":"codex","remaining_fraction":0.75}]` {
			t.Fatalf("facade quota: %d %s", status, raw)
		}
	}
	if status, fields, _ := f.manage(http.MethodPost, path, query, `{"x":1}`); status != http.StatusBadRequest || string(fields["status"]) != `"error"` {
		t.Fatalf("facade accepted a body: %d %v", status, fields)
	}
	if status, _, _ := f.manage(http.MethodPost, path, url.Values{"id": {"0123456789abcdef"}}, "{}"); status != http.StatusNotFound {
		t.Fatalf("unknown account answered %d", status)
	}
	upstream.status = http.StatusUnauthorized
	if status, fields, raw := f.manage(http.MethodPost, path, query, "{}"); status != http.StatusBadGateway || len(fields) != 1 || string(fields["status"]) != `"error"` {
		t.Fatalf("facade error: %d %s", status, raw)
	}
	if status, _, _ := f.manage(http.MethodGet, path, query, ""); status != http.StatusNotFound {
		t.Fatalf("GET facade quota answered %d", status)
	}
	for _, other := range []string{"/codex/accounts/refresh", "/unknown/accounts/quota", "/codex/accounts"} {
		if status, _, _ := f.manage(http.MethodPost, managementPrefix+facadeNamespace+other, query, "{}"); status != http.StatusNotFound {
			t.Fatalf("%s answered %d", other, status)
		}
	}
}

func antigravityFixture(t *testing.T) (*fixture, string, *fakeQuotaHTTP) {
	t.Helper()
	f, upstream := quotaFixture(t)
	const subject = "109876543210"
	id, err := f.plugin.inventory.antigravity.register(subject)
	if err != nil {
		t.Fatal(err)
	}
	f.host.addEntry("g", ProviderAntigravity, map[string]any{
		"type": "antigravity", "google_subject": subject, "email": "g@example.com",
		"access_token": testAccessToken, "project_id": "project-1", "refresh_token": "secret",
	}, f.clock)
	upstream.body = `{"models":{` +
		`"gemini-3-flash":{"quotaInfo":{"remainingFraction":0.5,"resetTime":"2026-11-12T05:00:00Z"}},` +
		`"gemini-3.1-flash-lite":{"quotaInfo":{"remainingFraction":1}},` +
		`"gemini-bad-reset":{"quotaInfo":{"remainingFraction":0.25,"resetTime":"tomorrow<script>"}},` +
		`"gemini-out-of-range":{"quotaInfo":{"remainingFraction":1.5}},` +
		`"gemini-<script>":{"quotaInfo":{"remainingFraction":0.1}},` +
		`"claude-opus":{"quotaInfo":{"remainingFraction":0.9}},` +
		`"gemini-no-quota":{}}}`
	return f, id, upstream
}

func TestAntigravityQuotaFacade(t *testing.T) {
	f, id, upstream := antigravityFixture(t)
	path := managementPrefix + facadeNamespace + "/antigravity/accounts/quota"
	status, fields, raw := f.manage(http.MethodPost, path, url.Values{"id": {id}}, "{}")
	want := `[{"model":"gemini-3-flash","remaining_fraction":0.5,"reset_time":"2026-11-12T05:00:00Z"},` +
		`{"model":"gemini-3.1-flash-lite","remaining_fraction":1},{"model":"gemini-bad-reset","remaining_fraction":0.25}]`
	if status != http.StatusOK || string(fields["quota"]) != want {
		t.Fatalf("antigravity quota: %d %s", status, raw)
	}
	call := upstream.seen()[0]
	if call.Proxy != testGlobalProxy || call.Method != http.MethodPost || call.URL != antigravityQuotaURL || call.Body != `{"project":"project-1"}` ||
		len(call.Header) != 3 || call.Header.Get("Authorization") != "Bearer "+testAccessToken ||
		call.Header.Get("Content-Type") != "application/json" || call.Header.Get("User-Agent") != antigravityUserAgent {
		t.Fatalf("unexpected antigravity request: %+v", call)
	}
	for name, body := range map[string]string{"no models": `{}`, "only invalid": `{"models":{"claude":{"quotaInfo":{"remainingFraction":1}}}}`, "not json": `<html>`} {
		upstream.body = body
		if status, fields, _ = f.manage(http.MethodPost, path, url.Values{"id": {id}}, "{}"); status != http.StatusBadGateway || string(fields["status"]) != `"error"` {
			t.Fatalf("%s: %d %v", name, status, fields)
		}
	}
	// During a bridge rollback this CPA must not use Google credentials.
	f.plugin.cfg.AntigravityEnabled = false
	calls := len(upstream.seen())
	if status, _, _ = f.manage(http.MethodPost, path, url.Values{"id": {id}}, "{}"); status != http.StatusServiceUnavailable || len(upstream.seen()) != calls {
		t.Fatalf("antigravity quota ran while disabled: %d", status)
	}
}

func claudeFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f, _ := quotaFixture(t)
	account, organization := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	subject := AnthropicSubject(account, organization)
	id, err := f.plugin.inventory.anthropic.register(subject)
	if err != nil {
		t.Fatal(err)
	}
	f.host.addEntry("c", ProviderClaude, map[string]any{
		"type": "claude", "gateway_anthropic_subject": subject, "account_uuid": account, "organization_uuid": organization,
		"email": "c@example.com", "access_token": testAccessToken,
	}, f.clock)
	return f, id
}

func TestAnthropicQuotaFacadeProjectsPassiveSignals(t *testing.T) {
	f, id := claudeFixture(t)
	observed := f.clock.Add(-time.Minute)
	observation := credentialObservation{
		Quota: signalSnapshot{ObservedAt: observed.Add(-time.Hour), Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
		}},
		ModelQuotas: map[string]signalSnapshot{"claude-opus-5": {ObservedAt: observed, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status":      "allowed_warning",
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.825",
			"Anthropic-Ratelimit-Unified-5h-Reset":       "1794200000",
			"Anthropic-Ratelimit-Unified-7d-Status":      "something-new",
			"Anthropic-Ratelimit-Unified-7d-Utilization": "1.5",
			"Anthropic-Ratelimit-Unified-7d-Reset":       "soon",
			"Retry-After":                                "30",
		}}},
		NextRetryAfter: f.clock.Add(10 * time.Minute),
	}
	var seen []Account
	f.plugin.observeCredential = func(_ context.Context, account Account) (credentialObservation, error) {
		seen = append(seen, account)
		return observation, nil
	}
	path := managementPrefix + facadeNamespace + "/anthropic/accounts/quota"
	status, fields, raw := f.manage(http.MethodPost, path, url.Values{"id": {id}}, "{}")
	if status != http.StatusOK || string(fields["status"]) != `"ok"` || string(fields["quota"]) != `[]` ||
		string(fields["observed_at"]) != `"`+observed.Format(time.RFC3339)+`"` ||
		string(fields["cooldown_until"]) != `"`+f.clock.Add(10*time.Minute).Format(time.RFC3339)+`"` ||
		string(fields["windows"]) != `[{"window":"five_hour","used_percent":82.5,"reset_at":"2026-11-09T04:53:20Z","status":"allowed_warning"}]` {
		t.Fatalf("anthropic quota: %d %s", status, raw)
	}
	if len(seen) != 1 || seen[0].ID != id || seen[0].AuthIndex != "idx-c" {
		t.Fatalf("observed the wrong credential: %+v", seen)
	}

	// No observation yet: an empty projection without times, not an error.
	observation = credentialObservation{NextRetryAfter: f.clock.Add(-time.Minute)}
	status, fields, raw = f.manage(http.MethodPost, path, url.Values{"id": {id}}, "{}")
	if status != http.StatusOK || string(fields["windows"]) != `[]` || fields["observed_at"] != nil || fields["cooldown_until"] != nil {
		t.Fatalf("empty observation: %d %s", status, raw)
	}

	f.plugin.observeCredential = func(context.Context, Account) (credentialObservation, error) {
		return credentialObservation{}, ErrUnavailable
	}
	if status, fields, _ = f.manage(http.MethodPost, path, url.Values{"id": {id}}, "{}"); status != http.StatusServiceUnavailable || string(fields["status"]) != `"error"` {
		t.Fatalf("unavailable observation: %d %v", status, fields)
	}
	if status, _, _ = f.manage(http.MethodPost, path, url.Values{"id": {f.ids["a"]}}, "{}"); status != http.StatusNotFound {
		t.Fatalf("a Codex account answered as Claude: %d", status)
	}
}

func TestLoopbackObserveCredential(t *testing.T) {
	f, id := claudeFixture(t)
	var requests []*http.Request
	answer := `{"observed_at":"2026-11-11T00:00:00Z","files":[` +
		`{"id":"other","auth_index":"idx-other","quota":{"observed_at":"2026-11-10T00:00:00Z","signals":{"Anthropic-Ratelimit-Unified-5h-Status":"rejected"}}},` +
		`{"id":"c","auth_index":"idx-c","email":"c@example.com","id_token":{"sub":"x"},"next_retry_after":"2026-11-11T00:05:00Z",` +
		`"quota":{"observed_at":"2026-11-10T23:59:00Z","signals":{"Anthropic-Ratelimit-Unified-5h-Status":"allowed"}},` +
		`"model_quotas":{"claude-opus-5":{"signals":{}}}}]}`
	status := http.StatusOK
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer cpa.Close()
	f.plugin.cfg.LoopbackURL = cpa.URL
	account, _, _ := f.plugin.inventory.Find(ProviderClaude, id)

	observation, err := f.plugin.loopbackObserveCredential(context.Background(), account)
	if err != nil || observation.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] != "allowed" ||
		!observation.NextRetryAfter.Equal(time.Date(2026, 11, 11, 0, 5, 0, 0, time.UTC)) {
		t.Fatalf("observation %+v, %v", observation, err)
	}
	request := requests[0]
	if request.Method != http.MethodGet || request.URL.Path != "/v0/management/auth-files" || request.URL.RawQuery != "auth_index=idx-c" ||
		request.Header.Get("Authorization") != "Bearer "+testManagementKey {
		t.Fatalf("unexpected loopback request: %s %s %v", request.Method, request.URL, request.Header)
	}
	// Another credential's entry, an error status or a malformed listing is
	// never taken as this account's observation.
	for name, change := range map[string]func(){
		"wrong id":      func() { answer = strings.Replace(answer, `"id":"c"`, `"id":"d"`, 1) },
		"error status":  func() { status = http.StatusInternalServerError },
		"malformed":     func() { status, answer = http.StatusOK, `{"files":"x"}` },
		"no management": func() { f.plugin.cfg.ManagementKey = "" },
	} {
		change()
		if _, err = f.plugin.loopbackObserveCredential(context.Background(), account); err == nil {
			t.Fatalf("%s: an observation was accepted", name)
		}
	}
}

func TestDirectQuotaHTTPUsesOnlyTheGivenProxyAndRefusesRedirects(t *testing.T) {
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A forward proxy receives the absolute target URL.
		proxied = append(proxied, r.Method+" "+r.URL.String()+" "+r.Header.Get("Proxy-Authorization"))
		w.Header().Set("Location", "http://elsewhere.invalid/next")
		w.WriteHeader(http.StatusFound)
	}))
	defer proxy.Close()
	decoy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the environment's proxy was used")
	}))
	defer decoy.Close()
	t.Setenv("HTTP_PROXY", decoy.URL)
	t.Setenv("HTTPS_PROXY", decoy.URL)

	request, _ := http.NewRequest(http.MethodGet, "http://provider.invalid/usage", nil)
	parsed, _ := url.Parse(proxy.URL)
	parsed.User = url.UserPassword("route-a", "secret")
	response, err := directQuotaHTTP(context.Background(), parsed.String(), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound || len(proxied) != 1 ||
		!strings.HasPrefix(proxied[0], "GET http://provider.invalid/usage Basic ") {
		t.Fatalf("redirect followed or proxy bypassed: %d %v", response.StatusCode, proxied)
	}
	for _, invalid := range []string{"", "direct", "ftp://proxy:21", "http://", "http://proxy:1/path", "http://proxy:1?x=1"} {
		if _, err = directQuotaHTTP(context.Background(), invalid, request); !errors.Is(err, errQuotaEgress) {
			t.Fatalf("proxy %q was accepted: %v", invalid, err)
		}
	}
}
