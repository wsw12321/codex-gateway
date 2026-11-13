package gatewayplugin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPickRequiresTrustedIdentity(t *testing.T) {
	f := newFixture(t, 2)
	cases := map[string]pickOptions{
		"missing token":      {noToken: true},
		"missing user":       {noUser: true},
		"missing affinity":   {noAffinity: true},
		"malformed user":     {user: "not-a-uuid"},
		"uppercase user":     {user: "11111111-1111-4111-8111-11111111111A"},
		"malformed token":    {token: "short"},
		"malformed groups":   {groups: `[{"group_id":"x"}]`},
		"duplicate user":     {extraHdr: http.Header{HeaderUser: {testUser, testOtherUser}}},
		"duplicate affinity": {extraHdr: http.Header{HeaderAffinity: {b64(1), b64(2)}}},
	}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.pick(options); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("got %v, want %v", err, ErrUnavailable)
			}
		})
	}
	if len(f.alloc.eligibleCalls) != 0 || len(f.alloc.selectCalls) != 0 {
		t.Fatal("an untrusted request reached the Gateway callbacks")
	}
	if f.active("a")+f.active("b") != 0 {
		t.Fatal("an untrusted request took a slot")
	}
}

func TestPickAllocatesAndReports(t *testing.T) {
	f := newFixture(t, 3)
	f.alloc.prefer = []string{f.ids["b"]}
	name := f.mustPick(pickOptions{session: "codex:s1"})
	if name != "b" {
		t.Fatalf("picked %q, want the Gateway's choice b", name)
	}
	report := f.alloc.lastReport(t)
	if report.AccountID != f.ids["b"] || report.UserID != testUser || report.Rejected != "" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.ConversationHash) != len("conv-")+32 {
		t.Fatalf("conversation hash %q has the wrong shape", report.ConversationHash)
	}
	if f.active("b") != 1 {
		t.Fatalf("active on b = %d, want 1", f.active("b"))
	}
}

func TestPickOnlyOffersAuthorizedAccounts(t *testing.T) {
	f := newFixture(t, 3)
	f.alloc.permitted = map[string]map[string]bool{testUser: {f.ids["c"]: true}}
	if name := f.mustPick(pickOptions{}); name != "c" {
		t.Fatalf("picked %q, want the only authorized account c", name)
	}
	for _, offered := range f.alloc.selectCalls {
		for _, id := range offered {
			if id != f.ids["c"] {
				t.Fatalf("an unauthorized account %s was offered for selection", id)
			}
		}
	}
	// A user with no entitlement at all is refused, never given a default.
	if _, err := f.pick(pickOptions{user: testOtherUser}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want %v", err, ErrUnavailable)
	}
}

func TestPickFailsClosedWhenGatewayUnavailable(t *testing.T) {
	for name, configure := range map[string]func(*fakeAllocator){
		"eligible": func(a *fakeAllocator) { a.eligibleErr = ErrUnavailable },
		"select":   func(a *fakeAllocator) { a.selectErr = ErrUnavailable },
		"report":   func(a *fakeAllocator) { a.reportErr = ErrUnavailable },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 2)
			configure(f.alloc)
			if _, err := f.pick(pickOptions{session: "codex:s1"}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("got %v, want %v", err, ErrUnavailable)
			}
			if f.active("a")+f.active("b") != 0 {
				t.Fatal("a refused request kept a slot")
			}
			f.alloc.eligibleErr, f.alloc.selectErr, f.alloc.reportErr = nil, nil, nil
			// A failed report must not leave a binding behind either.
			f.alloc.prefer = []string{f.ids["b"]}
			if got := f.mustPick(pickOptions{session: "codex:s1"}); got != "b" {
				t.Fatalf("picked %q after recovery, want a fresh Gateway choice b", got)
			}
		})
	}
}

