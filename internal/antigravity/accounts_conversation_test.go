package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const accountConversationA = "Conversation ID: 11111111-1111-4111-8111-111111111112"
const accountConversationB = "Conversation ID: 22222222-2222-4222-8222-222222222222"
const accountConversationC = "Conversation ID: 33333333-3333-4333-8333-333333333333"

type conversationAccountExecutor struct {
	entered chan chan *Failure
}

func newConversationAccountExecutor() *conversationAccountExecutor {
	return &conversationAccountExecutor{entered: make(chan chan *Failure, 65)}
}

func (*conversationAccountExecutor) Check(context.Context) ([]string, error) {
	return []string{PublicModel}, nil
}

func (e *conversationAccountExecutor) Run(ctx context.Context, _, _ string) (Result, *Failure) {
	finish := make(chan *Failure, 1)
	e.entered <- finish
	select {
	case failure := <-finish:
		return Result{Status: "SUCCESS", Response: "hello", Usage: &Usage{}}, failure
	case <-ctx.Done():
		return Result{}, &Failure{499, "request_canceled", "Canceled"}
	}
}

type conversationSelector struct {
	mu              sync.Mutex
	limits          map[string]int64
	zeroWeight      map[string]bool
	preferLast      bool
	fail            bool
	failEligibility bool
	before          func()
}

func newConversationSelector(t *testing.T, limits map[string]int64) (*conversationSelector, *httptest.Server) {
	t.Helper()
	state := &conversationSelector{limits: limits, zeroWeight: map[string]bool{}}
	selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			IDs  []string `json:"account_ids"`
			User string   `json:"user_id"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.User != accountUserID || r.Header.Get("Authorization") != "Bearer "+testBridgeToken {
			t.Error("invalid selector request")
			http.Error(w, "invalid request", 400)
			return
		}
		state.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/eligible") {
			if state.failEligibility {
				state.mu.Unlock()
				writeJSON(w, 503, map[string]string{"error": "unavailable"})
				return
			}
			accounts := []map[string]any{}
			for _, id := range request.IDs {
				if limit := state.limits[id]; limit > 0 {
					accounts = append(accounts, map[string]any{"id": id, "concurrent_limit": limit})
				}
			}
			state.mu.Unlock()
			writeJSON(w, 200, map[string]any{"accounts": accounts})
			return
		}
		before := state.before
		state.mu.Unlock()
		if before != nil {
			before()
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.fail {
			writeJSON(w, 503, map[string]string{"error": "unavailable"})
			return
		}
		if state.preferLast {
			for i, j := 0, len(request.IDs)-1; i < j; i, j = i+1, j-1 {
				request.IDs[i], request.IDs[j] = request.IDs[j], request.IDs[i]
			}
		}
		for _, id := range request.IDs {
			if !state.zeroWeight[id] {
				writeJSON(w, 200, map[string]string{"account_id": id})
				return
			}
		}
		writeFailure(w, accountBusyFailure())
	}))
	t.Cleanup(selector.Close)
	return state, selector
}

func newAccountConversationRequest(ctx context.Context, marker, scope string, stream bool) *http.Request {
	body := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hello"}}}}}
	if marker != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]string{"text": marker}}}
	}
	encoded, _ := json.Marshal(body)
	method := "generateContent"
	if stream {
		method = "streamGenerateContent?alt=sse"
	}
	r := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+PublicModel+":"+method, strings.NewReader(string(encoded))).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+testBridgeToken)
	r.Header.Set("X-Codex-Gateway-User", accountUserID)
	if scope != "" {
		r.Header.Set("X-Codex-Gateway-Affinity", scope)
	}
	return r
}

func postAccountConversation(server *Server, ctx context.Context, marker, scope string, stream bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	server.ServeHTTP(w, newAccountConversationRequest(ctx, marker, scope, stream))
	return w
}

type pendingAccountConversation struct {
	done   chan *httptest.ResponseRecorder
	finish chan *Failure
	cancel context.CancelFunc
}

func startAccountConversation(t *testing.T, server *Server, executor *conversationAccountExecutor, marker, scope string, stream bool) *pendingAccountConversation {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pending := &pendingAccountConversation{done: make(chan *httptest.ResponseRecorder, 1), cancel: cancel}
	go func() { pending.done <- postAccountConversation(server, ctx, marker, scope, stream) }()
	select {
	case pending.finish = <-executor.entered:
	case response := <-pending.done:
		t.Fatalf("conversation rejected: %d %s", response.Code, response.Body)
	case <-time.After(3 * time.Second):
		t.Fatal("conversation did not start on expected account")
	}
	return pending
}

func (p *pendingAccountConversation) wait(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-p.done:
		return response
	case <-time.After(3 * time.Second):
		t.Fatal("conversation did not finish")
		return nil
	}
}

func assertAccountConversationUsage(t *testing.T, server *Server, name string, requests, slots int64) {
	t.Helper()
	id := accountID(name)
	server.manager.mu.Lock()
	for _, account := range server.manager.accounts {
		if account.record.ID == id && (account.active != requests || account.activeSlots() != slots) {
			t.Errorf("%s: requests=%d slots=%d, want %d/%d", name, account.active, account.activeSlots(), requests, slots)
		}
	}
	server.manager.mu.Unlock()
	response := bridgeRequest(server, http.MethodGet, "/internal/upstream-accounts/concurrency", "")
	var snapshot struct {
		Accounts []struct {
			ID     string `json:"id"`
			Active int64  `json:"active_requests"`
		} `json:"accounts"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil {
		t.Fatalf("invalid concurrency snapshot: %d %s", response.Code, response.Body)
	}
	for _, account := range snapshot.Accounts {
		if account.ID == id {
			if account.Active != slots {
				t.Errorf("snapshot %s: active_requests=%d, want %d", name, account.Active, slots)
			}
			return
		}
	}
	t.Fatalf("account %s missing from snapshot", name)
}

