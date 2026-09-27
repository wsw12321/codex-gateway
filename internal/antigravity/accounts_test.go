package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const accountUserID = "11111111-1111-4111-8111-111111111111"
const accountRequestBody = `{"model":"gemini-3.1-pro-preview","input":"hello"}`

type accountExecutor struct {
	failure *Failure
	runs    atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (*accountExecutor) Check(context.Context) error { return nil }
func (e *accountExecutor) Run(ctx context.Context, _ string) (Result, *Failure) {
	e.runs.Add(1)
	if e.entered != nil {
		e.entered <- struct{}{}
	}
	if e.release != nil {
		select {
		case <-ctx.Done():
			return Result{}, &Failure{499, "request_canceled", "Canceled"}
		case <-e.release:
		}
	}
	return Result{Status: "SUCCESS", Response: "test response", Usage: &Usage{}}, e.failure
}

func testRegistry(t *testing.T, names ...string) *AccountRegistry {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	registry := &AccountRegistry{path: filepath.Join(dir, "accounts.json")}
	for _, name := range names {
		if err := registry.Register(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func accountServer(t *testing.T, gateway string, executors ...Executor) *Server {
	t.Helper()
	names := []string{"default", "work", "backup"}
	server, err := NewAccountServer(Runner{}, testRegistry(t, names[:len(executors)]...), testBridgeToken, gateway)
	if err != nil {
		t.Fatal(err)
	}
	for index, executor := range executors {
		server.manager.accounts[index].runner = executor
	}
	server.Refresh(context.Background())
	return server
}

func managedRequest(server http.Handler, ctx context.Context, path, body, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testBridgeToken)
	if user != "" {
		req.Header.Set("X-Codex-Gateway-User", user)
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	return w
}

func testSelector(t *testing.T, allowed map[string]int64, selections *[]string, selectionMu *sync.Mutex) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testBridgeToken {
			t.Error("callback token mismatch")
		}
		var request struct {
			IDs  []string `json:"account_ids"`
			User string   `json:"user_id"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.User != accountUserID {
			t.Error("invalid callback")
		}
		if strings.HasSuffix(r.URL.Path, "/eligible") {
			accounts := []map[string]any{}
			for _, id := range request.IDs {
				if limit := allowed[id]; limit > 0 {
					accounts = append(accounts, map[string]any{"id": id, "concurrent_limit": limit})
				}
			}
			writeJSON(w, 200, map[string]any{"accounts": accounts})
			return
		}
		if len(request.IDs) == 0 {
			t.Error("empty select")
			http.Error(w, "empty", 500)
			return
		}
		selectionMu.Lock()
		*selections = append(*selections, request.IDs[0])
		selectionMu.Unlock()
		writeJSON(w, 200, map[string]string{"account_id": request.IDs[0]})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAccountManagerRotatesBeforeJSONOrSSEOutput(t *testing.T) {
	for _, status := range []int{429, 401, 403, 503} {
		for _, stream := range []bool{false, true} {
			t.Run(http.StatusText(status)+map[bool]string{true: " SSE", false: " JSON"}[stream], func(t *testing.T) {
				var chosen []string
				var mu sync.Mutex
				selector := testSelector(t, map[string]int64{accountID("default"): 2, accountID("work"): 2}, &chosen, &mu)
				bad, good := &accountExecutor{failure: &Failure{status, "synthetic_failure", "Provider failure"}}, &accountExecutor{}
				server := accountServer(t, selector.URL, bad, good)
				body := accountRequestBody
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				response := managedRequest(server, context.Background(), "/v1/responses", body, accountUserID)
				if response.Code != 200 || response.Header().Get("X-Codex-Upstream-Account") != accountID("work") {
					t.Fatalf("response=%d %s headers=%v", response.Code, response.Body, response.Header())
				}
				if bad.runs.Load() != 1 || good.runs.Load() != 1 || strings.Contains(response.Body.String(), "synthetic_failure") {
					t.Fatal("rotation did not isolate output")
				}
				if stream && !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("missing SSE completion")
				}
				response = managedRequest(server, context.Background(), "/v1/responses", body, accountUserID)
				if response.Code != 200 || bad.runs.Load() != 1 {
					t.Fatal("cooldown was ignored")
				}
				for _, account := range server.manager.accounts {
					if account.active != 0 {
						t.Fatal("request lease leaked")
					}
				}
			})
		}
	}
}

func TestAccountManagerFailsClosedOnSelectorProtocolOrMissingIdentity(t *testing.T) {
	for _, reply := range []string{`{}`, `{"Accounts":[]}`, `{"accounts":null}`, `{"accounts":[{"id":"0123456789abcdef","concurrent_limit":1}]}`, `{"accounts":[{"id":"` + accountID("default") + `","concurrent_limit":0}]}`, `{"accounts":[{"ID":"` + accountID("default") + `","concurrent_limit":1}]}`, `{"accounts":[],"accounts":[]}`, `{"accounts":[]} {}`, `{"accounts":[],"extra":"secret"}`} {
		t.Run(reply, func(t *testing.T) {
			selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(reply))
			}))
			defer selector.Close()
			executor := &accountExecutor{}
			server := accountServer(t, selector.URL, executor)
			response := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID)
			if response.Code != 503 || executor.runs.Load() != 0 {
				t.Fatalf("response=%d executed=%d", response.Code, executor.runs.Load())
			}
		})
	}
	for _, user := range []string{"", "invalid-user", "11111111111141118111111111111111"} {
		executor := &accountExecutor{}
		server := accountServer(t, "http://127.0.0.1:1", executor)
		response := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, user)
		if response.Code != 503 || executor.runs.Load() != 0 {
			t.Fatal("missing authenticated identity bypassed allocation")
		}
	}
	selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/private", http.StatusTemporaryRedirect)
	}))
	defer selector.Close()
	server := accountServer(t, selector.URL, &accountExecutor{})
	if got := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID); got.Code != 503 {
		t.Fatalf("redirect=%d", got.Code)
	}
}

func TestAccountRequestConcurrencyPermissionsAndCancellation(t *testing.T) {
	var selected []string
	var mu sync.Mutex
	selector := testSelector(t, map[string]int64{accountID("default"): 2}, &selected, &mu)
	allowed := &accountExecutor{entered: make(chan struct{}, 4), release: make(chan struct{})}
	denied := &accountExecutor{}
	server := accountServer(t, selector.URL, allowed, denied)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { done <- managedRequest(server, ctx, "/v1/responses", accountRequestBody, accountUserID) }()
		select {
		case <-allowed.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent request did not start")
		}
	}
	response := bridgeRequest(server, "GET", "/internal/upstream-accounts/concurrency", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"active_requests":2`) {
		t.Fatalf("concurrency=%d %s", response.Code, response.Body)
	}
	third := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID)
	if third.Code != 429 || denied.runs.Load() != 0 || allowed.runs.Load() != 2 {
		t.Fatal("concurrency or account access not enforced")
	}
	server.Refresh(context.Background()) // Busy accounts are not probed.
	cancel()
	for range 2 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("cancellation blocked")
		}
	}
	response = bridgeRequest(server, "GET", "/internal/upstream-accounts/concurrency", "")
	if strings.Contains(response.Body.String(), `"active_requests":2`) || strings.Contains(response.Body.String(), `"active_requests":1`) {
		t.Fatalf("leaked concurrency=%s", response.Body)
	}
}

