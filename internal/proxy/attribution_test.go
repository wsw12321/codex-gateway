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
		{name: "unknown historical attribution", status: 200},
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
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get(upstreamAccountHeader) != "" {
					t.Fatal("caller attribution forwarded")
				}
				header := http.Header{"Content-Type": {content}}
				if test.account != "" {
					header.Set(upstreamAccountHeader, test.account)
				}
				return &http.Response{StatusCode: test.status, Header: header, Body: body}, nil
			})
			base, _ := url.Parse("http://compat.invalid")
			client := NewWithHTTPClient(base, "secret", &http.Client{Transport: transport})
			request := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
			request.Header.Set(upstreamAccountHeader, "0123456789abcdef")
			response := httptest.NewRecorder()
			result, failure := client.Forward(context.Background(), response, request, "/v1/responses")
			if result.UpstreamAccountID != test.account {
				t.Fatalf("attribution=%q want %q", result.UpstreamAccountID, test.account)
			}
			if (failure != nil) != (test.status >= 400 || test.readErr != nil) {
				t.Fatalf("failure=%+v", failure)
			}
			if response.Header().Get(upstreamAccountHeader) != "" {
				t.Fatal("internal attribution leaked to caller")
			}
		})
	}
}

func TestForwardNotifiesOnlyForValidSingleValuedAttributionHeader(t *testing.T) {
	const valid = "fedcba9876543210"
	for _, test := range []struct {
		name       string
		header     []string
		wantID     string
		wantNotify string
	}{
		{name: "valid", header: []string{valid}, wantID: valid, wantNotify: valid},
		{name: "trimmed valid", header: []string{"  " + valid + "  "}, wantID: valid, wantNotify: valid},
		{name: "invalid characters", header: []string{"FEDCBA9876543210"}},
		{name: "wrong length", header: []string{"0123456789abcde"}},
		{name: "comma separated", header: []string{valid + ",0123456789abcdef"}},
		{name: "multiple values", header: []string{valid, "0123456789abcdef"}},
		{name: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": {"application/json"}}
				if test.header != nil {
					header[upstreamAccountHeader] = append([]string(nil), test.header...)
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: header,
					Body: io.NopCloser(strings.NewReader(`{"model":"model"}`)),
				}, nil
			})
			base, _ := url.Parse("http://compat.invalid")
			client := NewWithHTTPClient(base, "secret", &http.Client{Transport: transport})
			var notifications atomic.Int64
			var notified atomic.Value
			request := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
			result, failure := client.ForwardWithOptions(context.Background(), httptest.NewRecorder(), request, "/v1/responses", ForwardOptions{
				OnUpstreamAccount: func(accountID string) {
					notifications.Add(1)
					notified.Store(accountID)
				},
			})
			if failure != nil {
				t.Fatalf("failure=%v", failure)
			}
			if result.UpstreamAccountID != test.wantID {
				t.Fatalf("result attribution=%q want %q", result.UpstreamAccountID, test.wantID)
			}
			wantCount := int64(0)
			if test.wantNotify != "" {
				wantCount = 1
			}
			if notifications.Load() != wantCount {
				t.Fatalf("notifications=%d want %d", notifications.Load(), wantCount)
			}
			if test.wantNotify != "" && notified.Load() != test.wantNotify {
				t.Fatalf("notified=%v want %q", notified.Load(), test.wantNotify)
			}
		})
	}
}