func TestAccountConversationOverlapAndLastRelease(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[stream], func(t *testing.T) {
			_, selector := newConversationSelector(t, map[string]int64{accountID("default"): 1})
			executor := newConversationAccountExecutor()
			server := accountServer(t, selector.URL, executor)
			scope := strings.Repeat("a", 43)
			first := startAccountConversation(t, server, executor, accountConversationA, scope, stream)
			second := startAccountConversation(t, server, executor, accountConversationA, scope, stream)
			assertAccountConversationUsage(t, server, "default", 2, 1)
			for _, next := range []struct{ marker, scope string }{
				{accountConversationB, scope}, {accountConversationA, strings.Repeat("b", 43)}, {"", scope}, {accountConversationA, ""},
			} {
				if response := postAccountConversation(server, context.Background(), next.marker, next.scope, stream); response.Code != 429 {
					t.Fatalf("unrelated conversation admitted: %d %s", response.Code, response.Body)
				}
			}
			first.finish <- nil
			if response := first.wait(t); response.Code != 200 {
				t.Fatalf("first response: %d %s", response.Code, response.Body)
			}
			assertAccountConversationUsage(t, server, "default", 1, 1)
			second.cancel()
			second.wait(t)
			assertAccountConversationUsage(t, server, "default", 0, 0)
			third := startAccountConversation(t, server, executor, accountConversationB, scope, stream)
			third.finish <- nil
			third.wait(t)
			assertAccountConversationUsage(t, server, "default", 0, 0)
		})
	}
}

func TestAccountConversationUnidentifiedRequestsStayIndependent(t *testing.T) {
	for _, test := range []struct{ name, marker, scope string }{
		{"identical title", "", strings.Repeat("a", 43)},
		{"malformed marker", "Conversation ID: invalid", strings.Repeat("a", 43)},
		{"conflicting markers", accountConversationA + "\n" + accountConversationB, strings.Repeat("a", 43)},
		{"missing scope", accountConversationA, ""},
		{"invalid scope", accountConversationA, "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, selector := newConversationSelector(t, map[string]int64{accountID("default"): 2})
			executor := newConversationAccountExecutor()
			server := accountServer(t, selector.URL, executor)
			first := startAccountConversation(t, server, executor, test.marker, test.scope, false)
			second := startAccountConversation(t, server, executor, test.marker, test.scope, false)
			assertAccountConversationUsage(t, server, "default", 2, 2)
			if response := postAccountConversation(server, context.Background(), test.marker, test.scope, false); response.Code != 429 {
				t.Fatalf("unidentified requests shared a slot: %d", response.Code)
			}
			for _, pending := range []*pendingAccountConversation{first, second} {
				pending.finish <- nil
				if response := pending.wait(t); response.Header().Get("X-Codex-Conversation-Hash") != "" {
					t.Fatal("unidentified request emitted conversation hash")
				}
			}
			assertAccountConversationUsage(t, server, "default", 0, 0)
		})
	}
}

