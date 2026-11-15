package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	quotaPushAccount   = "0123456789abcdef"
	quotaPushCodex     = "/internal/upstream-accounts/quota-observed"
	quotaPushAnthropic = "/internal/anthropic-accounts/quota-observed"
)

func quotaPushServer(repository *fakeQuotaGuardRepository) *Server {
	s := &Server{
		config: config.Config{SidecarToken: "sidecar-test-secret"}, mux: http.NewServeMux(),
		quotaGuardRepo: repository, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.routes()
	return s
}

func quotaPushServe(s *Server, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sidecar-test-secret")
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

// codexQuotaPushBody is the report the plugin sends for a Codex account, in
// the exact shape it marshals.
func codexQuotaPushBody(primary, primaryMinutes int, primaryReset, weeklyReset int64, observedAt string) string {
	limit := fmt.Sprintf(`{"limitId":"codex","limitName":null,"primary":{"usedPercent":%d,"windowDurationMins":%d,"resetsAt":%d},`+
		`"secondary":{"usedPercent":55,"windowDurationMins":10080,"resetsAt":%d},"rateLimitReachedType":null}`, primary, primaryMinutes, primaryReset, weeklyReset)
	return `{"account_id":"` + quotaPushAccount + `","quota":{"id":6,"result":{"rateLimits":` + limit +
		`,"rateLimitsByLimitId":{"codex":` + limit + `},"rateLimitResetCredits":null},"observed_at":"` + observedAt + `"}}`
}

func claudeQuotaPushBody(observedAt, windows string) string {
	return `{"account_id":"` + quotaPushAccount + `","observed_at":"` + observedAt + `","windows":` + windows + `}`
}

func TestObservedUpstreamQuotaRecordsCodexReport(t *testing.T) {
	next := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	repository := &fakeQuotaGuardRepository{state: store.UpstreamQuotaGuardState{NextCheckAt: &next}}
	s := quotaPushServer(repository)
	observed := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	primaryReset, weeklyReset := observed.Add(2*time.Hour).Unix(), observed.Add(72*time.Hour).Unix()

	w := quotaPushServe(s, quotaPushCodex, codexQuotaPushBody(30, 300, primaryReset, weeklyReset, observed.Format(time.RFC3339Nano)), nil)
	if w.Code != http.StatusNoContent || len(repository.records) != 1 {
		t.Fatalf("status=%d body=%s records=%d", w.Code, w.Body.String(), len(repository.records))
	}
	got := repository.records[0]
	if got.Provider != store.UpstreamProviderCodex || got.AccountID != quotaPushAccount || got.Automatic || !got.RequireNewer ||
		!got.Observation.ObservedAt.Equal(observed) || got.Observation.FiveHour == nil || got.Observation.FiveHour.UsedPercent != 30 ||
		got.Observation.FiveHour.ResetAt == nil || got.Observation.FiveHour.ResetAt.Unix() != primaryReset ||
		got.Observation.SevenDay == nil || got.Observation.SevenDay.UsedPercent != 55 || got.Observation.SevenDay.ResetAt.Unix() != weeklyReset {
		t.Fatalf("recorded %+v", got)
	}
	scheduler := s.upstreamQuotaGuardScheduler()
	scheduler.mu.Lock()
	entry := scheduler.entries[quotaPushAccount]
	scheduler.mu.Unlock()
	if !entry.next.Equal(next) || entry.running {
		t.Fatalf("the automatic check was not rescheduled: %+v", entry)
	}

	// A limit without the five-hour or weekly window gives the guard nothing.
	w = quotaPushServe(s, quotaPushCodex, codexQuotaPushBody(30, 60, primaryReset, weeklyReset, observed.Format(time.RFC3339Nano)), nil)
	if w.Code != http.StatusNoContent || len(repository.records) != 2 || repository.records[1].Observation.FiveHour != nil {
		t.Fatalf("status=%d records=%+v", w.Code, repository.records)
	}
	w = quotaPushServe(s, quotaPushCodex, strings.Replace(codexQuotaPushBody(30, 60, primaryReset, weeklyReset, observed.Format(time.RFC3339Nano)), `"windowDurationMins":10080`, `"windowDurationMins":20160`, 2), nil)
	if w.Code != http.StatusNoContent || len(repository.records) != 2 {
		t.Fatalf("an observation without guarded windows was recorded: status=%d records=%d", w.Code, len(repository.records))
	}
}

func TestObservedUpstreamQuotaRecordsClaudeReport(t *testing.T) {
	repository := &fakeQuotaGuardRepository{}
	s := quotaPushServer(repository)
	observed := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	reset := observed.Add(2 * time.Hour).Truncate(time.Second)
	windows := `[{"window":"five_hour","used_percent":42.4,"reset_at":"` + reset.Format(time.RFC3339) + `","status":"allowed_warning"},` +
		`{"window":"seven_day","used_percent":10,"status":"allowed"}]`
	w := quotaPushServe(s, quotaPushAnthropic, claudeQuotaPushBody(observed.Format(time.RFC3339Nano), windows), nil)
	if w.Code != http.StatusNoContent || len(repository.records) != 1 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := repository.records[0]
	if got.Provider != store.UpstreamProviderAnthropic || got.AccountID != quotaPushAccount || got.Automatic || !got.RequireNewer ||
		!got.Observation.ObservedAt.Equal(observed) || got.Observation.FiveHour == nil || got.Observation.FiveHour.UsedPercent != 42 ||
		!got.Observation.FiveHour.ResetAt.Equal(reset) || got.Observation.SevenDay == nil || got.Observation.SevenDay.UsedPercent != 10 {
		t.Fatalf("recorded %+v", got)
	}
	// Windows without a used percentage give the guard nothing.
	w = quotaPushServe(s, quotaPushAnthropic, claudeQuotaPushBody(observed.Format(time.RFC3339Nano), `[{"window":"seven_day","status":"allowed"}]`), nil)
	if w.Code != http.StatusNoContent || len(repository.records) != 1 {
		t.Fatalf("status=%d records=%d", w.Code, len(repository.records))
	}
}

func TestObservedUpstreamQuotaRejectsInvalidReports(t *testing.T) {
	repository := &fakeQuotaGuardRepository{}
	s := quotaPushServer(repository)
	now := time.Now().UTC()
	observed := now.Add(-time.Second).Format(time.RFC3339Nano)
	reset, week := now.Add(2*time.Hour).Unix(), now.Add(72*time.Hour).Unix()
	codex := codexQuotaPushBody(30, 300, reset, week, observed)
	claudeWindows := `[{"window":"five_hour","used_percent":42,"status":"allowed"}]`
	claude := claudeQuotaPushBody(observed, claudeWindows)

	for _, authorization := range [][]string{nil, {"Bearer wrong"}, {"Bearer sidecar-test-secret", "Bearer sidecar-test-secret"}} {
		w := quotaPushServe(s, quotaPushCodex, codex, func(r *http.Request) { r.Header["Authorization"] = authorization })
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_sidecar_token") {
			t.Fatalf("authorization %v: status=%d", authorization, w.Code)
		}
	}
	for name, test := range map[string]struct {
		path, body string
		edit       func(*http.Request)
	}{
		"empty":             {quotaPushCodex, ``, nil},
		"empty object":      {quotaPushCodex, `{}`, nil},
		"two values":        {quotaPushCodex, codex + `{}`, nil},
		"unknown field":     {quotaPushCodex, strings.Replace(codex, `{"account_id"`, `{"extra":1,"account_id"`, 1), nil},
		"duplicate key":     {quotaPushCodex, strings.Replace(codex, `{"account_id":"`+quotaPushAccount+`"`, `{"account_id":"`+quotaPushAccount+`","account_id":"`+quotaPushAccount+`"`, 1), nil},
		"bad account":       {quotaPushCodex, strings.Replace(codex, quotaPushAccount, "0123456789ABCDEF", 1), nil},
		"no observed time":  {quotaPushCodex, strings.Replace(codex, `,"observed_at":"`+observed+`"`, ``, 1), nil},
		"future observed":   {quotaPushCodex, codexQuotaPushBody(30, 300, reset, week, now.Add(10*time.Minute).Format(time.RFC3339)), nil},
		"ancient observed":  {quotaPushCodex, codexQuotaPushBody(30, 300, reset, week, "1999-12-31T23:59:59Z"), nil},
		"wrong envelope id": {quotaPushCodex, strings.Replace(codex, `"id":6`, `"id":7`, 1), nil},
		"percent too high":  {quotaPushCodex, codexQuotaPushBody(130, 300, reset, week, observed), nil},
		"claude to codex":   {quotaPushCodex, claude, nil},
		"codex to claude":   {quotaPushAnthropic, codex, nil},
		"claude duplicate window": {quotaPushAnthropic, claudeQuotaPushBody(observed,
			`[{"window":"five_hour","used_percent":42,"status":"allowed"},{"window":"five_hour","used_percent":1,"status":"allowed"}]`), nil},
		"claude unknown window":  {quotaPushAnthropic, claudeQuotaPushBody(observed, `[{"window":"one_hour","used_percent":42,"status":"allowed"}]`), nil},
		"claude unknown status":  {quotaPushAnthropic, claudeQuotaPushBody(observed, `[{"window":"five_hour","used_percent":42,"status":"maybe"}]`), nil},
		"claude percent":         {quotaPushAnthropic, claudeQuotaPushBody(observed, `[{"window":"five_hour","used_percent":101,"status":"allowed"}]`), nil},
		"claude future observed": {quotaPushAnthropic, claudeQuotaPushBody(now.Add(10*time.Minute).Format(time.RFC3339), claudeWindows), nil},
		"claude no windows":      {quotaPushAnthropic, `{"account_id":"` + quotaPushAccount + `","observed_at":"` + observed + `"}`, nil},
		"query string":           {quotaPushCodex + "?x=1", codex, nil},
		"not json":               {quotaPushCodex, codex, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		"too large":              {quotaPushCodex, codex + strings.Repeat(" ", upstreamQuotaPushRequestBytes), nil},
	} {
		w := quotaPushServe(s, test.path, test.body, test.edit)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_upstream_quota_report") {
			t.Fatalf("%s: status=%d body=%s", name, w.Code, w.Body.String())
		}
	}
	// Antigravity has no quota headers and no route.
	if w := quotaPushServe(s, "/internal/antigravity-accounts/quota-observed", codex, nil); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("antigravity status=%d", w.Code)
	}
	if len(repository.records) != 0 {
		t.Fatalf("an invalid report was recorded: %+v", repository.records)
	}
}

