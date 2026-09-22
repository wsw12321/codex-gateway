package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestUpstreamAccountConcurrencyAuthenticatedSnapshot(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/upstream-accounts/concurrency" || r.Header.Get("Authorization") != "Bearer internal-secret" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(UpstreamConcurrency{SampledAt: now, Accounts: []UpstreamAccountConcurrency{{ID: "0123456789abcdef", ActiveRequests: 0}, {ID: "fedcba9876543210", ActiveRequests: 3}}})
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	result, err := NewWithHTTPClient(base, "internal-secret", server.Client()).UpstreamAccountConcurrency(context.Background())
	if err != nil || !result.SampledAt.Equal(now) || len(result.Accounts) != 2 || result.Accounts[0].ActiveRequests != 0 || result.Accounts[1].ActiveRequests != 3 {
		t.Fatalf("snapshot=%+v err=%v", result, err)
	}
}

func TestUpstreamAccountConcurrencyRejectsMissingStaleOrUnsafeValues(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, body := range []string{
		`{}`, `{"sampled_at":"` + now + `"}`, `{"sampled_at":"` + now + `","accounts":null}`,
		`{"sampled_at":"2000-01-01T00:00:00Z","accounts":[]}`,
		`{"sampled_at":"2099-01-01T00:00:00Z","accounts":[]}`,
		`{"sampled_at":"` + now + `","accounts":[{"id":"0123456789abcdef"}]}`,
		`{"sampled_at":"` + now + `","accounts":[{"id":"0123456789abcdef","active_requests":null}]}`,
		`{"sampled_at":"` + now + `","accounts":[{"id":"0123456789abcdef","active_requests":-1}]}`,
		`{"sampled_at":"` + now + `","accounts":[{"id":"0123456789abcdef","active_requests":1.5}]}`,
		`{"sampled_at":"` + now + `","accounts":[{"id":"secret@example.com","active_requests":0}]}`,
		`{"sampled_at":"` + now + `","accounts":[{"id":"0123456789abcdef","active_requests":0,"token":"secret"}]}`,
		`{"sampled_at":"` + now + `","accounts":[{"id":"0123456789abcdef","active_requests":0},{"id":"0123456789abcdef","active_requests":1}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			result, err := NewWithHTTPClient(base, "secret", server.Client()).UpstreamAccountConcurrency(context.Background())
			if err == nil || len(result.Accounts) != 0 || strings.Contains(err.Error(), "example.com") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestUpstreamAccountConcurrencyOldCompatUnavailable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	base, _ := url.Parse(server.URL)
	result, err := NewWithHTTPClient(base, "secret", server.Client()).UpstreamAccountConcurrency(context.Background())
	if err == nil || result.Accounts != nil {
		t.Fatalf("old sidecar falsely produced zero: %+v %v", result, err)
	}
}
