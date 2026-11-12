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
	"testing"
	"time"
)

const (
	testAccessToken = "synthetic-access-token"
	// testObservedUnix is when CPA received the observed Codex response.
	testObservedUnix = 1794190000
)

// codexSignals is what CPA records from one Codex response: the default
// limit's windows, a namespaced extra limit, and headers the projection must
// ignore (plan, credits, limit display name, Retry-After).
func codexSignals() map[string]string {
	return map[string]string{
		"X-Codex-Primary-Used-Percent":               "25",
		"X-Codex-Primary-Window-Minutes":             "300",
		"X-Codex-Primary-Reset-At":                   "1794200000",
		"X-Codex-Secondary-Used-Percent":             "40",
		"X-Codex-Secondary-Window-Minutes":           "10080",
		"X-Codex-Secondary-Reset-After-Seconds":      "510000",
		"X-Codex-Limit-Reached":                      "true",
		"X-Codex-Code-Review-Primary-Used-Percent":   "0",
		"X-Codex-Code-Review-Primary-Window-Minutes": "10080",
		"X-Codex-Code-Review-Primary-Reset-At":       "1794700000",
		"X-Codex-Code-Review-Limit-Name":             "Code review",
		"X-Codex-Plan-Type":                          "plus",
		"X-Codex-Credits-Has-Credits":                "false",
		"Retry-After":                                "30",
	}
}