func TestBindingReusesAccountAndRechecksEligibility(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.prefer = []string{f.ids["a"]}
	first, _ := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded)
	f.alloc.prefer = []string{f.ids["b"]}
	selects := len(f.alloc.selectCalls)
	second, _ := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded)
	if first != "a" || second != "a" {
		t.Fatalf("picks = %q, %q; the bound conversation must stay on a", first, second)
	}
	if len(f.alloc.selectCalls) != selects {
		t.Fatal("a bound conversation asked the Gateway for a new selection")
	}
	if len(f.alloc.eligibleCalls) != 2 {
		t.Fatalf("eligibility was checked %d times, want once per pick", len(f.alloc.eligibleCalls))
	}
	// Losing the entitlement unbinds this conversation and reallocates it.
	f.alloc.permitted = map[string]map[string]bool{testUser: {f.ids["b"]: true}}
	third, _ := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded)
	if third != "b" {
		t.Fatalf("picked %q after losing access to a, want b", third)
	}
}

func TestBindingsAreIsolatedPerCaller(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.prefer = []string{f.ids["a"]}
	first := f.mustPick(pickOptions{affinity: 1, session: "codex:shared"})
	f.alloc.prefer = []string{f.ids["b"]}
	second := f.mustPick(pickOptions{affinity: 2, user: testOtherUser, session: "codex:shared"})
	if first != "a" || second != "b" {
		t.Fatalf("picks = %q, %q; a second caller must not inherit the first caller's binding", first, second)
	}
	reports := f.alloc.reports
	if reports[0].ConversationHash == reports[1].ConversationHash {
		t.Fatal("two callers with the same client session id share a conversation hash")
	}
}

func TestSubagentFollowsParentBinding(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.limits[f.ids["a"]], f.alloc.limits[f.ids["b"]] = 1, 1
	f.alloc.prefer = []string{f.ids["b"]}
	parent := f.mustPick(pickOptions{session: "codex:root"})
	f.alloc.prefer = []string{f.ids["a"]}
	child := f.mustPick(pickOptions{session: "codex:child", parent: "codex:root"})
	if parent != "b" || child != "b" {
		t.Fatalf("picks = %q, %q; the subagent must follow its parent", parent, child)
	}
	// Parent and subagent are one root conversation: one slot, one hash.
	if f.active("b") != 1 {
		t.Fatalf("active on b = %d, want one shared root slot", f.active("b"))
	}
	if f.alloc.reports[0].ConversationHash != f.alloc.reports[1].ConversationHash {
		t.Fatal("the subagent reported a different conversation hash from its root")
	}
}

func TestFundingLayersSelectInBillingOrder(t *testing.T) {
	f := newFixture(t, 3)
	groups := groupsHeader(groupLayer(testGroupA, f.ids["a"]), groupLayer(testGroupB, f.ids["b"]), personalLayer)
	if name := f.mustPick(pickOptions{groups: groups}); name != "a" {
		t.Fatalf("picked %q, want the first layer's account a", name)
	}
	if got := f.alloc.selectCalls[0]; len(got) != 1 || got[0] != f.ids["a"] {
		t.Fatalf("first selection offered %v, want only the first layer", got)
	}
	// An exhausted layer falls through to the next one, in order.
	f.alloc.exhausted[f.ids["a"]] = true
	if name := f.mustPick(pickOptions{groups: groups}); name != "b" {
		t.Fatalf("picked %q, want the second layer's account b", name)
	}
	f.alloc.exhausted[f.ids["b"]] = true
	if name := f.mustPick(pickOptions{groups: groups}); name != "c" {
		t.Fatalf("picked %q, want the personal layer's account c", name)
	}
}

func TestGroupScopeRestrictsNewAndBoundAccounts(t *testing.T) {
	f := newFixture(t, 3)
	f.alloc.prefer = []string{f.ids["c"]}
	if name, _ := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded); name != "c" {
		t.Fatalf("picked %q, want c", name)
	}
	// The same conversation, now admitted through a group that excludes c,
	// must leave its binding rather than spend group funds outside the pool.
	groups := groupsHeader(groupLayer(testGroupA, f.ids["a"]))
	if name := f.mustPick(pickOptions{session: "codex:s1", groups: groups}); name != "a" {
		t.Fatalf("picked %q, want the group's account a", name)
	}
	for _, offered := range f.alloc.eligibleCalls[1:] {
		for _, id := range offered {
			if id != f.ids["a"] {
				t.Fatalf("account %s outside the group scope was offered", id)
			}
		}
	}
}