func TestAccountStatusPersistsAndSmokeRequiresLoopback(t *testing.T) {
	server := accountServer(t, "http://127.0.0.1:1", &accountExecutor{})
	path := "/internal/upstream-accounts/" + accountID("default") + "/status"
	set := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testBridgeToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}
	for _, body := range []string{`{"Enabled":true}`, `{"enabled":null}`, `{"enabled":true,"enabled":false}`, `{"enabled":true,"extra":1}`} {
		if response := set(body); response.Code != 400 {
			t.Fatalf("accepted malformed status %s", body)
		}
	}
	if response := set(`{"enabled":false}`); response.Code != 200 || !strings.Contains(response.Body.String(), "manual_disabled") {
		t.Fatalf("status=%d %s", response.Code, response.Body)
	}
	loaded, err := OpenAccountRegistry(context.Background(), server.manager.registry.path, Runner{})
	if err != nil || loaded.Records()[0].Enabled {
		t.Fatalf("disabled status not persisted: %v", err)
	}
	if response := set(`{"enabled":true}`); response.Code != 200 {
		t.Fatal(response.Code)
	}
	for _, remote := range []string{"192.0.2.1:9999", "127.0.0.1:9999"} {
		req := httptest.NewRequest("POST", "/internal/smoke/responses/default", strings.NewReader(accountRequestBody))
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+testBridgeToken)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		want := 404
		if strings.HasPrefix(remote, "127.") {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("smoke from %s=%d", remote, w.Code)
		}
	}
}