// quotaFixture builds a plugin with one Codex account whose credential holds
// an access token, and a scripted CPA observation of that credential.
func quotaFixture(t *testing.T) (*fixture, *credentialObservation) {
	t.Helper()
	f := newFixture(t, 1)
	f.host.setDocField("idx-a", "access_token", testAccessToken)
	observation := &credentialObservation{Quota: signalSnapshot{ObservedAt: time.Unix(testObservedUnix, 0).UTC(), Signals: codexSignals()}}
	f.plugin.observeCredential = func(_ context.Context, account Account) (credentialObservation, error) {
		if account.AuthIndex == "" {
			return credentialObservation{}, ErrUnavailable
		}
		return *observation, nil
	}
	return f, observation
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

func TestNormalizeCodexSignalsMatchesGolden(t *testing.T) {
	observed := time.Unix(testObservedUnix, 0).UTC()
	result, err := normalizeCodexSignals(codexSignals(), observed)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.MarshalIndent(quotaRPCResponse{ID: quotaRPCID, Result: result, ObservedAt: observed.Format(time.RFC3339)}, "", "  ")
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
	if strings.Contains(string(got), "plus") || strings.Contains(string(got), "Code review") {
		t.Fatalf("plan or display name leaked: %s", got)
	}
}

func TestNormalizeCodexSignalsShapes(t *testing.T) {
	observed := time.Unix(testObservedUnix, 0).UTC()
	window := func(fields map[string]string) map[string]string {
		out := map[string]string{}
		for name, value := range fields {
			out["X-Codex-Primary-"+name] = value
		}
		return out
	}
	for name, test := range map[string]struct {
		signals map[string]string
		code    string
	}{
		"percent only":           {window(map[string]string{"Used-Percent": "100"}), ""},
		"reached flag only":      {map[string]string{"X-Codex-Allowed": "false"}, ""},
		"only an extra limit":    {map[string]string{"X-Codex-Bengalfox-Primary-Used-Percent": "5"}, ""},
		"websocket extra limit":  {map[string]string{"X-Codex-Additional-Gpt-5-Codex-Spark-Primary-Used-Percent": "5"}, ""},
		"lowercase names":        {map[string]string{"x-codex-primary-used-percent": "5"}, ""},
		"nothing recorded":       {map[string]string{}, "not observed"},
		"only plan and credits":  {map[string]string{"X-Codex-Plan-Type": "plus", "X-Codex-Credits-Balance": "3"}, "not observed"},
		"window without percent": {window(map[string]string{"Window-Minutes": "300"}), "not observed"},
		"percent above range":    {window(map[string]string{"Used-Percent": "101"}), "quota_percent_out_of_range"},
		"percent below range":    {window(map[string]string{"Used-Percent": "-1"}), "quota_percent_out_of_range"},
		"percent not a number":   {window(map[string]string{"Used-Percent": "NaN"}), "quota_percent_out_of_range"},
		"percent as text":        {window(map[string]string{"Used-Percent": "most"}), "quota_field_type_invalid"},
		"window as text":         {window(map[string]string{"Used-Percent": "1", "Window-Minutes": "5h"}), "quota_field_type_invalid"},
		"window zero":            {window(map[string]string{"Used-Percent": "1", "Window-Minutes": "0"}), "quota_window_invalid"},
		"window too long":        {window(map[string]string{"Used-Percent": "1", "Window-Minutes": "44641"}), "quota_window_invalid"},
		"reset too early":        {window(map[string]string{"Used-Percent": "1", "Reset-At": "946684799"}), "quota_reset_invalid"},
		"reset too late":         {window(map[string]string{"Used-Percent": "1", "Reset-At": "4102444801"}), "quota_reset_invalid"},
		"reset as text":          {window(map[string]string{"Used-Percent": "1", "Reset-At": "soon"}), "quota_field_type_invalid"},
		"negative reset after":   {window(map[string]string{"Used-Percent": "1", "Reset-After-Seconds": "-1"}), "quota_reset_invalid"},
		"extra limit invalid":    {map[string]string{"X-Codex-Bengalfox-Primary-Used-Percent": "500"}, "quota_percent_out_of_range"},
	} {
		_, err := normalizeCodexSignals(test.signals, observed)
		switch {
		case test.code == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case test.code == "not observed" && !errors.Is(err, errQuotaNotObserved):
			t.Errorf("%s: got %v, want not observed", name, err)
		case test.code != "" && test.code != "not observed" && quotaSchemaCode(err) != test.code:
			t.Errorf("%s: got %v, want %q", name, err, test.code)
		}
	}

	// A fractional percentage is rounded up; a relative reset is anchored to
	// the observation; the reached flag maps to the generic reached type.
	result, err := normalizeCodexSignals(window(map[string]string{"Used-Percent": "12.1", "Reset-After-Seconds": "60"}), observed)
	if err != nil || result.RateLimits.Primary.UsedPercent != 13 || *result.RateLimits.Primary.ResetsAt != testObservedUnix+60 ||
		result.RateLimits.RateLimitReachedType != nil {
		t.Fatalf("rounding and relative reset: %+v, %v", result.RateLimits.Primary, err)
	}
	result, _ = normalizeCodexSignals(map[string]string{"X-Codex-Allowed": "false"}, observed)
	if result.RateLimits.RateLimitReachedType == nil || *result.RateLimits.RateLimitReachedType != "rate_limit_reached" {
		t.Fatalf("reached flag: %+v", result.RateLimits)
	}
	// Extra limits: both namespaces map to the same id; a name that cannot be
	// an id, or that would shadow the default limit, is left out.
	result, err = normalizeCodexSignals(map[string]string{
		"X-Codex-Primary-Used-Percent":                                   "1",
		"X-Codex-Additional-Code-Review-Primary-Used-Percent":            "2",
		"X-Codex-" + strings.Repeat("Long-", 8) + "Primary-Used-Percent": "3",
		"X-Codex-Codex-Primary-Used-Percent":                             "4",
		"X-Codex-9lives-Primary-Used-Percent":                            "5",
	}, observed)
	if err != nil || len(result.RateLimitsByLimitID) != 2 || result.RateLimitsByLimitID["code_review"].Primary.UsedPercent != 2 ||
		result.RateLimitsByLimitID[codexPrimaryLimitID].Primary.UsedPercent != 1 {
		t.Fatalf("extra limits: %+v, %v", result.RateLimitsByLimitID, err)
	}
	// Only the default limit observed: it is also the rateLimits entry.
	if result.RateLimits.Primary.UsedPercent != 1 {
		t.Fatalf("rateLimits is not the default limit: %+v", result.RateLimits)
	}
	// An extra limit without the default: the default is present and empty.
	result, _ = normalizeCodexSignals(map[string]string{"X-Codex-Bengalfox-Primary-Used-Percent": "5"}, observed)
	if result.RateLimits.LimitID != codexPrimaryLimitID || result.RateLimits.Primary != nil || len(result.RateLimitsByLimitID) != 2 {
		t.Fatalf("default limit placeholder: %+v", result)
	}
	// Extra limits are capped.
	many := map[string]string{}
	for i := 0; i < 40; i++ {
		many["X-Codex-L"+strings.Repeat("x", i%10)+string(rune('a'+i/10))+"-Primary-Used-Percent"] = "1"
	}
	if result, err = normalizeCodexSignals(many, observed); err != nil || len(result.RateLimitsByLimitID) > maxQuotaExtraLimits+2 {
		t.Fatalf("%d limits, %v", len(result.RateLimitsByLimitID), err)
	}
}

func TestCodexQuotaFromObservation(t *testing.T) {
	f, observation := quotaFixture(t)
	query := url.Values{"id": {f.ids["a"]}}
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

	// The newest snapshot with Codex signals wins, the credential's or a
	// model's; a newer snapshot without Codex signals is not one.
	later := time.Unix(testObservedUnix+600, 0).UTC()
	observation.ModelQuotas = map[string]signalSnapshot{
		"gpt-6":   {ObservedAt: later, Signals: map[string]string{"X-Codex-Primary-Used-Percent": "80"}},
		"gpt-6.1": {ObservedAt: later.Add(time.Minute), Signals: map[string]string{"Retry-After": "1"}},
	}
	status, fields, body = f.manage(http.MethodPost, managementPrefix+"upstream-accounts/quota", query, rpcBody)
	if status != http.StatusOK || string(fields["observed_at"]) != `"`+later.Format(time.RFC3339)+`"` ||
		!strings.Contains(body, `"usedPercent":80`) {
		t.Fatalf("latest snapshot: %d %s", status, body)
	}

	// The credential page sees the same snapshot.
	path := managementPrefix + facadeNamespace + "/codex/accounts/quota"
	status, fields, body = f.manage(http.MethodPost, path, query, "{}")
	if status != http.StatusOK || string(fields["status"]) != `"ok"` || string(fields["quota"]) != `[{"model":"codex","remaining_fraction":0.2}]` ||
		string(fields["observed_at"]) != `"`+later.Format(time.RFC3339)+`"` {
		t.Fatalf("facade quota: %d %s", status, body)
	}
	if status, fields, _ = f.manage(http.MethodPost, path, query, `{"x":1}`); status != http.StatusBadRequest || string(fields["status"]) != `"error"` {
		t.Fatalf("facade accepted a body: %d %v", status, fields)
	}
	for _, other := range []string{"/unknown/accounts/quota", "/codex/accounts", "/codex/accounts/quota/x"} {
		if status, _, _ := f.manage(http.MethodPost, managementPrefix+facadeNamespace+other, query, "{}"); status != http.StatusNotFound {
			t.Fatalf("%s answered %d", other, status)
		}
	}

	// Nothing observed yet: a fixed code for the Owner, an empty list on the
	// credential page.
	*observation = credentialObservation{}
	if status, fields, _ = f.manage(http.MethodPost, managementPrefix+"upstream-accounts/quota", query, rpcBody); status != http.StatusConflict || string(fields["error"]) != `"quota_not_observed"` {
		t.Fatalf("no observation: %d %v", status, fields)
	}
	if status, fields, _ = f.manage(http.MethodPost, path, query, "{}"); status != http.StatusOK || string(fields["quota"]) != `[]` {
		t.Fatalf("facade without observation: %d %v", status, fields)
	}
}

func TestCodexQuotaErrors(t *testing.T) {
	for name, test := range map[string]struct {
		setup  func(*fixture, *credentialObservation)
		query  func(*fixture) url.Values
		body   string
		status int
		code   string
	}{
		"invalid request":      {body: `{"method":"account/rateLimits/read","id":7}`, status: 400, code: "quota_request_invalid"},
		"oversized request":    {body: `{"method":"x","pad":"` + strings.Repeat("x", 300) + `"}`, status: 413, code: "quota_request_too_large"},
		"missing id":           {query: func(*fixture) url.Values { return url.Values{} }, status: 404, code: "upstream_account_not_found"},
		"two ids":              {query: func(f *fixture) url.Values { return url.Values{"id": {f.ids["a"], f.ids["a"]}} }, status: 404, code: "upstream_account_not_found"},
		"extra parameter":      {query: func(f *fixture) url.Values { return url.Values{"id": {f.ids["a"]}, "x": {"1"}} }, status: 404, code: "upstream_account_not_found"},
		"unknown account":      {query: func(*fixture) url.Values { return url.Values{"id": {"0123456789abcdef"}} }, status: 404, code: "upstream_account_not_found"},
		"registry unavailable": {setup: func(f *fixture, _ *credentialObservation) { f.host.listErr = errors.New("down") }, status: 503, code: "account_registry_unavailable"},
		"disabled credential": {setup: func(f *fixture, _ *credentialObservation) {
			f.host.entries[0].Disabled = true
		}, status: 409, code: "upstream_account_disabled"},
		"credential replaced by another account": {setup: func(f *fixture, _ *credentialObservation) {
			// The inventory still maps the file to account a; the document now
			// names another ChatGPT account.
			_, _, _ = f.plugin.inventory.Find(ProviderCodex, f.ids["a"])
			f.host.mu.Lock()
			f.host.docs["idx-a"] = strings.Replace(f.host.docs["idx-a"], `"acct-a"`, `"acct-z"`, 1)
			f.host.mu.Unlock()
		}, status: 502, code: "quota_account_identity_unavailable"},
		"observation unavailable": {setup: func(f *fixture, _ *credentialObservation) {
			f.plugin.observeCredential = func(context.Context, Account) (credentialObservation, error) {
				return credentialObservation{}, ErrUnavailable
			}
		}, status: 503, code: "quota_observation_unavailable"},
		"nothing observed": {setup: func(_ *fixture, o *credentialObservation) { *o = credentialObservation{} }, status: 409, code: "quota_not_observed"},
		"observed without codex signals": {setup: func(_ *fixture, o *credentialObservation) {
			o.Quota.Signals = map[string]string{"Retry-After": "30"}
		}, status: 409, code: "quota_not_observed"},
		"observed value out of range": {setup: func(_ *fixture, o *credentialObservation) {
			o.Quota.Signals = map[string]string{"X-Codex-Primary-Used-Percent": "120"}
		}, status: 502, code: "quota_percent_out_of_range"},
	} {
		t.Run(name, func(t *testing.T) {
			f, observation := quotaFixture(t)
			if test.setup != nil {
				test.setup(f, observation)
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
			if strings.Contains(raw, testAccessToken) {
				t.Fatalf("credential text leaked: %s", raw)
			}
		})
	}
}

func TestQuotaRoutesOnlyForCodexNamespace(t *testing.T) {
	f, _ := quotaFixture(t)
	for _, namespace := range []string{"antigravity-accounts", "anthropic-accounts"} {
		if status, _, _ := f.manage(http.MethodPost, managementPrefix+namespace+"/quota", url.Values{"id": {f.ids["a"]}}, rpcBody); status != http.StatusNotFound {
			t.Fatalf("%s/quota answered %d", namespace, status)
		}
	}
	if status, _, _ := f.manage(http.MethodGet, managementPrefix+"upstream-accounts/quota", url.Values{"id": {f.ids["a"]}}, ""); status != http.StatusNotFound {
		t.Fatalf("GET quota answered %d", status)
	}
}

func TestAntigravityQuotaIsUnavailable(t *testing.T) {
	f, _ := quotaFixture(t)
	const subject = "109876543210"
	id, err := f.plugin.inventory.antigravity.register(subject)
	if err != nil {
		t.Fatal(err)
	}
	f.host.addEntry("g", ProviderAntigravity, map[string]any{
		"type": "antigravity", "google_subject": subject, "email": "g@example.com",
		"access_token": testAccessToken, "project_id": "project-1", "refresh_token": "secret",
	}, f.clock)
	path := managementPrefix + facadeNamespace + "/antigravity/accounts/quota"
	status, fields, raw := f.manage(http.MethodPost, path, url.Values{"id": {id}}, "{}")
	if status != http.StatusOK || string(fields["status"]) != `"unavailable"` || string(fields["quota"]) != `[]` || len(fields) != 2 {
		t.Fatalf("antigravity quota: %d %s", status, raw)
	}
	if status, _, _ = f.manage(http.MethodPost, path, url.Values{"id": {"0123456789abcdef"}}, "{}"); status != http.StatusNotFound {
		t.Fatalf("unknown account answered %d", status)
	}
	if status, _, _ = f.manage(http.MethodPost, path, url.Values{"id": {id}}, `{"x":1}`); status != http.StatusBadRequest {
		t.Fatalf("a body was accepted: %d", status)
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
