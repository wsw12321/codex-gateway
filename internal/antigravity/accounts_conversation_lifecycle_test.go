package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAccountConversationFailoverPreservesSiblingReferenceAndCooldown(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			_, selector := newConversationSelector(t, map[string]int64{accountID("default"): 1, accountID("work"): 1})
			primary, backup := newConversationAccountExecutor(), newConversationAccountExecutor()
			server := accountServer(t, selector.URL, primary, backup)
			scope := strings.Repeat("a", 43)
			sibling := startAccountConversation(t, server, primary, accountConversationA, scope, false)
			retrying := startAccountConversation(t, server, primary, accountConversationA, scope, true)
			assertAccountConversationUsage(t, server, "default", 2, 1)
			retrying.finish <- &Failure{status, "upstream_test_failure", "Retry this account"}
			var finishBackup chan *Failure
			select {
			case finishBackup = <-backup.entered:
			case response := <-retrying.done:
				t.Fatalf("retry ended before backup execution: %d %s", response.Code, response.Body)
			case <-time.After(3 * time.Second):
				t.Fatal("retry did not reach backup")
			}
			assertAccountConversationUsage(t, server, "default", 1, 1)
			assertAccountConversationUsage(t, server, "work", 1, 1)
			server.manager.mu.Lock()
			cooldown := server.manager.accounts[0].cooldown
			server.manager.mu.Unlock()
			if !time.Now().Before(cooldown) {
				t.Fatal("failed account did not enter cooldown")
			}
			sibling.finish <- nil
			if response := sibling.wait(t); response.Code != 200 {
				t.Fatalf("sibling failed: %d %s", response.Code, response.Body)
			}
			assertAccountConversationUsage(t, server, "default", 0, 0)
			server.manager.mu.Lock()
			account := server.manager.accounts[0]
			gotCooldown, ready, quota := account.cooldown, account.ready, account.quota
			server.manager.mu.Unlock()
			if !gotCooldown.Equal(cooldown) || status == 429 && !quota || status == 503 && ready {
				t.Fatalf("late sibling success erased failure: cooldown=%v want %v ready=%v quota=%v", gotCooldown, cooldown, ready, quota)
			}
			// A continuation must stay off the original account even after the
			// sibling succeeds, and share the retry's slot on its new account.
			continuation := startAccountConversation(t, server, backup, accountConversationA, scope, false)
			assertAccountConversationUsage(t, server, "work", 2, 1)
			finishBackup <- nil
			if response := retrying.wait(t); response.Code != 200 || response.Header().Get("X-Codex-Upstream-Account") != accountID("work") {
				t.Fatalf("retry response=%d %s", response.Code, response.Body)
			}
			assertAccountConversationUsage(t, server, "default", 0, 0)
			assertAccountConversationUsage(t, server, "work", 1, 1)
			continuation.finish <- nil
			continuation.wait(t)
			assertAccountConversationUsage(t, server, "work", 0, 0)
		})
	}
}

func TestAccountConversationSlotHeldDuringJSONAndSSEWrite(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[stream], func(t *testing.T) {
			_, selector := newConversationSelector(t, map[string]int64{accountID("default"): 1})
			executor := newConversationAccountExecutor()
			server := accountServer(t, selector.URL, executor)
			scope := strings.Repeat("a", 43)
			writer := &blockedAccountWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
			unblock := sync.OnceFunc(func() { close(writer.release) })
			t.Cleanup(unblock)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan struct{})
			go func() {
				defer close(done)
				server.ServeHTTP(writer, newAccountConversationRequest(ctx, accountConversationA, scope, stream))
			}()
			select {
			case finish := <-executor.entered:
				finish <- nil
			case <-time.After(3 * time.Second):
				t.Fatal("request did not execute")
			}
			select {
			case <-writer.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("response write did not start")
			}
			assertAccountConversationUsage(t, server, "default", 1, 1)
			continuation := startAccountConversation(t, server, executor, accountConversationA, scope, stream)
			assertAccountConversationUsage(t, server, "default", 2, 1)
			if response := postAccountConversation(server, context.Background(), accountConversationB, scope, stream); response.Code != 429 {
				t.Fatalf("blocked writer lost its conversation slot: %d", response.Code)
			}
			continuation.finish <- nil
			continuation.wait(t)
			assertAccountConversationUsage(t, server, "default", 1, 1)
			unblock()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("response write did not finish")
			}
			assertAccountConversationUsage(t, server, "default", 0, 0)
		})
	}
}