func TestAccountConversationPreferenceRechecksAccountPolicies(t *testing.T) {
	for _, policy := range []string{"unchanged", "permission revoked", "all permissions revoked", "eligibility failed", "disabled", "cooldown", "model removed", "zero weight", "all zero", "selector failed"} {
		t.Run(policy, func(t *testing.T) {
			state, selector := newConversationSelector(t, map[string]int64{accountID("default"): 1, accountID("work"): 1})
			state.preferLast = true
			primary, work := newConversationAccountExecutor(), newConversationAccountExecutor()
			server := accountServer(t, selector.URL, primary, work)
			scope := strings.Repeat("a", 43)
			first := startAccountConversation(t, server, work, accountConversationA, scope, false)
			state.mu.Lock()
			state.preferLast = false
			switch policy {
			case "permission revoked":
				delete(state.limits, accountID("work"))
			case "all permissions revoked":
				clear(state.limits)
			case "eligibility failed":
				state.failEligibility = true
			case "zero weight", "all zero":
				state.zeroWeight[accountID("work")] = true
				state.zeroWeight[accountID("default")] = policy == "all zero"
			case "selector failed":
				state.fail = true
			}
			state.mu.Unlock()
			server.manager.mu.Lock()
			switch policy {
			case "disabled":
				server.manager.accounts[1].record.Enabled = false
			case "cooldown":
				server.manager.accounts[1].cooldown = time.Now().Add(time.Minute)
			case "model removed":
				server.manager.accounts[1].models = nil
			}
			server.manager.mu.Unlock()
			if policy == "all zero" || policy == "selector failed" || policy == "all permissions revoked" || policy == "eligibility failed" {
				want := 429
				if policy == "selector failed" || policy == "eligibility failed" {
					want = 503
				}
				if response := postAccountConversation(server, context.Background(), accountConversationA, scope, false); response.Code != want {
					t.Fatalf("policy bypassed: %d, want %d", response.Code, want)
				}
			} else {
				executor, name := primary, "default"
				if policy == "unchanged" {
					executor, name = work, "work"
				}
				second := startAccountConversation(t, server, executor, accountConversationA, scope, false)
				second.finish <- nil
				if response := second.wait(t); response.Header().Get("X-Codex-Upstream-Account") != accountID(name) {
					t.Fatal("wrong account selected")
				}
			}
			assertAccountConversationUsage(t, server, "work", 1, 1)
			first.finish <- nil
			first.wait(t)
			assertAccountConversationUsage(t, server, "work", 0, 0)
			if policy == "unchanged" {
				// Once every reference is gone the old account has no affinity.
				third := startAccountConversation(t, server, primary, accountConversationA, scope, false)
				third.finish <- nil
				third.wait(t)
			}
		})
	}
}

func TestAccountConversationLoweredLimitAndCallbackRelease(t *testing.T) {
	state, selector := newConversationSelector(t, map[string]int64{accountID("default"): 2})
	executor := newConversationAccountExecutor()
	server := accountServer(t, selector.URL, executor)
	scope := strings.Repeat("a", 43)
	first := startAccountConversation(t, server, executor, accountConversationA, scope, false)
	other := startAccountConversation(t, server, executor, accountConversationB, scope, false)
	state.mu.Lock()
	state.limits[accountID("default")] = 1
	state.mu.Unlock()
	sibling := startAccountConversation(t, server, executor, accountConversationA, scope, false)
	assertAccountConversationUsage(t, server, "default", 3, 2)
	if response := postAccountConversation(server, context.Background(), accountConversationC, scope, false); response.Code != 429 {
		t.Fatal("new conversation bypassed lowered limit")
	}
	sibling.finish <- nil
	sibling.wait(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	state.mu.Lock()
	state.before = func() { close(entered); <-resume }
	state.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- postAccountConversation(server, ctx, accountConversationA, scope, false) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(resume)
		t.Fatal("selection did not start")
	}
	first.finish <- nil
	first.wait(t)
	close(resume)
	select {
	case response := <-done:
		if response.Code != 429 {
			t.Fatalf("last-reference release during callback bypassed limit: %d", response.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selection did not finish")
	}
	assertAccountConversationUsage(t, server, "default", 1, 1)
	other.finish <- nil
	other.wait(t)
	assertAccountConversationUsage(t, server, "default", 0, 0)
}