func TestGroupScopeEmptyFailsClosed(t *testing.T) {
	f := newFixture(t, 2)
	for name, groups := range map[string]string{
		"empty list":     `[]`,
		"empty layer":    groupsHeader(groupLayer(testGroupA)),
		"unknown member": groupsHeader(groupLayer(testGroupA, "0123456789abcdef")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.pick(pickOptions{groups: groups}); !errors.Is(err, ErrConcurrency) {
				t.Fatalf("got %v, want %v", err, ErrConcurrency)
			}
		})
	}
	if len(f.alloc.selectCalls) != 0 {
		t.Fatal("an empty scope widened into a selection")
	}
	if report := f.alloc.lastReport(t); report.Rejected != ErrConcurrency.Error() || report.AccountID != "" {
		t.Fatalf("the refusal was not reported: %+v", report)
	}
}

func TestFullAccountsAreSkippedBeforeSelection(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.prefer = []string{f.ids["a"]}
	f.mustPick(pickOptions{session: "codex:s1"})
	// a is full: the next conversation must not even be offered it.
	if name := f.mustPick(pickOptions{session: "codex:s2"}); name != "b" {
		t.Fatalf("picked %q, want the free account b", name)
	}
	last := f.alloc.selectCalls[len(f.alloc.selectCalls)-1]
	if len(last) != 1 || last[0] != f.ids["b"] {
		t.Fatalf("selection offered %v, want only the free account", last)
	}
	if _, err := f.pick(pickOptions{session: "codex:s3"}); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("got %v, want %v when every account is full", err, ErrConcurrency)
	}
}

func TestHeldConversationKeepsItsAccount(t *testing.T) {
	f := newFixture(t, 1)
	if _, err := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded); err != nil {
		t.Fatal(err)
	}
	// Between turns the slot stays with the conversation.
	if f.active("a") != 1 {
		t.Fatalf("active on a = %d, want the hold to occupy the slot", f.active("a"))
	}
	if _, err := f.pick(pickOptions{session: "codex:other"}); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("another conversation took a held slot: %v", err)
	}
	if _, err := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded); err != nil {
		t.Fatalf("the holding conversation could not continue: %v", err)
	}
	f.advance(5*time.Minute + time.Second)
	if _, err := f.pick(pickOptions{session: "codex:other"}); err != nil {
		t.Fatalf("the hold did not expire: %v", err)
	}
}

func TestHoldRequiresConversationAndMovesWithIt(t *testing.T) {
	f := newFixture(t, 2)
	// A request without a session identity never holds a slot.
	if _, err := f.run(pickOptions{only: []string{"a"}}, pluginapi.RequestCompletionSucceeded); err != nil {
		t.Fatal(err)
	}
	if f.active("a") != 0 {
		t.Fatalf("active on a = %d, want no hold without a conversation", f.active("a"))
	}
	f.alloc.prefer = []string{f.ids["a"]}
	f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded)
	// The conversation moves to b: its idle hold on a is released at once.
	f.alloc.permitted = map[string]map[string]bool{testUser: {f.ids["b"]: true}}
	f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded)
	if f.active("a") != 0 || f.active("b") != 1 {
		t.Fatalf("active = a:%d b:%d, want the hold to move to b", f.active("a"), f.active("b"))
	}
}