func TestObservedUpstreamQuotaStoreErrors(t *testing.T) {
	observed := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	body := codexQuotaPushBody(30, 300, time.Now().Add(time.Hour).Unix(), time.Now().Add(72*time.Hour).Unix(), observed)
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrNotFound, http.StatusNotFound, "upstream_account_not_found"},
		{store.ErrInvalid, http.StatusBadRequest, "invalid_upstream_quota_report"},
		{errors.New("database down"), http.StatusInternalServerError, "internal_error"},
	} {
		s := quotaPushServer(&fakeQuotaGuardRepository{recordErr: test.err})
		w := quotaPushServe(s, quotaPushCodex, body, nil)
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) || strings.Contains(w.Body.String(), "database down") {
			t.Fatalf("%v: status=%d body=%s", test.err, w.Code, w.Body.String())
		}
		s.upstreamQuotaGuardScheduler().mu.Lock()
		_, rescheduled := s.upstreamQuotaGuardScheduler().entries[quotaPushAccount]
		s.upstreamQuotaGuardScheduler().mu.Unlock()
		if rescheduled {
			t.Fatalf("%v: a failed record rescheduled the check", test.err)
		}
	}
	s := quotaPushServer(nil)
	s.quotaGuardRepo = nil
	if w := quotaPushServe(s, quotaPushCodex, body, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a guard repository status=%d", w.Code)
	}
}

