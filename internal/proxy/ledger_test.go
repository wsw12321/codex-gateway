package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	ledgerUser    = "11111111-1111-4111-8111-111111111111"
	ledgerAccount = "0123456789abcdef"
	ledgerOther   = "fedcba9876543210"
	ledgerHash    = "conv-0123456789abcdef0123456789abcdef"
)

// ledgerSidecar plays CPA with the plugin: it answers the capability probe
// and, for a generation request, lets report act as the plugin's callback
// before returning the response.
func ledgerSidecar(t *testing.T, status int, header http.Header, body string, report func(token string)) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(
				`{"protocol":"` + AccountAccessProtocolV1 + `","group_upstream_account_scope":"` + GroupUpstreamAccountScopeV1 + `"}`))}, nil
		}
		token := r.Header.Get(dispatchHeader)
		if !requestTokenPattern.MatchString(token) {
			t.Errorf("dispatch carried no valid request token: %q", token)
		}
		if report != nil {
			report(token)
		}
		if header == nil {
			header = http.Header{}
		}
		if header.Get("Content-Type") == "" {
			header.Set("Content-Type", "application/json")
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
}

func ledgerForward(t *testing.T, ledger *AllocationLedger, transport http.RoundTripper, options ForwardOptions) (Result, *Failure) {
	t.Helper()
	base, _ := url.Parse("http://compat.invalid")
	client := NewWithHTTPClient(base, "secret", &http.Client{Transport: transport})
	client.UseAllocationLedger(ledger)
	request := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
	return client.ForwardWithOptions(context.Background(), httptest.NewRecorder(), request, "/v1/responses", options)
}

const ledgerBody = `{"model":"model","usage":{"input_tokens":1,"output_tokens":1}}`

func TestLedgerAttributesDispatchFromPluginReport(t *testing.T) {
	ledger := NewAllocationLedger()
	var notified, conversation string
	options := ForwardOptions{UserID: ledgerUser, OnUpstreamAccount: func(id string) { notified = id }, OnConversation: func(hash string) { conversation = hash }}
	// CPA with the plugin sets no attribution header; one that shows up anyway
	// must not be believed.
	spoofed := http.Header{upstreamAccountHeader: {ledgerOther}, conversationHashHeader: {"conv-ffffffffffffffffffffffffffffffff"}}
	transport := ledgerSidecar(t, 200, spoofed, ledgerBody, func(token string) {
		// A failed first attempt followed by the account that answered.
		for _, account := range []string{ledgerOther, ledgerAccount} {
			if err := ledger.Record("codex", AllocationReport{Token: token, UserID: ledgerUser, AccountID: account, ConversationHash: ledgerHash}); err != nil {
				t.Errorf("record: %v", err)
			}
		}
	})
	result, failure := ledgerForward(t, ledger, transport, options)
	if failure != nil {
		t.Fatalf("failure: %v", failure)
	}
	if result.UpstreamAccountID != ledgerAccount || result.ConversationHash != ledgerHash || notified != ledgerAccount || conversation != ledgerHash {
		t.Fatalf("attribution = %q/%q (notified %q/%q), want the last reported account", result.UpstreamAccountID, result.ConversationHash, notified, conversation)
	}
	if len(ledger.entries) != 0 {
		t.Fatal("the dispatch stayed in the ledger after it finished")
	}
}

func TestLedgerWithoutReportLeavesDispatchUnattributed(t *testing.T) {
	ledger := NewAllocationLedger()
	spoofed := http.Header{upstreamAccountHeader: {ledgerOther}}
	result, failure := ledgerForward(t, ledger, ledgerSidecar(t, 200, spoofed, ledgerBody, nil), ForwardOptions{UserID: ledgerUser})
	if failure != nil || result.UpstreamAccountID != "" {
		t.Fatalf("an unreported dispatch was attributed to %q (%v)", result.UpstreamAccountID, failure)
	}
}

