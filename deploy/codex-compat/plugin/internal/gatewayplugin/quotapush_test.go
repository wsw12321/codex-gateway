package gatewayplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testClaudeAccount = "0c1a0de000000001"

func codexHeaders(primary, secondary string) http.Header {
	return http.Header{
		"X-Codex-Primary-Used-Percent":        {primary},
		"X-Codex-Primary-Window-Minutes":      {"300"},
		"X-Codex-Primary-Reset-After-Seconds": {"3600"},
		"X-Codex-Secondary-Used-Percent":      {secondary},
		"X-Codex-Secondary-Window-Minutes":    {"10080"},
		"X-Codex-Secondary-Reset-At":          {"1794800000"},
		"Content-Type":                        {"text/event-stream"},
	}
}

// usage delivers one usage record through the host RPC, as CPA does.
func (f *fixture) usage(record pluginapi.UsageRecord) {
	f.t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		f.t.Fatal(err)
	}
	done := make(chan []byte, 1)
	go func() { done <- f.plugin.Handle(pluginabi.MethodUsageHandle, raw) }()
	select {
	case out := <-done:
		if !strings.Contains(string(out), `"ok":true`) {
			f.t.Fatalf("usage answer %s", out)
		}
	case <-time.After(2 * time.Second):
		f.t.Fatal("the usage handler blocked")
	}
}

