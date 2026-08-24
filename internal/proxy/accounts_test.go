package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestInternalAccountAPIIsNarrowAndNormalized(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sidecar-secret" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/upstream-accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{{
				"id": "0123456789abcdef", "masked_email": "u***@example.com", "plan": "plus",
				"status": "active", "last_synced_at": now,
			}}})
		case "/internal/upstream-accounts/0123456789abcdef/quota":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"queried_at": now, "plan": "plus",
				"five_hour":          map[string]any{"used_ratio": 0.4, "remaining_ratio": 0.6, "reset_at": now.Add(time.Hour)},
				"seven_day":          map[string]any{"used_ratio": 0.25, "remaining_ratio": 0.75, "reset_at": now.Add(24 * time.Hour)},
				"additional_windows": []map[string]any{{"name": "codex_other", "used_ratio": 0.1, "remaining_ratio": 0.9, "reset_at": now.Add(30 * time.Minute), "window_seconds": 1800}},
			})
		default:
			t.Errorf("unexpected internal path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewWithHTTPClient(base, "sidecar-secret", server.Client())

	accounts, err := client.ListUpstreamAccounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].MaskedEmail != "u***@example.com" {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	quota, err := client.QueryUpstreamAccountQuota(context.Background(), accounts[0].ID)
	if err != nil || quota.Plan != "plus" || quota.FiveHour.UsedRatio != 0.4 ||
		len(quota.AdditionalWindows) != 1 || quota.AdditionalWindows[0].Name != "additional_1" {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
}

func TestInternalAccountAPIRejectsLeaksAndSchemaDrift(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "full email", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"user@example.com","plan":"plus","status":"active","last_synced_at":"2026-08-24T12:00:00Z"}]}`},
		{name: "literal asterisk email", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"alice*tag@example.com","plan":"plus","status":"active","last_synced_at":"2026-08-24T12:00:00Z"}]}`},
		{name: "too short masked email", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"a***@x","plan":"plus","status":"active","last_synced_at":"2026-08-24T12:00:00Z"}]}`},
		{name: "padded masked email", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":" a***@example.com ","plan":"plus","status":"active","last_synced_at":"2026-08-24T12:00:00Z"}]}`},
		{name: "secret-shaped plan", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"u***@example.com","plan":"refresh_token_deadbeef","status":"active","last_synced_at":"2026-08-24T12:00:00Z"}]}`},
		{name: "token field", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"u***@example.com","plan":"plus","status":"active","last_synced_at":"2026-08-24T12:00:00Z","access_token":"secret"}]}`},
		{name: "duplicate id", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"u***@example.com","plan":"plus","status":"active","last_synced_at":"2026-08-24T12:00:00Z"},{"id":"0123456789abcdef","masked_email":"v***@example.com","plan":"pro","status":"active","last_synced_at":"2026-08-24T12:00:00Z"}]}`},
		{name: "missing accounts", body: `{}`},
		{name: "null accounts", body: `{"accounts":null}`},
		{name: "missing account field", body: `{"accounts":[{"id":"0123456789abcdef","masked_email":"u***@example.com","plan":"plus","status":"active"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			_, err := NewWithHTTPClient(base, "secret", server.Client()).ListUpstreamAccounts(context.Background())
			var internalErr *InternalAPIError
			if !errors.As(err, &internalErr) || internalErr.SafeCode() != "sidecar_invalid_response" || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestInternalQuotaRejectsMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "missing used ratio", body: `{"queried_at":"2026-08-24T12:00:00Z","plan":"plus","five_hour":{"remaining_ratio":1,"reset_at":"2026-08-24T17:00:00Z"},"seven_day":{"used_ratio":0.2,"remaining_ratio":0.8,"reset_at":"2026-08-31T12:00:00Z"}}`},
		{name: "missing remaining ratio", body: `{"queried_at":"2026-08-24T12:00:00Z","plan":"plus","five_hour":{"used_ratio":0,"reset_at":"2026-08-24T17:00:00Z"},"seven_day":{"used_ratio":0.2,"remaining_ratio":0.8,"reset_at":"2026-08-31T12:00:00Z"}}`},
		{name: "unknown quota plan", body: `{"queried_at":"2026-08-24T12:00:00Z","plan":"unknown","five_hour":{"used_ratio":0.4,"remaining_ratio":0.6,"reset_at":"2026-08-24T17:00:00Z"},"seven_day":{"used_ratio":0.2,"remaining_ratio":0.8,"reset_at":"2026-08-31T12:00:00Z"}}`},
		{name: "missing additional name", body: `{"queried_at":"2026-08-24T12:00:00Z","plan":"plus","five_hour":{"used_ratio":0.4,"remaining_ratio":0.6,"reset_at":"2026-08-24T17:00:00Z"},"seven_day":{"used_ratio":0.2,"remaining_ratio":0.8,"reset_at":"2026-08-31T12:00:00Z"},"additional_windows":[{"used_ratio":0.1,"remaining_ratio":0.9,"reset_at":"2026-08-24T12:30:00Z"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			_, err := NewWithHTTPClient(base, "secret", server.Client()).QueryUpstreamAccountQuota(context.Background(), "0123456789abcdef")
			var internalErr *InternalAPIError
			if !errors.As(err, &internalErr) || internalErr.SafeCode() != "sidecar_invalid_response" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestInternalQuotaRejectsArbitraryAccountPathAndSanitizesFailures(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"access_token":"must-not-be-parsed"}`))
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewWithHTTPClient(base, "secret", server.Client())

	if _, err := client.QueryUpstreamAccountQuota(context.Background(), "../../metadata"); err == nil || calls != 0 {
		t.Fatalf("invalid id error=%v calls=%d", err, calls)
	}
	_, err := client.QueryUpstreamAccountQuota(context.Background(), "0123456789abcdef")
	var internalErr *InternalAPIError
	if !errors.As(err, &internalErr) || internalErr.SafeCode() != "upstream_quota_rate_limited" || internalErr.RetryAfter != 7 {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), "access_token") || strings.Contains(err.Error(), "must-not-be-parsed") {
		t.Fatalf("sensitive response leaked through error: %v", err)
	}
}