func TestLedgerMapsRefusedAllocation(t *testing.T) {
	for rejected, want := range map[string]struct {
		status int
		code   string
	}{
		allocationRejectedConcurrency: {http.StatusTooManyRequests, "upstream_concurrency_exceeded"},
		allocationRejectedUnavailable: {http.StatusServiceUnavailable, "upstream_allocation_unavailable"},
	} {
		t.Run(rejected, func(t *testing.T) {
			ledger := NewAllocationLedger()
			// The body is whatever unmodified CPA renders; it is not consulted.
			transport := ledgerSidecar(t, 500, nil, `{"error":{"message":"x","type":"server_error","code":"internal_server_error"}}`, func(token string) {
				if err := ledger.Record("codex", AllocationReport{Token: token, UserID: ledgerUser, Rejected: rejected}); err != nil {
					t.Errorf("record: %v", err)
				}
			})
			result, failure := ledgerForward(t, ledger, transport, ForwardOptions{UserID: ledgerUser})
			if failure == nil || failure.Status != want.status || failure.Code != want.code || result.UpstreamAccountID != "" {
				t.Fatalf("failure = %+v, want %d %s", failure, want.status, want.code)
			}
		})
	}
	// Without a report an upstream 429 keeps meaning an upstream usage limit.
	ledger := NewAllocationLedger()
	_, failure := ledgerForward(t, ledger, ledgerSidecar(t, 429, nil, `{"error":{"code":"rate_limit_exceeded"}}`, nil), ForwardOptions{UserID: ledgerUser})
	if failure == nil || failure.Code != "upstream_rate_limited" {
		t.Fatalf("failure = %+v, want upstream_rate_limited", failure)
	}
}

func TestLedgerRecordRejectsForeignAndOutOfScopeReports(t *testing.T) {
	ledger := NewAllocationLedger()
	groups := []AllocationGroup{{GroupID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", AccountIDs: []string{ledgerAccount}}}
	opened, err := ledger.open("codex", ledgerUser, groups)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	report := AllocationReport{Token: opened.token, UserID: ledgerUser, AccountID: ledgerAccount}
	other := "22222222-2222-4222-8222-222222222222"
	for name, test := range map[string]struct {
		provider string
		mutate   func(*AllocationReport)
		want     error
	}{
		"another user":       {"codex", func(r *AllocationReport) { r.UserID = other }, ErrUnknownDispatch},
		"another provider":   {"anthropic", func(*AllocationReport) {}, ErrUnknownDispatch},
		"unknown token":      {"codex", func(r *AllocationReport) { r.Token = strings.Repeat("A", 43) }, ErrUnknownDispatch},
		"outside the pool":   {"codex", func(r *AllocationReport) { r.AccountID = ledgerOther }, ErrAccountOutsideScope},
		"malformed account":  {"codex", func(r *AllocationReport) { r.AccountID = "XYZ" }, ErrInvalidAllocationReport},
		"malformed hash":     {"codex", func(r *AllocationReport) { r.ConversationHash = "conv-1" }, ErrInvalidAllocationReport},
		"account and reject": {"codex", func(r *AllocationReport) { r.Rejected = allocationRejectedConcurrency }, ErrInvalidAllocationReport},
		"unknown rejection":  {"codex", func(r *AllocationReport) { r.AccountID, r.Rejected = "", "whatever" }, ErrInvalidAllocationReport},
		"neither":            {"codex", func(r *AllocationReport) { r.AccountID = "" }, ErrInvalidAllocationReport},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := report
			test.mutate(&candidate)
			if err := ledger.Record(test.provider, candidate); !errors.Is(err, test.want) {
				t.Fatalf("Record() = %v, want %v", err, test.want)
			}
		})
	}
	if got := opened.snapshot(); got.accountID != "" {
		t.Fatalf("a rejected report changed the dispatch: %+v", got)
	}
	if err = ledger.Record("codex", report); err != nil {
		t.Fatalf("the valid report was refused: %v", err)
	}
	// A personal layer leaves the pool to the user's own entitlement.
	personal, _ := ledger.open("codex", ledgerUser, append(groups, AllocationGroup{GroupID: PersonalLayerID, AllAccounts: true}))
	defer personal.close()
	if err = ledger.Record("codex", AllocationReport{Token: personal.token, UserID: ledgerUser, AccountID: ledgerOther}); err != nil {
		t.Fatalf("a personal layer restricted the pool: %v", err)
	}
	opened.close()
	if err = ledger.Record("codex", report); !errors.Is(err, ErrUnknownDispatch) {
		t.Fatalf("a finished dispatch accepted a report: %v", err)
	}
}

func TestPatchIntegrationKeepsHeaderAttribution(t *testing.T) {
	// No ledger: the source-patched image stamps the response and no request
	// token is sent.
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"protocol":"` + AccountAccessProtocolV1 + `"}`))}, nil
		}
		if r.Header.Get(dispatchHeader) != "" {
			t.Error("a request token was sent without the plugin integration")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, upstreamAccountHeader: {ledgerAccount}}, Body: io.NopCloser(strings.NewReader(ledgerBody))}, nil
	})
	result, failure := ledgerForward(t, nil, transport, ForwardOptions{UserID: ledgerUser})
	if failure != nil || result.UpstreamAccountID != ledgerAccount {
		t.Fatalf("header attribution was lost: %q, %v", result.UpstreamAccountID, failure)
	}
}