func TestSlotRaceSpendsNoAttempt(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.prefer = []string{f.ids["a"], f.ids["b"]}
	// While the Gateway is choosing for one conversation, another takes a.
	var once sync.Once
	f.alloc.beforeSelect = func() {
		once.Do(func() {
			if _, ok := f.plugin.concurrency.Acquire(f.ids["a"], "root:rival"); !ok {
				t.Error("the rival could not take the slot")
			}
		})
	}
	name := f.mustPick(pickOptions{session: "codex:s1"})
	if name != "b" {
		t.Fatalf("picked %q, want the request to move on to b", name)
	}
	if got := f.alloc.lastReport(t); got.AccountID != f.ids["b"] {
		t.Fatalf("reported %s, want only the final account", got.AccountID)
	}
	if len(f.alloc.reports) != 1 {
		t.Fatalf("%d attempts reported, want the lost race to be invisible", len(f.alloc.reports))
	}
}

func TestFailoverDropsPreviousAttempt(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.prefer = []string{f.ids["a"]}
	first := f.request(pickOptions{session: "codex:s1"})
	token := http.Header(first.Options.Headers).Get(HeaderRequest)
	f.plugin.requests.begin(token, "req-1", true)
	if name, err := f.plugin.Pick(context.Background(), first); err != nil || name != "a" {
		t.Fatalf("first pick = %q, %v", name, err)
	}
	// CPA excludes the failed credential and asks again for the same request.
	second := f.request(pickOptions{session: "codex:s1", token: token, only: []string{"b"}})
	if name, err := f.plugin.Pick(context.Background(), second); err != nil || name != "b" {
		t.Fatalf("second pick = %q, %v", name, err)
	}
	if f.active("a") != 0 || f.active("b") != 1 {
		t.Fatalf("active = a:%d b:%d, want the failed account released", f.active("a"), f.active("b"))
	}
	f.plugin.Complete("req-1", pluginapi.RequestCompletionSucceeded)
	// The conversation is now bound to b, not to the account that failed.
	if name, _ := f.run(pickOptions{session: "codex:s1"}, pluginapi.RequestCompletionSucceeded); name != "b" {
		t.Fatalf("next turn picked %q, want b", name)
	}
}

