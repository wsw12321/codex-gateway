package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestForwardAcceptsTrustedConversationHashWithoutForwardingOrLeakingIt(t *testing.T) {
	const hash = "conv-0123456789abcdef0123456789abcdef"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get(conversationHashHeader); got != "" {
			t.Fatalf("conversation hash forwarded from caller: %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}, conversationHashHeader: {hash}},
			Body:       io.NopCloser(strings.NewReader(`{"model":"model"}`)),
		}, nil
	})
	base, _ := url.Parse("http://compat.invalid")
	client := NewWithHTTPClient(base, "secret", &http.Client{Transport: transport})
	request := httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`))
	request.Header.Set(conversationHashHeader, "conv-deadbeef")
	var notified string
	recorder := httptest.NewRecorder()
	result, failure := client.ForwardWithOptions(context.Background(), recorder, request, "/v1/responses", ForwardOptions{
		OnConversation: func(value string) { notified = value },
	})
	if failure != nil {
		t.Fatalf("failure=%v", failure)
	}
	if result.ConversationHash != hash || notified != hash {
		t.Fatalf("conversation hash result=%q callback=%q want %q", result.ConversationHash, notified, hash)
	}
	if got := recorder.Header().Get(conversationHashHeader); got != "" {
		t.Fatalf("conversation hash leaked to caller: %q", got)
	}
}

func TestForwardRejectsAmbiguousOrMalformedConversationHash(t *testing.T) {
	for _, values := range [][]string{
		{"conv-0123456789abcdef0123456789abcde"},
		{"conv-0123456789abcdef0123456789abcdef", "conv-0123456789abcdef0123456789abcdef"},
		{"conv-0123456789abcdef0123456789abcdef,conv-0123456789abcdef0123456789abcdef"},
	} {
		t.Run(strings.Join(values, ","), func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}, conversationHashHeader: values},
					Body:       io.NopCloser(strings.NewReader(`{"model":"model"}`)),
				}, nil
			})
			base, _ := url.Parse("http://compat.invalid")
			client := NewWithHTTPClient(base, "secret", &http.Client{Transport: transport})
			result, failure := client.Forward(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://gateway.invalid/v1/responses", strings.NewReader(`{}`)), "/v1/responses")
			if failure != nil {
				t.Fatalf("failure=%v", failure)
			}
			if result.ConversationHash != "" {
				t.Fatalf("conversation hash=%q, want empty", result.ConversationHash)
			}
		})
	}
}
