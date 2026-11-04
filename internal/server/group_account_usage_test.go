package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const accountUsageQuotaJSON = `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":40,"windowDurationMins":300,"resetsAt":1800000000},"secondary":{"usedPercent":25,"windowDurationMins":10080,"resetsAt":1800600000},"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":40,"windowDurationMins":300,"resetsAt":1800000000},"secondary":{"usedPercent":25,"windowDurationMins":10080,"resetsAt":1800600000},"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`

type fakeGroupAccountUsageRepository struct {
	mu       sync.Mutex
	accounts []store.GroupAccountUsageTarget
	err      error
	userIDs  []string
}

func (f *fakeGroupAccountUsageRepository) ListGroupAccountUsageTargets(_ context.Context, userID string) ([]store.GroupAccountUsageTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userIDs = append(f.userIDs, userID)
	return f.accounts, f.err
}

type countingAccountUsageReader struct {
	mu    sync.Mutex
	reads map[string]int
	read  func(provider, accountID string) (accountUsage, string)
}

func (c *countingAccountUsageReader) reader() accountUsageReader {
	return func(_ context.Context, provider, accountID string) (accountUsage, string) {
		c.mu.Lock()
		if c.reads == nil {
			c.reads = map[string]int{}
		}
		c.reads[accountUsageKey(provider, accountID)]++
		c.mu.Unlock()
		return c.read(provider, accountID)
	}
}

func (c *countingAccountUsageReader) count(provider, accountID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[accountUsageKey(provider, accountID)]
}

func myAccountUsageRequest(t *testing.T, path string, session bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if session {
		addBillingSourceTestSession(t, r)
	}
	return r
}