// settle waits until no quota report is waiting or being delivered.
func (f *fixture) settle() {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f.plugin.quotaPush.idle() {
		if time.Now().After(deadline) {
			f.t.Fatal("quota delivery did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

// selectAccount installs an allocation for selection, as a pick does.
func (f *fixture) selectAccount(provider, account, selection string) {
	f.plugin.requests.replace("token-"+selection, &attempt{provider: provider, account: account, selectionID: selection})
}

func TestQuotaSignalsFromHeaders(t *testing.T) {
	headers := http.Header{
		"x-codex-primary-used-percent":          {"10", " 20 "},
		"X-Codex-Secondary-Used-Percent":        {"bad\nvalue"},
		"X-Codex-Plan-Type":                     {strings.Repeat("p", maxQuotaSignalValue+1)},
		"X-Codex-Primary-Window-Minutes":        {""},
		"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"},
		"Authorization":                         {"Bearer secret"},
	}
	codex := quotaSignalsFromHeaders(ProviderCodex, headers)
	if len(codex) != 1 || codex["X-Codex-Primary-Used-Percent"] != "20" {
		t.Fatalf("codex signals = %v", codex)
	}
	claude := quotaSignalsFromHeaders(ProviderClaude, headers)
	if len(claude) != 1 || claude["Anthropic-Ratelimit-Unified-5h-Status"] != "allowed" {
		t.Fatalf("claude signals = %v", claude)
	}
	if got := quotaSignalsFromHeaders(ProviderAntigravity, headers); got != nil {
		t.Fatalf("antigravity signals = %v", got)
	}

	// Truncation keeps the default limit's windows.
	many := http.Header{"X-Codex-Primary-Used-Percent": {"5"}, "X-Codex-Secondary-Used-Percent": {"6"}}
	for i := 0; i < 2*maxQuotaSignalHeaders; i++ {
		many.Set("X-Codex-Additional-L"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+"-Primary-Used-Percent", "1")
		many.Set("X-Codex-Aa"+string(rune('a'+i/26))+string(rune('a'+i%26))+"-Primary-Used-Percent", "1")
	}
	kept := quotaSignalsFromHeaders(ProviderCodex, many)
	if len(kept) != maxQuotaSignalHeaders || kept["X-Codex-Primary-Used-Percent"] != "5" || kept["X-Codex-Secondary-Used-Percent"] != "6" {
		t.Fatalf("truncated to %d signals, default windows %q %q", len(kept), kept["X-Codex-Primary-Used-Percent"], kept["X-Codex-Secondary-Used-Percent"])
	}
}

func TestQuotaPushCodexOnlyOnChange(t *testing.T) {
	f := newFixture(t, 1)
	request := f.request(pickOptions{selection: "sel-q"})
	if _, err := f.plugin.Pick(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-q", Provider: ProviderCodex, ResponseHeaders: codexHeaders("30", "55")})
	f.settle()
	calls := f.alloc.quotaCalls()
	if len(calls) != 1 || calls[0].provider != ProviderCodex || calls[0].report.AccountID != f.ids["a"] || calls[0].report.Codex == nil {
		t.Fatalf("quota calls = %+v", calls)
	}
	envelope := calls[0].report.Codex
	primary, secondary := envelope.Result.RateLimits.Primary, envelope.Result.RateLimits.Secondary
	if envelope.ID != quotaRPCID || primary == nil || primary.UsedPercent != 30 || *primary.WindowDurationMins != 300 ||
		*primary.ResetsAt != f.clock.Unix()+3600 || secondary == nil || secondary.UsedPercent != 55 ||
		envelope.ObservedAt != f.clock.Format(time.RFC3339Nano) {
		raw, _ := json.Marshal(envelope)
		t.Fatalf("envelope %s", raw)
	}

	// The same usage later: the reset derived from Reset-After-Seconds moves
	// with the clock but names the same period.
	f.advance(30 * time.Second)
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-q", ResponseHeaders: codexHeaders("30", "55")})
	f.settle()
	if calls = f.alloc.quotaCalls(); len(calls) != 1 {
		t.Fatalf("an unchanged quota was delivered again: %d calls", len(calls))
	}

	// One more percent is news, also on a failed attempt.
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-q", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 500}, ResponseHeaders: codexHeaders("31", "55")})
	f.settle()
	if calls = f.alloc.quotaCalls(); len(calls) != 2 || calls[1].report.Codex.Result.RateLimits.Primary.UsedPercent != 31 {
		t.Fatalf("a changed quota was not delivered: %+v", calls)
	}

	// A new period is news even at the same usage.
	f.advance(2 * time.Hour)
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-q", ResponseHeaders: codexHeaders("31", "55")})
	f.settle()
	if calls = f.alloc.quotaCalls(); len(calls) != 3 {
		t.Fatalf("a new window period was not delivered: %d calls", len(calls))
	}
}

func TestQuotaPushClaude(t *testing.T) {
	f := newFixture(t, 1)
	f.selectAccount(ProviderClaude, testClaudeAccount, "sel-c")
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-c", ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
		"Anthropic-Ratelimit-Unified-5h-Status":      {"allowed_warning"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1794200000"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.1"},
		"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
	}})
	f.settle()
	calls := f.alloc.quotaCalls()
	if len(calls) != 1 || calls[0].provider != ProviderClaude || calls[0].report.AccountID != testClaudeAccount ||
		calls[0].report.Codex != nil || calls[0].report.ObservedAt != f.clock.Format(time.RFC3339Nano) {
		t.Fatalf("quota calls = %+v", calls)
	}
	raw, _ := json.Marshal(calls[0].report.Windows)
	if string(raw) != `[{"window":"five_hour","used_percent":42,"reset_at":"2026-11-09T04:53:20Z","status":"allowed_warning"},{"window":"seven_day","used_percent":10,"status":"allowed"}]` {
		t.Fatalf("windows %s", raw)
	}
	// A finer utilization that rounds to the same percent is not news.
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-c", ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4204"},
		"Anthropic-Ratelimit-Unified-5h-Status":      {"allowed_warning"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1794200000"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.1"},
		"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
	}})
	f.settle()
	if calls = f.alloc.quotaCalls(); len(calls) != 1 {
		t.Fatalf("an unchanged Claude quota was delivered again: %d calls", len(calls))
	}
}

func TestQuotaPushIgnoresRecordsWithoutQuota(t *testing.T) {
	f := newFixture(t, 1)
	f.selectAccount(ProviderAntigravity, "0a0a0a0a0a0a0a0a", "sel-g")
	f.selectAccount(ProviderCodex, f.ids["a"], "sel-a")
	invalid := codexHeaders("120", "55")
	for name, record := range map[string]pluginapi.UsageRecord{
		"antigravity":       {SelectionID: "sel-g", ResponseHeaders: codexHeaders("30", "55")},
		"unknown selection": {SelectionID: "sel-unknown", ResponseHeaders: codexHeaders("30", "55")},
		"no selection":      {ResponseHeaders: codexHeaders("30", "55")},
		"no headers":        {SelectionID: "sel-a"},
		"no quota headers":  {SelectionID: "sel-a", ResponseHeaders: http.Header{"Content-Type": {"application/json"}}},
		"invalid snapshot":  {SelectionID: "sel-a", ResponseHeaders: invalid},
		// The record names a provider and credential; only the plugin's own
		// allocation decides the account.
		"record provider": {SelectionID: "sel-g", Provider: ProviderCodex, AuthID: "a", ResponseHeaders: codexHeaders("30", "55")},
	} {
		f.usage(record)
		f.settle()
		if f.alloc.attempts() != 0 {
			t.Fatalf("%s: a quota report was delivered", name)
		}
	}
}

