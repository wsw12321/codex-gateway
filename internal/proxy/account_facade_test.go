package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestProviderAccountFacadeRejectsBadAuthWithoutFallback(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer sidecar.Close()
	base, _ := url.Parse(sidecar.URL)
	facade := NewAccountFacade(NewClientAccountAdapter("codex", New(base, "bad-token")))
	_, err := facade.ListAccounts(context.Background(), "codex")
	if err == nil {
		t.Fatal("bad sidecar auth unexpectedly succeeded")
	}
	apiErr, ok := err.(*InternalAPIError)
	if !ok || apiErr.Code != "sidecar_auth_failed" {
		t.Fatalf("bad auth error = %#v, want allowlisted reauthentication code", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/internal/upstream-accounts/capabilities") {
		t.Fatalf("bad auth dispatched unexpected paths: %v", paths)
	}
	if _, err := facade.ListAccounts(context.Background(), "missing"); err == nil {
		t.Fatal("unknown provider unexpectedly fell back to codex")
	}
}

func TestProviderAccountFacadeRejectsMalformedAndDuplicateAccountIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"accounts":[{"id":"not-an-id","masked_email":"a***@example.com","plan":"pro","status":"available","last_synced_at":"2026-10-30T19:00:00Z"}]}`},
		{name: "duplicate", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"a***@example.com","plan":"pro","status":"available","last_synced_at":"2026-10-30T19:00:00Z"},{"id":"0123456789abcdef","masked_email":"b***@example.com","plan":"pro","status":"available","last_synced_at":"2026-10-30T19:00:00Z"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/internal/upstream-accounts/capabilities":
					io.WriteString(w, `{"protocol":"upstream_account_access_v1"}`)
				case "/internal/upstream-accounts":
					io.WriteString(w, tc.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer sidecar.Close()
			base, _ := url.Parse(sidecar.URL)
			facade := NewAccountFacade(NewClientAccountAdapter("codex", New(base, "token")))
			_, err := facade.ListAccounts(context.Background(), "codex")
			if err == nil {
				t.Fatal("invalid account response unexpectedly succeeded")
			}
			apiErr, ok := err.(*InternalAPIError)
			if !ok || apiErr.Code != "sidecar_invalid_response" {
				t.Fatalf("invalid account error = %#v", err)
			}
		})
	}
}

func TestProviderAccountFacadeRejectsStaleCapabilityBeforeQuota(t *testing.T) {
	var quotaCalls int
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/internal/upstream-accounts/capabilities" {
			io.WriteString(w, `{"protocol":"old"}`)
			return
		}
		quotaCalls++
		io.WriteString(w, `{}`)
	}))
	defer sidecar.Close()
	base, _ := url.Parse(sidecar.URL)
	facade := NewAccountFacade(NewClientAccountAdapter("codex", New(base, "token")))
	_, err := facade.Quota(context.Background(), "codex", "0123456789abcdef")
	apiErr, ok := err.(*InternalAPIError)
	if !ok || apiErr.Code != "upstream_access_protocol_unavailable" || quotaCalls != 0 {
		t.Fatalf("stale capability result = %#v quota_calls=%d", err, quotaCalls)
	}
}

func TestProviderAccountFacadeRejectsQuotaSchemaDuplicates(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/upstream-accounts/capabilities":
			io.WriteString(w, `{"protocol":"upstream_account_access_v1"}`)
		case "/internal/upstream-accounts/0123456789abcdef/quota":
			io.WriteString(w, `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":null},"codex":{"limitId":"codex","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer sidecar.Close()
	base, _ := url.Parse(sidecar.URL)
	facade := NewAccountFacade(NewClientAccountAdapter("codex", New(base, "token")))
	_, err := facade.Quota(context.Background(), "codex", "0123456789abcdef")
	apiErr, ok := err.(*InternalAPIError)
	if !ok || apiErr.Code != "sidecar_invalid_response" {
		t.Fatalf("duplicate quota schema error = %#v", err)
	}
}

func TestProviderAccountFacadeKeepsProviderPathsIsolated(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"protocol":"upstream_account_access_v1","anthropic_messages":"anthropic_messages_v1"}`)
	}))
	defer sidecar.Close()
	base, _ := url.Parse(sidecar.URL)
	facade := NewAccountFacade(
		NewClientAccountAdapter("codex", New(base, "token")),
		NewClientAccountAdapter("anthropic", NewCPAAnthropic(base, "token")),
	)
	if _, err := facade.Capabilities(context.Background(), "codex"); err != nil {
		t.Fatalf("codex capabilities: %v", err)
	}
	if _, err := facade.Capabilities(context.Background(), "anthropic"); err != nil {
		t.Fatalf("anthropic capabilities: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/internal/upstream-accounts/capabilities" || paths[1] != "/internal/anthropic-accounts/capabilities" {
		t.Fatalf("provider capability paths crossed: %v", paths)
	}
}