func TestAccountRegistryValidatesAndProtectsMetadata(t *testing.T) {
	registry := testRegistry(t, "default", "work")
	if err := registry.Register("../../secret", ""); err == nil {
		t.Fatal("unsafe name accepted")
	}
	if err := registry.Register("bad", "private@example.com"); err == nil {
		t.Fatal("raw email accepted")
	}
	loaded, err := OpenAccountRegistry(context.Background(), registry.path, Runner{})
	if err != nil || len(loaded.Records()) != 2 {
		t.Fatalf("reload=%v", err)
	}
	if err := os.Chmod(registry.path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAccountRegistry(context.Background(), registry.path, Runner{}); err == nil {
		t.Fatal("public registry accepted")
	}
	if err := os.Remove(registry.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", registry.path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAccountRegistry(context.Background(), registry.path, Runner{}); err == nil {
		t.Fatal("registry symlink accepted")
	}
}

func TestAccountRegistryAdoptsLegacyCredentialsAndMasksIdentity(t *testing.T) {
	registry := testRegistry(t)
	data := strings.Replace(syntheticCredential, `"auth_method":"oauth"`, `"auth_method":"oauth","email":"private.person@example.com"`, 1)
	runner := Runner{Credentials: &runnerCredentials{data: data}}
	loaded, err := OpenAccountRegistry(context.Background(), registry.path, runner)
	if err != nil {
		t.Fatal(err)
	}
	records := loaded.Records()
	if len(records) != 1 || records[0].Name != "default" || records[0].MaskedEmail != "p***@example.com" {
		t.Fatalf("adoption=%+v", records)
	}
	persisted, err := os.ReadFile(registry.path)
	if err != nil || strings.Contains(string(persisted), "private") || strings.Contains(string(persisted), "token") {
		t.Fatal("registry disclosed credentials or raw identity")
	}
	missing := testRegistry(t)
	_, err = OpenAccountRegistry(context.Background(), missing.path, Runner{Credentials: &runnerCredentials{restoreErr: ErrCredentialsMissing}})
	if err != nil && !errors.Is(err, ErrCredentialsMissing) {
		t.Fatal(err)
	}
}

func TestAccountSelectorZeroWeightAndEmptyAccessRemainRateLimited(t *testing.T) {
	for _, empty := range []bool{false, true} {
		selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/eligible") {
				accounts := []map[string]any{}
				if !empty {
					accounts = append(accounts, map[string]any{"id": accountID("default"), "concurrent_limit": 2})
				}
				writeJSON(w, 200, map[string]any{"accounts": accounts})
			} else {
				writeJSON(w, 429, map[string]any{"error": map[string]string{"code": "upstream_concurrency_exceeded"}})
			}
		}))
		executor := &accountExecutor{}
		server := accountServer(t, selector.URL, executor)
		response := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID)
		selector.Close()
		if response.Code != 429 || executor.runs.Load() != 0 || response.Header().Get("Retry-After") == "" {
			t.Fatalf("allocation denial=%d %s", response.Code, response.Body)
		}
	}
}

func TestAccountCallbacksBypassEgressProxyAndLeaveStateUnlocked(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:1234")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:1234")
	var server *Server
	selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Callback may inspect bridge state without causing a lock cycle.
		if !server.manager.mu.TryLock() {
			t.Error("state mutex held during callback")
		} else {
			server.manager.mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/eligible") {
			writeJSON(w, 200, map[string]any{"accounts": []map[string]any{{"id": accountID("default"), "concurrent_limit": 1}}})
		} else {
			writeJSON(w, 200, map[string]string{"account_id": accountID("default")})
		}
	}))
	defer selector.Close()
	server = accountServer(t, selector.URL, &accountExecutor{})
	transport, ok := server.manager.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("callback can leak through CLI egress proxy")
	}
	response := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID)
	if response.Code != 200 {
		t.Fatalf("callback=%d %s", response.Code, response.Body)
	}
}

type blockedAccountWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (w *blockedAccountWriter) Header() http.Header { return w.header }
func (*blockedAccountWriter) WriteHeader(int)       {}
func (w *blockedAccountWriter) Write(data []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(data), nil
}

func TestAccountLeaseCountsRequestUntilResponseWriteFinishes(t *testing.T) {
	t.Run("success", func(t *testing.T) { testAccountLeaseDuringWrite(t, nil) })
	t.Run("final failure", func(t *testing.T) {
		testAccountLeaseDuringWrite(t, &Failure{502, "upstream_process_error", "Process failed"})
	})
}

func testAccountLeaseDuringWrite(t *testing.T, failure *Failure) {
	var selected []string
	var mu sync.Mutex
	selector := testSelector(t, map[string]int64{accountID("default"): 1}, &selected, &mu)
	server := accountServer(t, selector.URL, &accountExecutor{failure: failure})
	writer := &blockedAccountWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(accountRequestBody))
	req.Header.Set("Authorization", "Bearer "+testBridgeToken)
	req.Header.Set("X-Codex-Gateway-User", accountUserID)
	done := make(chan struct{})
	go func() { defer close(done); server.ServeHTTP(writer, req) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("response write did not start")
	}
	response := bridgeRequest(server, "GET", "/internal/upstream-accounts/concurrency", "")
	if !strings.Contains(response.Body.String(), `"active_requests":1`) {
		t.Fatalf("response lease released early: %s", response.Body)
	}
	if response := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID); response.Code != 429 {
		t.Fatalf("slow response failed to hold limit: %d", response.Code)
	}
	close(writer.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("response didn't release")
	}
	response = bridgeRequest(server, "GET", "/internal/upstream-accounts/concurrency", "")
	if strings.Contains(response.Body.String(), `"active_requests":1`) {
		t.Fatal("response lease leaked")
	}
}

func TestAccountManagerNativeGeminiUsesAccountPermissions(t *testing.T) {
	for _, method := range []string{"generateContent", "streamGenerateContent?alt=sse"} {
		var selected []string
		var mu sync.Mutex
		selector := testSelector(t, map[string]int64{accountID("work"): 1}, &selected, &mu)
		denied, allowed := &accountExecutor{}, &accountExecutor{}
		server := accountServer(t, selector.URL, denied, allowed)
		response := managedRequest(server, context.Background(), "/v1beta/models/"+PublicModel+":"+method, nativeText, accountUserID)
		if response.Code != 200 || response.Header().Get("X-Codex-Upstream-Account") != accountID("work") || denied.runs.Load() != 0 || allowed.runs.Load() != 1 {
			t.Fatalf("native selection=%d %s", response.Code, response.Body)
		}
	}
}

type concurrentFailureExecutor struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (*concurrentFailureExecutor) Check(context.Context) error { return nil }
func (e *concurrentFailureExecutor) Run(ctx context.Context, _ string) (Result, *Failure) {
	if e.calls.Add(1) == 1 {
		close(e.entered)
		select {
		case <-e.release:
		case <-ctx.Done():
			return Result{}, &Failure{499, "request_canceled", "Canceled"}
		}
		return Result{Status: "SUCCESS", Response: "success", Usage: &Usage{}}, nil
	}
	return Result{}, &Failure{429, "upstream_rate_limited", "Quota exhausted"}
}

func TestLateSuccessfulRequestPreservesSiblingQuotaCooldown(t *testing.T) {
	var selected []string
	var mu sync.Mutex
	selector := testSelector(t, map[string]int64{accountID("default"): 2}, &selected, &mu)
	executor := &concurrentFailureExecutor{entered: make(chan struct{}), release: make(chan struct{})}
	server := accountServer(t, selector.URL, executor)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID)
	}()
	select {
	case <-executor.entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}
	if got := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID); got.Code != 429 {
		t.Fatalf("quota failure=%d", got.Code)
	}
	close(executor.release)
	select {
	case response := <-done:
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not finish")
	}
	response := bridgeRequest(server, "GET", "/internal/upstream-accounts", "")
	if !strings.Contains(response.Body.String(), `"gateway_quota_status":"quota_exhausted"`) {
		t.Fatalf("late success erased cooldown: %s", response.Body)
	}
}
