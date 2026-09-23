package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestSetUpstreamAccountStatusUsesConfirmedAuthenticatedProtocol(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			wantStatus := "unavailable"
			if enabled {
				wantStatus = "available"
			}
			sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPut || r.URL.Path != "/internal/upstream-accounts/0123456789abcdef/status" ||
					r.Header.Get("Authorization") != "Bearer sidecar-secret" || r.Header.Get("Content-Type") != "application/json" ||
					string(body) != `{"enabled":`+strconv.FormatBool(enabled)+`}` {
					t.Errorf("unexpected internal request: %s %s %s", r.Method, r.URL.Path, body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"0123456789abcdef","status":"`+wantStatus+`"}`)
			}))
			defer sidecar.Close()
			base, _ := url.Parse(sidecar.URL)
			result, err := NewWithHTTPClient(base, "sidecar-secret", sidecar.Client()).SetUpstreamAccountStatus(context.Background(), "0123456789abcdef", enabled)
			if err != nil || result.ID != "0123456789abcdef" || result.Status != wantStatus {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestSetUpstreamAccountStatusAcceptsSplitStatusResponse(t *testing.T) {
	for _, test := range []struct {
		enabled bool
		body    string
		manual  string
		final   string
	}{
		{enabled: false, body: `{"id":"0123456789abcdef","status":"unavailable","cliproxy_status":"active","gateway_manual_status":"manual_disabled","gateway_quota_status":"available"}`, manual: "manual_disabled", final: "unavailable"},
		{enabled: true, body: `{"id":"0123456789abcdef","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available"}`, manual: "enabled", final: "available"},
	} {
		t.Run(strconv.FormatBool(test.enabled), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			result, err := NewWithHTTPClient(base, "sidecar-secret", server.Client()).SetUpstreamAccountStatus(context.Background(), "0123456789abcdef", test.enabled)
			if err != nil || result.Status != test.final || result.CliproxyStatus != "active" || result.GatewayManualStatus != test.manual || result.GatewayQuotaStatus != "available" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestSetUpstreamAccountStatusRejectsUnconfirmedOrLeakingResponse(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"id":"0123456789abcdef"}`, `{"status":"available"}`,
		`{"id":"fedcba9876543210","status":"available"}`,
		`{"id":"0123456789abcdef","status":"unavailable"}`,
		`{"id":"0123456789abcdef","status":"active"}`,
		`{"id":"0123456789abcdef","status":null}`,
		`{"ID":"0123456789abcdef","status":"available"}`,
		`{"id":"0123456789abcdef","Status":"available"}`,
		`{"id":"0123456789abcdef","status":"available","token":"sensitive-canary"}`,
		`{"id":"0123456789abcdef","status":"unavailable","status":"available"}`,
		`{"id":"0123456789abcdef","status":"available"}{}`,
	} {
		t.Run(body, func(t *testing.T) {
			base, _ := url.Parse("http://sidecar.internal")
			client := NewWithHTTPClient(base, "secret", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})})
			_, err := client.SetUpstreamAccountStatus(context.Background(), "0123456789abcdef", true)
			var internalErr *InternalAPIError
			if !errors.As(err, &internalErr) || internalErr.Code != "sidecar_invalid_response" || strings.Contains(err.Error(), "sensitive-canary") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestSetUpstreamAccountStatusRejectsUnsafeAccountIDBeforeNetwork(t *testing.T) {
	for _, id := range []string{"", "../secret", "0123456789ABCDEF", "0123456789abcdef?token=secret", "0123456789abcdef0"} {
		_, err := (&Client{}).SetUpstreamAccountStatus(context.Background(), id, true)
		var internalErr *InternalAPIError
		if !errors.As(err, &internalErr) || internalErr.Code != "invalid_upstream_account" {
			t.Fatalf("id=%q error=%v", id, err)
		}
	}
}