func TestQuotaPushBacksOffAfterAFailedDelivery(t *testing.T) {
	f := newFixture(t, 1)
	f.selectAccount(ProviderCodex, f.ids["a"], "sel-a")
	f.alloc.quotaErr = errors.New("gateway down")
	record := pluginapi.UsageRecord{SelectionID: "sel-a", ResponseHeaders: codexHeaders("30", "55")}
	f.usage(record)
	f.settle()
	if f.alloc.attempts() != 1 {
		t.Fatalf("attempts = %d", f.alloc.attempts())
	}
	// Within the backoff nothing is sent, changed or not.
	f.alloc.mu.Lock()
	f.alloc.quotaErr = nil
	f.alloc.mu.Unlock()
	f.advance(30 * time.Second)
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-a", ResponseHeaders: codexHeaders("35", "55")})
	f.settle()
	if f.alloc.attempts() != 1 {
		t.Fatalf("a report was sent during the backoff: %d attempts", f.alloc.attempts())
	}
	// After it, the unchanged quota is sent again: the Gateway never had it.
	f.advance(31 * time.Second)
	f.usage(record)
	f.settle()
	if calls := f.alloc.quotaCalls(); len(calls) != 1 || calls[0].report.Codex.Result.RateLimits.Primary.UsedPercent != 30 {
		t.Fatalf("quota calls after the backoff = %+v", calls)
	}
}

func TestQuotaPushKeepsOnlyTheNewestReportWhileDelivering(t *testing.T) {
	f := newFixture(t, 1)
	f.selectAccount(ProviderCodex, f.ids["a"], "sel-a")
	gate := make(chan struct{})
	f.alloc.quotaGate = gate
	// The handler returns at once although the delivery is held. The same
	// quota again, while it is waiting or being delivered, is not queued.
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-a", ResponseHeaders: codexHeaders("30", "55")})
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-a", ResponseHeaders: codexHeaders("30", "55")})
	deadline := time.Now().Add(5 * time.Second)
	for f.plugin.quotaPush.pendingCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first report was not taken for delivery")
		}
		time.Sleep(time.Millisecond)
	}
	f.usage(pluginapi.UsageRecord{SelectionID: "sel-a", ResponseHeaders: codexHeaders("30", "55")})
	if n := f.plugin.quotaPush.pendingCount(); n != 0 {
		t.Fatalf("a report equal to the one being delivered was queued: %d pending", n)
	}
	for _, used := range []string{"31", "32", "33"} {
		f.usage(pluginapi.UsageRecord{SelectionID: "sel-a", ResponseHeaders: codexHeaders(used, "55")})
	}
	close(gate)
	f.settle()
	calls := f.alloc.quotaCalls()
	if len(calls) != 2 || calls[0].report.Codex.Result.RateLimits.Primary.UsedPercent != 30 ||
		calls[1].report.Codex.Result.RateLimits.Primary.UsedPercent != 33 {
		t.Fatalf("quota calls = %+v", calls)
	}
}