func TestMyAccountUsageRequiresSessionAndRejectsQueries(t *testing.T) {
	s, _ := newBillingSourceTestServer(t, store.UserRoleMember, nil)
	repo := &fakeGroupAccountUsageRepository{}
	s.groupAccountUsageRepo = repo
	s.accountUsageRead = func(context.Context, string, string) (accountUsage, string) {
		t.Fatal("no account may be read")
		return accountUsage{}, ""
	}
	for _, tc := range []struct {
		name, path, code string
		session          bool
		status           int
	}{
		{name: "unauthenticated", path: "/admin/billing/me/account-usage", code: "session_required", status: http.StatusUnauthorized},
		{name: "query rejected", path: "/admin/billing/me/account-usage?user_id=other", session: true, code: "invalid_account_usage_request", status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, myAccountUsageRequest(t, tc.path, tc.session))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || len(repo.userIDs) != 0 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d body=%s reads=%v", w.Code, w.Body.String(), repo.userIDs)
			}
		})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, myAccountUsageRequest(t, "/admin/billing/me/account-usage", true))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"groups":[]}` || len(repo.userIDs) != 1 || repo.userIDs[0] != "self-1" {
		t.Fatalf("empty account usage: status=%d body=%s users=%v", w.Code, w.Body.String(), repo.userIDs)
	}

	repo.err = errors.New("private database detail")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, myAccountUsageRequest(t, "/admin/billing/me/account-usage", true))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "private database detail") {
		t.Fatalf("store failure: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMyAccountUsageGroupsAccountsAndSharesReads(t *testing.T) {
	const (
		groupA = "11111111-1111-4111-8111-111111111111"
		groupB = "22222222-2222-4222-8222-222222222222"
		codexA = "0123456789abcdef"
		codexB = "1123456789abcdef"
		claude = "aaaabbbbccccdddd"
	)
	s, _ := newBillingSourceTestServer(t, store.UserRoleMember, nil)
	s.groupAccountUsageRepo = &fakeGroupAccountUsageRepository{accounts: []store.GroupAccountUsageTarget{
		{GroupID: groupA, GroupName: "研究组", Provider: store.UpstreamProviderCodex, AccountID: codexA, Alias: "主力"},
		{GroupID: groupA, GroupName: "研究组", Provider: store.UpstreamProviderAnthropic, AccountID: claude},
		{GroupID: groupB, GroupName: "工程组", Provider: store.UpstreamProviderCodex, AccountID: codexB},
		{GroupID: groupB, GroupName: "工程组", Provider: store.UpstreamProviderAnthropic, AccountID: claude},
	}}
	observed := time.Date(2026, 11, 4, 6, 0, 0, 0, time.UTC)
	reads := &countingAccountUsageReader{read: func(provider, accountID string) (accountUsage, string) {
		switch accountID {
		case codexA:
			used := 40
			return accountUsage{Source: "live", UpdatedAt: &observed, FiveHour: &accountUsageWindow{UsedPercent: &used}}, ""
		case claude:
			used := 18
			return accountUsage{Source: "observed", UpdatedAt: &observed, SevenDay: &accountUsageWindow{UsedPercent: &used}}, ""
		default:
			return accountUsage{}, "usage_rate_limited"
		}
	}}
	s.accountUsageRead = reads.reader()

	for attempt := 0; attempt < 2; attempt++ {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, myAccountUsageRequest(t, "/admin/billing/me/account-usage", true))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		for _, id := range []string{codexA, codexB, claude} {
			if strings.Contains(w.Body.String(), id) {
				t.Fatalf("member response exposed account ID %s: %s", id, w.Body.String())
			}
		}
		var response myAccountUsageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Groups) != 2 || response.Groups[0].GroupID != groupA || response.Groups[0].GroupName != "研究组" || response.Groups[1].GroupID != groupB {
			t.Fatalf("groups=%+v", response.Groups)
		}
		first, second := response.Groups[0].Accounts, response.Groups[1].Accounts
		if len(first) != 2 || len(second) != 2 {
			t.Fatalf("accounts=%+v / %+v", first, second)
		}
		if first[0].Provider != store.UpstreamProviderCodex || first[0].Alias != "主力" || first[0].Source != "live" || first[0].FiveHour == nil || *first[0].FiveHour.UsedPercent != 40 || first[0].ErrorCode != "" {
			t.Fatalf("codex account=%+v", first[0])
		}
		if first[1].Provider != store.UpstreamProviderAnthropic || first[1].Alias != "" || first[1].SevenDay == nil || *first[1].SevenDay.UsedPercent != 18 || second[1].SevenDay == nil {
			t.Fatalf("claude account=%+v / %+v", first[1], second[1])
		}
		if second[0].ErrorCode != "usage_rate_limited" || second[0].FiveHour != nil || second[0].Source != "" {
			t.Fatalf("failed account=%+v", second[0])
		}
	}
	// One read per distinct account; the second request is served from cache,
	// failures included, so refreshing cannot hammer an unhealthy upstream.
	for _, key := range [][2]string{{store.UpstreamProviderCodex, codexA}, {store.UpstreamProviderCodex, codexB}, {store.UpstreamProviderAnthropic, claude}} {
		if got := reads.count(key[0], key[1]); got != 1 {
			t.Fatalf("%s/%s reads=%d, want 1", key[0], key[1], got)
		}
	}
}

func TestAccountUsageCacheSharesInFlightReadsAndExpires(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	cache := newAccountUsageCache()
	cache.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	advance := func(d time.Duration) { clock.Lock(); now = now.Add(d); clock.Unlock() }
	release := make(chan struct{})
	var reads int
	var readsMu sync.Mutex
	read := func() (accountUsage, string, time.Duration) {
		readsMu.Lock()
		reads++
		readsMu.Unlock()
		<-release
		return accountUsage{Source: "live"}, "", time.Minute
	}
	first := cache.lookup("codex/a", read)
	second := cache.lookup("codex/a", read)
	if first != second {
		t.Fatal("concurrent lookups must share one in-flight read")
	}
	close(release)
	<-first.ready
	if cache.lookup("codex/a", read) != first {
		t.Fatal("a fresh entry must be reused")
	}
	advance(time.Minute)
	expired := cache.lookup("codex/a", read)
	<-expired.ready
	readsMu.Lock()
	defer readsMu.Unlock()
	if expired == first || reads != 2 || expired.usage.Source != "live" {
		t.Fatalf("expired entry was not re-read: reads=%d", reads)
	}

	cache.store("codex/b", accountUsage{Source: "live", Limited: true}, time.Minute)
	stored := cache.lookup("codex/b", func() (accountUsage, string, time.Duration) {
		t.Error("a stored result must be served without a read")
		return accountUsage{}, "", 0
	})
	<-stored.ready
	if !stored.usage.Limited {
		t.Fatalf("stored usage=%+v", stored.usage)
	}
}

func TestCodexAccountUsageKeepsFiveHourAndWeeklyWindows(t *testing.T) {
	at := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	five, week, other := int64(300), int64(10080), int64(30)
	resets, reached := int64(1800000000), "rate_limit_reached"
	quota := gatewayproxy.UpstreamQuota{Result: gatewayproxy.AccountRateLimitsResult{RateLimits: gatewayproxy.RateLimitSnapshot{
		LimitID:              "codex",
		Primary:              &gatewayproxy.RateLimitWindow{UsedPercent: 100, WindowDurationMins: &five, ResetsAt: &resets},
		Secondary:            &gatewayproxy.RateLimitWindow{UsedPercent: 25, WindowDurationMins: &week},
		RateLimitReachedType: &reached,
	}}}
	usage := codexAccountUsage(quota, at)
	if usage.Source != "live" || usage.UpdatedAt == nil || !usage.UpdatedAt.Equal(at) || !usage.Limited {
		t.Fatalf("usage=%+v", usage)
	}
	if usage.FiveHour == nil || *usage.FiveHour.UsedPercent != 100 || usage.FiveHour.ResetAt == nil || usage.FiveHour.ResetAt.Unix() != resets {
		t.Fatalf("five hour=%+v", usage.FiveHour)
	}
	if usage.SevenDay == nil || *usage.SevenDay.UsedPercent != 25 || usage.SevenDay.ResetAt != nil {
		t.Fatalf("weekly=%+v", usage.SevenDay)
	}

	quota.Result.RateLimits = gatewayproxy.RateLimitSnapshot{Primary: &gatewayproxy.RateLimitWindow{UsedPercent: 5, WindowDurationMins: &other}}
	if usage := codexAccountUsage(quota, at); usage.FiveHour != nil || usage.SevenDay != nil || usage.Limited {
		t.Fatalf("unrecognized window must not be shown: %+v", usage)
	}
}

func TestAnthropicAccountUsageValidatesObservedWindows(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	observed, cooldown, reset := now.Add(-time.Minute), now.Add(time.Hour), now.Add(2*time.Hour)
	used := 42.6
	usage, ok := anthropicAccountUsage(&observed, &cooldown, []cpaObservedWindow{
		{Window: "five_hour", UsedPercent: &used, ResetAt: &reset, Status: "allowed_warning"},
		{Window: "seven_day", Status: "unknown"},
	}, now)
	if !ok || usage.Source != "observed" || !usage.UpdatedAt.Equal(observed) || !usage.Limited || usage.FiveHour == nil || *usage.FiveHour.UsedPercent != 43 ||
		!usage.FiveHour.ResetAt.Equal(reset) || usage.SevenDay == nil || usage.SevenDay.UsedPercent != nil {
		t.Fatalf("usage=%+v ok=%t", usage, ok)
	}
	if usage, ok := anthropicAccountUsage(nil, nil, []cpaObservedWindow{{Window: "seven_day", Status: "rejected"}}, now); !ok || !usage.Limited || usage.UpdatedAt != nil {
		t.Fatalf("rejected window must mark the account limited: %+v", usage)
	}
	tooHigh := 101.0
	for _, windows := range [][]cpaObservedWindow{
		{{Window: "five_hour", Status: "throttled"}},
		{{Window: "one_day", Status: "allowed"}},
		{{Window: "five_hour", Status: "allowed", UsedPercent: &tooHigh}},
	} {
		if _, ok := anthropicAccountUsage(nil, nil, windows, now); ok {
			t.Fatalf("invalid windows accepted: %+v", windows)
		}
	}
}

func accountUsageCodexServer(t *testing.T, handler http.HandlerFunc) (*Server, *upstreamAuditCapture) {
	t.Helper()
	sidecar := httptest.NewServer(handler)
	t.Cleanup(sidecar.Close)
	repository, capture := newUpstreamAuditStore(t)
	baseURL, _ := url.Parse(sidecar.URL)
	return &Server{store: repository, upstream: gatewayproxy.NewWithHTTPClient(baseURL, "sidecar-secret", sidecar.Client()), quotas: newUpstreamQuotaLimiter()}, capture
}

func TestReadCodexAccountUsageAuditsAndMapsErrors(t *testing.T) {
	ctx := context.WithValue(context.Background(), userContextKey, store.User{ID: "member-1", Role: store.UserRoleMember})
	ctx = context.WithValue(ctx, sessionContextKey, store.Session{ID: "session-9", UserID: "member-1"})

	s, capture := accountUsageCodexServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, accountUsageQuotaJSON)
	})
	usage, code := s.readCodexAccountUsage(ctx, "0123456789abcdef")
	if code != "" || usage.FiveHour == nil || *usage.FiveHour.UsedPercent != 40 || usage.SevenDay == nil || *usage.SevenDay.UsedPercent != 25 {
		t.Fatalf("usage=%+v code=%q", usage, code)
	}
	args := capture.snapshot()
	if len(args) != 12 || args[1].Value != "member-1" || args[2].Value != "session-9" || args[4].Value != "upstream_account.quota_queried" || args[6].Value != true || args[9].Value != "0123456789abcdef" {
		t.Fatalf("audit=%#v", args)
	}
	var metadata map[string]any
	if err := json.Unmarshal(args[11].Value.([]byte), &metadata); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, metadata, "duration_ms", "result_code", "http_status", "via")
	if metadata["via"] != "group_account_usage" || metadata["result_code"] != "ok" {
		t.Fatalf("audit metadata=%#v", metadata)
	}
	// The member read shares the Owner's per-account debounce.
	if _, code := s.readCodexAccountUsage(ctx, "0123456789abcdef"); code != "usage_rate_limited" {
		t.Fatalf("debounced read code=%q", code)
	}

	for _, tc := range []struct {
		name, want string
		status     int
		body       string
	}{
		{name: "disabled", status: http.StatusConflict, body: `{"error":"upstream_account_disabled"}`, want: "account_disabled"},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"access_token":"secret"}`, want: "usage_rate_limited"},
		{name: "other", status: http.StatusBadGateway, body: `{"error":"Bearer secret"}`, want: "usage_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := accountUsageCodexServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			if usage, code := s.readCodexAccountUsage(ctx, "0123456789abcdef"); code != tc.want || usage.FiveHour != nil {
				t.Fatalf("usage=%+v code=%q", usage, code)
			}
		})
	}
}

