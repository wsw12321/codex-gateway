package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	attemptUser    = "00000000-0000-0000-0000-000000000001"
	attemptAccount = "0123456789abcdef"
	attemptDenied  = "fedcba9876543210"
	attemptHash    = "conv-0123456789abcdef0123456789abcdef"
)

type attemptRoundTrip func(*http.Request) (*http.Response, error)

func (f attemptRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func attemptRequest(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sidecar-test-secret")
	return r
}

// attemptDispatch forwards one request through a ledger-backed client whose
// fake CPA posts report to the Gateway's attempt endpoint, the way the plugin
// does while the dispatch is in flight. It returns the endpoint's status and
// the dispatch result.
func attemptDispatch(t *testing.T, server *Server, provider string, groups []gatewayproxy.AllocationGroup, report func(token string) string) (int, string, gatewayproxy.Result) {
	t.Helper()
	status, body := 0, ""
	transport := attemptRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(
				`{"protocol":"upstream_account_access_v1","group_upstream_account_scope":"group_upstream_account_scope_v1"}`))}, nil
		}
		w := httptest.NewRecorder()
		server.recordUpstreamAttempt(w, attemptRequest(groupAllocationTestPath(provider, "attempt"), report(r.Header.Get("X-Codex-Gateway-Request"))))
		status, body = w.Code, w.Body.String()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
	})
	base, _ := url.Parse("http://compat.invalid")
	client := withManagement(gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}))
	client.UseAllocationLedger(server.allocations)
	incoming := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
	result, failure := client.ForwardWithOptions(context.Background(), httptest.NewRecorder(), incoming, "/v1/responses", gatewayproxy.ForwardOptions{UserID: attemptUser, Groups: groups})
	if failure != nil {
		t.Fatalf("dispatch failed: %v", failure)
	}
	return status, body, result
}

func attemptBody(fields map[string]string) string {
	raw, _ := json.Marshal(fields)
	return string(raw)
}

func TestUpstreamAttemptAttributesInFlightDispatch(t *testing.T) {
	var queries []string
	server := groupAllocationTestServer(t, store.UpstreamProviderCodex, attemptDenied, "", &queries)
	server.allocations = gatewayproxy.NewAllocationLedger()
	status, body, result := attemptDispatch(t, server, store.UpstreamProviderCodex, nil, func(token string) string {
		return attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "account_id": attemptAccount, "conversation_hash": attemptHash})
	})
	if status != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if result.UpstreamAccountID != attemptAccount || result.ConversationHash != attemptHash {
		t.Fatalf("attribution = %q/%q", result.UpstreamAccountID, result.ConversationHash)
	}
	if len(queries) != 1 || queries[0] != "eligible:"+attemptAccount {
		t.Fatalf("the reported account was not rechecked against the user's entitlement: %v", queries)
	}
}

func TestUpstreamAttemptRefusesAccountsTheUserMayNotUse(t *testing.T) {
	var queries []string
	server := groupAllocationTestServer(t, store.UpstreamProviderCodex, attemptDenied, "", &queries)
	server.allocations = gatewayproxy.NewAllocationLedger()
	status, body, result := attemptDispatch(t, server, store.UpstreamProviderCodex, nil, func(token string) string {
		return attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "account_id": attemptDenied})
	})
	if status != http.StatusForbidden || !strings.Contains(body, "upstream_account_not_permitted") || result.UpstreamAccountID != "" {
		t.Fatalf("status=%d body=%s attributed=%q", status, body, result.UpstreamAccountID)
	}
	// An account the user may use, but outside the group pool this request
	// was admitted with, is refused as well.
	groups := []gatewayproxy.AllocationGroup{{GroupID: "00000000-0000-0000-0000-000000000002", AccountIDs: []string{"1111111111111111"}}}
	status, body, result = attemptDispatch(t, server, store.UpstreamProviderCodex, groups, func(token string) string {
		return attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "account_id": attemptAccount})
	})
	if status != http.StatusForbidden || !strings.Contains(body, "upstream_account_not_permitted") || result.UpstreamAccountID != "" {
		t.Fatalf("status=%d body=%s attributed=%q", status, body, result.UpstreamAccountID)
	}
}