func TestBlockedAndDuplicateCredentialsAreNeverSelected(t *testing.T) {
	f := newFixture(t, 2)
	// A second, older credential file for account a.
	f.host.addCodex("a-old", "acct-a", f.clock.Add(-time.Hour))
	f.ids["a-old"] = f.ids["a"]
	f.advance(time.Minute)
	if err := f.plugin.control.SetEnabled(f.ids["b"], false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		name, err := f.run(pickOptions{}, pluginapi.RequestCompletionSucceeded)
		if err != nil || name != "a" {
			t.Fatalf("picked %q, %v; want the canonical credential a", name, err)
		}
	}
	// With only the disabled account and the duplicate offered, nothing routes.
	if _, err := f.pick(pickOptions{only: []string{"b", "a-old"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want %v", err, ErrUnavailable)
	}
}

func TestUnsupportedOrMixedProvidersFailClosed(t *testing.T) {
	f := newFixture(t, 1)
	request := f.request(pickOptions{})
	request.Candidates = append(request.Candidates, pluginapi.SchedulerAuthCandidate{ID: "g", Provider: "gemini"})
	if _, err := f.plugin.Pick(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want %v", err, ErrUnavailable)
	}
	request.Candidates = nil
	if _, err := f.plugin.Pick(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want %v for no candidates", err, ErrUnavailable)
	}
}

func TestUsageLimitLocksAccount(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		locked bool
	}{
		{"quota", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":1}}`, true},
		{"quota bad request", 400, `{"error":{"type":"usage_limit_reached"}}`, true},
		{"top-level type", 429, `{"type":"usage_limit_reached","resets_in_seconds":3600}`, true},
		{"ordinary rate limit", 429, `{"error":{"type":"rate_limit_exceeded"}}`, false},
		{"message only", 429, `{"error":{"message":"usage_limit_reached"}}`, false},
		{"unauthorized", 401, `{"error":{"type":"usage_limit_reached"}}`, false},
		{"forbidden", 403, `{"error":{"type":"usage_limit_reached"}}`, false},
		{"malformed", 429, `{"error":{"type":"usage_limit_reached"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1)
			request := f.request(pickOptions{session: "codex:s1", selection: "sel-x"})
			if _, err := f.plugin.Pick(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			f.plugin.ObserveFailure("sel-x", tc.status, tc.body)
			if _, quota := f.plugin.control.Statuses(f.ids["a"]); (quota == "quota_exhausted") != tc.locked {
				t.Fatalf("quota status = %q, locked want %v", quota, tc.locked)
			}
			_, err := f.pick(pickOptions{session: "codex:s2"})
			if tc.locked && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("a locked account was still routable: %v", err)
			}
		})
	}
}

func TestQuotaLockSurvivesRestartAndStaleResultCannotRelock(t *testing.T) {
	f := newFixture(t, 1)
	request := f.request(pickOptions{selection: "sel-old"})
	if _, err := f.plugin.Pick(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	f.plugin.ObserveFailure("sel-old", 429, `{"error":{"type":"usage_limit_reached"}}`)
	f.build()
	if !f.plugin.control.Blocked(f.ids["a"]) {
		t.Fatal("the quota lock did not survive a restart")
	}
	// The Owner re-enables the account; a result selected before that must not
	// lock it again.
	if err := f.plugin.control.SetEnabled(f.ids["a"], true); err != nil {
		t.Fatal(err)
	}
	if changed, _ := f.plugin.control.MarkQuotaExhausted(f.ids["a"], 1); changed {
		t.Fatal("a result older than the Owner's action relocked the account")
	}
	if f.plugin.control.Blocked(f.ids["a"]) {
		t.Fatal("the account is blocked after being re-enabled")
	}
}

func TestLateAccountFailureDropsHoldAndBinding(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.prefer = []string{f.ids["a"]}
	request := f.request(pickOptions{session: "codex:s1", selection: "sel-late"})
	token := http.Header(request.Options.Headers).Get(HeaderRequest)
	f.plugin.requests.begin(token, "req-late", true)
	if _, err := f.plugin.Pick(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	f.plugin.Complete("req-late", pluginapi.RequestCompletionFailed)
	// The failure report arrives after completion.
	f.plugin.ObserveFailure("sel-late", 500, `{"error":{"type":"server_error"}}`)
	if f.active("a") != 0 {
		t.Fatalf("active on a = %d, want the hold dropped", f.active("a"))
	}
	f.alloc.prefer = []string{f.ids["b"]}
	if name := f.mustPick(pickOptions{session: "codex:s1"}); name != "b" {
		t.Fatalf("picked %q, want a fresh allocation after the binding was dropped", name)
	}
}

func TestUnavailablePluginRefusesEverything(t *testing.T) {
	f := newFixture(t, 1)
	f.plugin.unavailable = ErrStateUnavailable
	if raw := f.plugin.Handle("scheduler.pick", []byte(`{}`)); !containsAll(string(raw), `"ok":false`, ErrUnavailable.Error(), `"http_status":503`) {
		t.Fatalf("unexpected pick response: %s", raw)
	}
	headers := http.Header{"Authorization": {"Bearer " + testInternalKey}}
	if response := f.plugin.Authenticate(headers); response.Authenticated {
		t.Fatal("an unavailable plugin authenticated a request")
	}
}

func TestHandleRecoversPanics(t *testing.T) {
	f := newFixture(t, 1)
	f.plugin.inventory = nil // any pick now dereferences nil
	request := f.request(pickOptions{})
	raw, _ := jsonMarshal(request)
	response := string(f.plugin.Handle("scheduler.pick", raw))
	if !containsAll(response, `"ok":false`, ErrUnavailable.Error(), `"http_status":503`) {
		t.Fatalf("a panic was not turned into a refusal: %s", response)
	}
}

func TestSweepReleasesAbandonedRequests(t *testing.T) {
	f := newFixture(t, 1)
	request := f.request(pickOptions{session: "codex:s1"})
	if _, err := f.plugin.Pick(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	f.advance(7 * time.Hour)
	// Any later pick sweeps; the abandoned request's slot comes back.
	if _, err := f.pick(pickOptions{session: "codex:s2"}); err != nil {
		t.Fatalf("an abandoned request kept its slot forever: %v", err)
	}
}