func TestOwnerQuotaQueryRefreshesAccountUsageCache(t *testing.T) {
	s, _ := accountUsageCodexServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, accountUsageQuotaJSON)
	})
	response := httptest.NewRecorder()
	s.upstreamAccountQuota(response, quotaTestRequest("0123456789abcdef"))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	entry := s.accountUsageCache().lookup(accountUsageKey(store.UpstreamProviderCodex, "0123456789abcdef"), func() (accountUsage, string, time.Duration) {
		t.Error("the Owner's result must be reused")
		return accountUsage{}, "", 0
	})
	<-entry.ready
	if entry.usage.FiveHour == nil || *entry.usage.FiveHour.UsedPercent != 40 || entry.usage.SevenDay == nil {
		t.Fatalf("cached usage=%+v", entry.usage)
	}
}

func TestGroupAccountUsageVisibilityRequiresBooleanEnabled(t *testing.T) {
	for _, value := range []any{nil, "true", 1, map[string]any{}} {
		now := time.Now()
		s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
		repo := &fakeGroupRepository{}
		s.groupRepo = repo
		e := groupHandlerWriteEndpoints()[len(groupHandlerWriteEndpoints())-1]
		if value == nil {
			delete(e.body, "enabled")
		} else {
			e.body["enabled"] = value
		}
		r := groupHandlerRequest(t, e)
		addBillingSourceTestSession(t, r)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || repo.calls != 0 {
			t.Fatalf("enabled=%#v status=%d body=%s calls=%d", value, w.Code, w.Body.String(), repo.calls)
		}
	}
	now := time.Now()
	s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
	repo := &fakeGroupRepository{group: store.Group{GroupSummary: store.GroupSummary{ID: groupHandlerTestID}, Members: []store.GroupMember{}}}
	s.groupRepo = repo
	e := groupHandlerWriteEndpoints()[len(groupHandlerWriteEndpoints())-1]
	e.body["enabled"] = false
	r := groupHandlerRequest(t, e)
	addBillingSourceTestSession(t, r)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || repo.usage == nil || repo.usage.Enabled {
		t.Fatalf("disable: status=%d body=%s params=%+v", w.Code, w.Body.String(), repo.usage)
	}
}
