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

func TestAccountAccessCapabilityAndAuthenticatedIdentity(t *testing.T) {
	const userID = "00000000-0000-0000-0000-000000000001"
	for _, protocol := range []string{"upstream_account_access_v1", "old", ""} {
		t.Run(protocol, func(t *testing.T) {
			requests := 0
			sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("internal authentication missing")
				}
				if r.URL.Path == "/internal/upstream-accounts/capabilities" {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"protocol":"`+protocol+`"}`)
					return
				}
				requests++
				if r.Header.Get(gatewayUserHeader) != userID {
					t.Errorf("untrusted user identity: %s", r.Header.Get(gatewayUserHeader))
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"ok"}`)
			}))
			defer sidecar.Close()
			base, _ := url.Parse(sidecar.URL)
			client := New(base, "test-key")
			incoming := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
			incoming.Header.Set(gatewayUserHeader, "forged-user")
			_, failure := client.ForwardWithOptions(context.Background(), httptest.NewRecorder(), incoming, "/v1/responses", ForwardOptions{UserID: userID})
			if protocol == "upstream_account_access_v1" {
				if failure != nil || requests != 1 {
					t.Fatalf("valid dispatch failed: %v requests=%d", failure, requests)
				}
			} else if failure == nil || failure.Code != "upstream_access_protocol_unavailable" || requests != 0 {
				t.Fatalf("old sidecar accepted payload: %v requests=%d", failure, requests)
			}
		})
	}
}

func TestForgedGatewayUserHeaderNeverForwardedWithoutIdentity(t *testing.T) {
	client := New(&url.URL{Scheme: "http", Host: "example.test"}, "test")
	out := make(http.Header)
	client.copyAllowedHeaders(out, http.Header{gatewayUserHeader: {"forged"}})
	if out.Get(gatewayUserHeader) != "" {
		t.Fatal("forwarded untrusted internal header")
	}
}