func TestUpstreamAttemptRejectsReportsWithoutADispatch(t *testing.T) {
	var queries []string
	server := groupAllocationTestServer(t, store.UpstreamProviderCodex, "", "", &queries)
	server.allocations = gatewayproxy.NewAllocationLedger()
	token := strings.Repeat("A", 43)
	valid := attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "account_id": attemptAccount})

	w := httptest.NewRecorder()
	server.recordUpstreamAttempt(w, attemptRequest("/internal/upstream-accounts/attempt", valid))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "unknown_upstream_dispatch") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// A dispatch belongs to one provider's callback namespace.
	status, body, result := attemptDispatch(t, server, store.UpstreamProviderCodex, nil, func(token string) string {
		return attemptBody(map[string]string{"request_token": token, "user_id": "00000000-0000-0000-0000-000000000009", "account_id": attemptAccount})
	})
	if status != http.StatusConflict || result.UpstreamAccountID != "" {
		t.Fatalf("another user's report was accepted: status=%d body=%s", status, body)
	}

	for _, authorization := range [][]string{nil, {"Bearer wrong"}, {"Bearer sidecar-test-secret", "Bearer sidecar-test-secret"}} {
		r := attemptRequest("/internal/upstream-accounts/attempt", valid)
		r.Header["Authorization"] = authorization
		w = httptest.NewRecorder()
		server.recordUpstreamAttempt(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %v status=%d", authorization, w.Code)
		}
	}
	for _, body := range []string{
		``, `{}`, `[]`, `null`,
		attemptBody(map[string]string{"request_token": "short", "user_id": attemptUser, "account_id": attemptAccount}),
		attemptBody(map[string]string{"request_token": token, "user_id": "not-a-uuid", "account_id": attemptAccount}),
		attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "account_id": "0123456789ABCDEF"}),
		attemptBody(map[string]string{"request_token": token, "user_id": attemptUser}),
		attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "rejected": "anything"}),
		attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "account_id": attemptAccount, "rejected": "upstream_concurrency_exceeded"}),
		attemptBody(map[string]string{"request_token": token, "user_id": attemptUser, "account_id": attemptAccount, "extra": "x"}),
		`{"request_token":"` + token + `","request_token":"` + token + `","user_id":"` + attemptUser + `","account_id":"` + attemptAccount + `"}`,
		valid + `{}`,
	} {
		w = httptest.NewRecorder()
		server.recordUpstreamAttempt(w, attemptRequest("/internal/upstream-accounts/attempt", body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q status=%d", body, w.Code)
		}
	}
	r := attemptRequest("/internal/upstream-accounts/attempt?x=1", valid)
	w = httptest.NewRecorder()
	server.recordUpstreamAttempt(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a query string was accepted: %d", w.Code)
	}
}

func TestUpstreamAttemptRecordsRefusalWithoutEligibilityQuery(t *testing.T) {
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		var queries []string
		server := groupAllocationTestServer(t, provider, "", "", &queries)
		server.allocations = gatewayproxy.NewAllocationLedger()
		w := httptest.NewRecorder()
		// A refusal for a dispatch that is not in flight is still a conflict,
		// and never touches the database.
		server.recordUpstreamAttempt(w, attemptRequest(groupAllocationTestPath(provider, "attempt"), attemptBody(map[string]string{"request_token": strings.Repeat("A", 43), "user_id": attemptUser, "rejected": "upstream_concurrency_exceeded"})))
		if w.Code != http.StatusConflict || len(queries) != 0 {
			t.Fatalf("%s: status=%d queries=%v", provider, w.Code, queries)
		}
	}
}