// TestObservedUpstreamQuotaAcceptsPluginGolden feeds the plugin's own pinned
// reports to the handler, so the two modules cannot drift apart unnoticed.
func TestObservedUpstreamQuotaAcceptsPluginGolden(t *testing.T) {
	observed := time.Date(2026, 11, 9, 2, 6, 40, 4000, time.UTC)
	for _, test := range []struct {
		path, golden, provider, account string
		five, seven                     int
		fiveReset                       int64
	}{
		{quotaPushCodex, "quota_push_codex.json", store.UpstreamProviderCodex, "0123456789abcdef", 25, 40, 1794200000},
		{quotaPushAnthropic, "quota_push_claude.json", store.UpstreamProviderAnthropic, "0c1a0de000000001", 42, 10, 1794200000},
	} {
		body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "codex-compat", "plugin", "internal", "gatewayplugin", "testdata", test.golden))
		if err != nil {
			t.Fatal(err)
		}
		repository := &fakeQuotaGuardRepository{}
		w := quotaPushServe(quotaPushServer(repository), test.path, string(body), nil)
		if w.Code != http.StatusNoContent || len(repository.records) != 1 {
			t.Fatalf("%s: the plugin's report was rejected: status=%d body=%s", test.golden, w.Code, w.Body.String())
		}
		got := repository.records[0]
		if got.Provider != test.provider || got.AccountID != test.account || !got.Observation.ObservedAt.Equal(observed) ||
			got.Observation.FiveHour == nil || got.Observation.FiveHour.UsedPercent != test.five ||
			got.Observation.FiveHour.ResetAt == nil || got.Observation.FiveHour.ResetAt.Unix() != test.fiveReset ||
			got.Observation.SevenDay == nil || got.Observation.SevenDay.UsedPercent != test.seven {
			t.Fatalf("%s: recorded %+v", test.golden, got)
		}
	}
}