func TestInternalQuotaMapsReauthenticationAndTimeouts(t *testing.T) {
	base, _ := url.Parse("http://sidecar.internal")
	for _, test := range []struct {
		name       string
		transport  http.RoundTripper
		wantCode   string
		wantStatus int
	}{
		{name: "unauthorized", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"token":"secret"}`))}, nil
		}), wantCode: "upstream_reauthentication_required", wantStatus: http.StatusUnauthorized},
		{name: "forbidden", transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"token":"secret"}`))}, nil
		}), wantCode: "upstream_reauthentication_required", wantStatus: http.StatusForbidden},
		{name: "context timeout", transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}), wantCode: "sidecar_timeout", wantStatus: http.StatusGatewayTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.name == "context timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
				defer cancel()
			}
			client := NewWithHTTPClient(base, "secret", &http.Client{Transport: test.transport})
			_, err := client.QueryUpstreamAccountQuota(ctx, "0123456789abcdef")
			var internalErr *InternalAPIError
			if !errors.As(err, &internalErr) || internalErr.SafeCode() != test.wantCode || internalErr.StatusCode != test.wantStatus {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("sensitive response leaked through error: %v", err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestInternalAccountAPIRejectsOversizedAndSensitiveMalformedJSON(t *testing.T) {
	t.Run("oversized trailing data", func(t *testing.T) {
		body := `{"accounts":[]}` + strings.Repeat(" ", maxInternalResponseBodyBytes)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()
		base, _ := url.Parse(server.URL)

		_, err := NewWithHTTPClient(base, "secret", server.Client()).ListUpstreamAccounts(context.Background())
		var internalErr *InternalAPIError
		if !errors.As(err, &internalErr) || internalErr.SafeCode() != "sidecar_invalid_response" {
			t.Fatalf("error = %#v", err)
		}
	})

	t.Run("malformed time value is not logged", func(t *testing.T) {
		const sensitive = "Bearer top-secret-access-token"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accounts":[{"id":"0123456789abcdef","masked_email":"u***@example.com","plan":"plus","status":"active","last_synced_at":"` + sensitive + `"}]}`))
		}))
		defer server.Close()
		base, _ := url.Parse(server.URL)

		_, err := NewWithHTTPClient(base, "secret", server.Client()).ListUpstreamAccounts(context.Background())
		var internalErr *InternalAPIError
		if !errors.As(err, &internalErr) {
			t.Fatalf("error = %#v", err)
		}
		if got := err.Error(); got != "sidecar_invalid_response" || strings.Contains(got, sensitive) {
			t.Fatalf("unsafe error text = %q", got)
		}
		if debug := fmt.Sprintf("%#v", internalErr); strings.Contains(debug, sensitive) {
			t.Fatalf("unsafe debug formatting = %q", debug)
		}
		if !errors.Is(err, internalErr.Cause) {
			t.Fatal("sanitizing error text removed the diagnostic unwrap chain")
		}
	})
}