type panicConversationExecutor struct {
	*conversationAccountExecutor
}

func (e *panicConversationExecutor) Run(ctx context.Context, model, prompt string) (Result, *Failure) {
	result, failure := e.conversationAccountExecutor.Run(ctx, model, prompt)
	if failure != nil && failure.Code == "test_panic" {
		panic("test conversation panic")
	}
	return result, failure
}

func TestAccountConversationPanicReleasesOnlyItsReference(t *testing.T) {
	_, selector := newConversationSelector(t, map[string]int64{accountID("default"): 1})
	executor := &panicConversationExecutor{newConversationAccountExecutor()}
	server := accountServer(t, selector.URL, executor)
	scope := strings.Repeat("a", 43)
	sibling := startAccountConversation(t, server, executor.conversationAccountExecutor, accountConversationA, scope, false)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		server.ServeHTTP(httptest.NewRecorder(), newAccountConversationRequest(ctx, accountConversationA, scope, false))
	}()
	select {
	case finish := <-executor.entered:
		finish <- &Failure{500, "test_panic", "Panic after admission"}
	case <-time.After(3 * time.Second):
		t.Fatal("panicking request did not execute")
	}
	select {
	case recovered := <-panicked:
		if recovered != "test conversation panic" {
			t.Fatalf("unexpected panic: %v", recovered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not panic")
	}
	assertAccountConversationUsage(t, server, "default", 1, 1)
	if response := postAccountConversation(server, context.Background(), accountConversationB, scope, false); response.Code != 429 {
		t.Fatalf("panic released the sibling's slot: %d", response.Code)
	}
	sibling.finish <- nil
	sibling.wait(t)
	assertAccountConversationUsage(t, server, "default", 0, 0)
	next := startAccountConversation(t, server, executor.conversationAccountExecutor, accountConversationB, scope, false)
	next.finish <- nil
	next.wait(t)
	assertAccountConversationUsage(t, server, "default", 0, 0)
	if len(server.gate) != 0 {
		t.Fatal("panic leaked a bridge request slot")
	}
}

func TestAccountConversationStillHonors64RequestBridgeGate(t *testing.T) {
	_, selector := newConversationSelector(t, map[string]int64{accountID("default"): 1})
	executor := newConversationAccountExecutor()
	server := accountServer(t, selector.URL, executor)
	scope := strings.Repeat("a", 43)
	pending := make([]*pendingAccountConversation, 0, 64)
	for range 64 {
		pending = append(pending, startAccountConversation(t, server, executor, accountConversationA, scope, false))
	}
	assertAccountConversationUsage(t, server, "default", 64, 1)
	request := newAccountConversationRequest(context.Background(), accountConversationA, scope, false)
	body := &failOnRead{}
	request.Body = body
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 429 || response.Header().Get("Retry-After") != "1" || body.read.Load() {
		t.Fatalf("bridge gate bypassed: status=%d bodyRead=%v response=%s", response.Code, body.read.Load(), response.Body)
	}
	pending[0].finish <- nil
	pending[0].wait(t)
	assertAccountConversationUsage(t, server, "default", 63, 1)
	pending[0] = startAccountConversation(t, server, executor, accountConversationA, scope, true)
	assertAccountConversationUsage(t, server, "default", 64, 1)
	for _, request := range pending {
		request.finish <- nil
		request.wait(t)
	}
	assertAccountConversationUsage(t, server, "default", 0, 0)
	if len(server.gate) != 0 {
		t.Fatal("bridge gate retained finished requests")
	}
}