func TestGatewayClientReportQuota(t *testing.T) {
	var path, auth, body string
	status := http.StatusNoContent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		path, auth, body = r.URL.Path, r.Header.Get("Authorization"), string(raw)
		w.WriteHeader(status)
	}))
	defer server.Close()
	client := NewGatewayClient(server.URL, testInternalKey, "egress-allowlist:3129", time.Second)
	ctx := context.Background()
	at := time.Date(2026, 11, 15, 1, 2, 3, 4000, time.UTC)
	codex, _, ok := buildQuotaReport(ProviderCodex, "0123456789abcdef", map[string]string{"X-Codex-Primary-Used-Percent": "30", "X-Codex-Primary-Window-Minutes": "300"}, at)
	if !ok {
		t.Fatal("no codex report")
	}
	if err := client.ReportQuota(ctx, ProviderCodex, codex); err != nil {
		t.Fatal(err)
	}
	want := `{"account_id":"0123456789abcdef","quota":{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":30,"windowDurationMins":300,"resetsAt":null},"secondary":null,"rateLimitReachedType":null},` +
		`"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":30,"windowDurationMins":300,"resetsAt":null},"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null},"observed_at":"2026-11-15T01:02:03.000004Z"}}`
	if path != "/internal/upstream-accounts/quota-observed" || auth != "Bearer "+testInternalKey || body != want {
		t.Fatalf("request %s %q\n%s", path, auth, body)
	}

	claude, _, ok := buildQuotaReport(ProviderClaude, testClaudeAccount, map[string]string{"Anthropic-Ratelimit-Unified-7d-Status": "allowed"}, at)
	if !ok {
		t.Fatal("no claude report")
	}
	if err := client.ReportQuota(ctx, ProviderClaude, claude); err != nil {
		t.Fatal(err)
	}
	if path != "/internal/anthropic-accounts/quota-observed" ||
		body != `{"account_id":"`+testClaudeAccount+`","observed_at":"2026-11-15T01:02:03.000004Z","windows":[{"window":"seven_day","status":"allowed"}]}` {
		t.Fatalf("request %s\n%s", path, body)
	}

	// Only 204 counts as delivered; a Gateway without the route answers 404.
	for _, code := range []int{http.StatusOK, http.StatusNotFound, http.StatusServiceUnavailable} {
		status = code
		if err := client.ReportQuota(ctx, ProviderCodex, codex); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("status %d: %v", code, err)
		}
	}
	status, path = http.StatusNoContent, ""
	for name, call := range map[string]func() error{
		"antigravity":     func() error { return client.ReportQuota(ctx, ProviderAntigravity, codex) },
		"claude as codex": func() error { return client.ReportQuota(ctx, ProviderCodex, claude) },
		"codex as claude": func() error { return client.ReportQuota(ctx, ProviderClaude, codex) },
		"invalid account": func() error {
			return client.ReportQuota(ctx, ProviderCodex, QuotaReport{AccountID: "x", Codex: codex.Codex})
		},
	} {
		if err := call(); !errors.Is(err, ErrUnavailable) || path != "" {
			t.Fatalf("%s: %v, path %q", name, err, path)
		}
	}
}

// TestQuotaPushBodiesMatchGolden pins the exact reports the Gateway receives.
// The Gateway's quota report handler is tested against these same files.
func TestQuotaPushBodiesMatchGolden(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := NewGatewayClient(server.URL, testInternalKey, "egress-allowlist:3129", time.Second)
	observed := time.Unix(testObservedUnix, 4000).UTC()
	codexHeaders := http.Header{}
	for name, value := range codexSignals() {
		codexHeaders.Set(name, value)
	}
	claudeHeaders := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
		"Anthropic-Ratelimit-Unified-5h-Status":      {"allowed_warning"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1794200000"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.1"},
		"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {"1794700000"},
	}
	for _, test := range []struct {
		provider, account, golden string
		headers                   http.Header
	}{
		{ProviderCodex, "0123456789abcdef", "quota_push_codex.json", codexHeaders},
		{ProviderClaude, testClaudeAccount, "quota_push_claude.json", claudeHeaders},
	} {
		report, _, ok := buildQuotaReport(test.provider, test.account, quotaSignalsFromHeaders(test.provider, test.headers), observed)
		if !ok {
			t.Fatalf("%s: no report", test.provider)
		}
		if err := client.ReportQuota(context.Background(), test.provider, report); err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", test.golden)
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(golden, []byte(body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != body+"\n" {
			t.Fatalf("%s report changed:\n%s\nwant:\n%s", test.provider, body, want)
		}
		if strings.Contains(body, "plus") || strings.Contains(body, "Code review") || strings.Contains(body, "Retry") {
			t.Fatalf("%s report carries data the Gateway must not see: %s", test.provider, body)
		}
	}
}

func (q *quotaPusher) pendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}
