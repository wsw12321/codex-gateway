package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type attributionInterruptedBody struct {
	first io.Reader
	err   error
}

func (b *attributionInterruptedBody) Read(p []byte) (int, error) {
	if b.first != nil {
		n, err := b.first.Read(p)
		if err != io.EOF {
			return n, err
		}
		b.first = nil
		if n > 0 {
			return n, nil
		}
	}
	return 0, b.err
}
func (*attributionInterruptedBody) Close() error { return nil }

// attributionCapabilities answers the capability probe of a dispatch.
func attributionCapabilities() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(
		`{"protocol":"` + AccountAccessProtocolV1 + `"}`))}
}

func TestForwardPreservesFinalAttributionForAllTerminalResults(t *testing.T) {
	const account = "fedcba9876543210"
	for _, test := range []struct {
		name    string
		status  int
		stream  bool
		readErr error
		account string
	}{
		{name: "ordinary success", status: 200, account: account},
		{name: "stream success", status: 200, stream: true, account: account},
		{name: "failed last attempt", status: 502, account: account},
		{name: "stream disconnect", status: 200, stream: true, readErr: io.ErrUnexpectedEOF, account: account},
		{name: "stream cancellation", status: 200, stream: true, readErr: context.Canceled, account: account},
		{name: "ordinary cancellation", status: 200, readErr: context.Canceled, account: account},
		{name: "no allocation reported", status: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := "application/json"
			payload := `{"model":"model","usage":{"input_tokens":1,"output_tokens":1}}`
			if test.stream {
				content = "text/event-stream"
				payload = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
			}
			var body io.ReadCloser = io.NopCloser(strings.NewReader(payload))
			if test.readErr != nil {
				body = &attributionInterruptedBody{first: strings.NewReader(payload), err: test.readErr}
			}
			ledger := NewAllocationLedger()
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/capabilities") {
					return attributionCapabilities(), nil
				}
				if r.Header.Get(retiredAccountHeader) != "" {
					t.Fatal("caller attribution forwarded")
				}
				if test.account != "" {
					reportAllocation(t, ledger, "codex", r, test.account, "")
				}
				// Whatever CPA renders in the response attributes nothing.
				return &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": {content}, retiredAccountHeader: {"0123456789abcdef"}}, Body: body}, nil
			})
			base, _ := url.Parse("http://compat.invalid")
			client := withManagement(NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}))
			client.UseAllocationLedger(ledger)
			request := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
			request.Header.Set(retiredAccountHeader, "0123456789abcdef")
			response := httptest.NewRecorder()
			result, failure := client.ForwardWithOptions(context.Background(), response, request, "/v1/responses", ForwardOptions{UserID: ledgerUser})
			if result.UpstreamAccountID != test.account {
				t.Fatalf("attribution=%q want %q", result.UpstreamAccountID, test.account)
			}
			if (failure != nil) != (test.status >= 400 || test.readErr != nil) {
				t.Fatalf("failure=%+v", failure)
			}
			if response.Header().Get(retiredAccountHeader) != "" {
				t.Fatal("internal attribution leaked to caller")
			}
		})
	}
}

// The removed source-patched image attributed a request through response
// headers. Nothing in a response may attribute one now, whatever its shape and
// whether or not the dispatch has a ledger entry.
func TestForwardNeverAttributesFromResponseHeaders(t *testing.T) {
	const account, hash = "fedcba9876543210", "conv-0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name          string
		accounts      []string
		conversations []string
	}{
		{name: "well-formed", accounts: []string{account}, conversations: []string{hash}},
		{name: "padded", accounts: []string{"  " + account + "  "}},
		{name: "path traversal", accounts: []string{"../../oauth-token"}},
		{name: "comma separated", accounts: []string{account + ",0123456789abcdef"}, conversations: []string{hash + "," + hash}},
		{name: "multiple values", accounts: []string{account, "0123456789abcdef"}, conversations: []string{hash, hash}},
		{name: "malformed hash", conversations: []string{"conv-0123456789abcdef0123456789abcde"}},
	} {
		for _, userID := range []string{"", ledgerUser} {
			t.Run(test.name+"/user="+userID, func(t *testing.T) {
				transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/capabilities") {
						return attributionCapabilities(), nil
					}
					if r.Header.Get(retiredAccountHeader) != "" || r.Header.Get(retiredConversationHeader) != "" {
						t.Error("caller-supplied attribution was forwarded")
					}
					header := http.Header{"Content-Type": {"application/json"}}
					header[retiredAccountHeader] = append([]string(nil), test.accounts...)
					header[retiredConversationHeader] = append([]string(nil), test.conversations...)
					return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"model":"model"}`))}, nil
				})
				base, _ := url.Parse("http://compat.invalid")
				client := withManagement(NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}))
				client.UseAllocationLedger(NewAllocationLedger())
				var notifications atomic.Int64
				request := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
				request.Header.Set(retiredAccountHeader, "0123456789abcdef")
				request.Header.Set(retiredConversationHeader, "conv-deadbeef")
				recorder := httptest.NewRecorder()
				result, failure := client.ForwardWithOptions(context.Background(), recorder, request, "/v1/responses", ForwardOptions{
					UserID:            userID,
					OnUpstreamAccount: func(string) { notifications.Add(1) },
					OnConversation:    func(string) { notifications.Add(1) },
				})
				if failure != nil {
					t.Fatalf("failure=%v", failure)
				}
				if result.UpstreamAccountID != "" || result.ConversationHash != "" || notifications.Load() != 0 {
					t.Fatalf("a response header attributed the request: %q %q notifications=%d", result.UpstreamAccountID, result.ConversationHash, notifications.Load())
				}
				if recorder.Header().Get(retiredAccountHeader) != "" || recorder.Header().Get(retiredConversationHeader) != "" {
					t.Fatal("an internal header leaked to the caller")
				}
			})
		}
	}
}
