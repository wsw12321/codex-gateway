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
			if r.Method != http.MethodGet {
				t.Errorf("account method = %q", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{{
				"id": "0123456789abcdef", "masked_email": "u***@example.com", "plan": "plus",
				"status": "active", "last_synced_at": now,
			}}})
		case "/internal/upstream-accounts/0123456789abcdef/quota":
			if r.Method != http.MethodPost {
				t.Errorf("quota method = %q", r.Method)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("content type = %q", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(body); got != `{"method":"account/rateLimits/read","id":6}` {
				t.Errorf("quota body = %q", got)
			}
			_, _ = io.WriteString(w, `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null},"codex_other":{"limitId":"codex_other","limitName":null,"primary":{"usedPercent":10,"windowDurationMins":null,"resetsAt":null},"secondary":null,"rateLimitReachedType":"rate_limit_reached"}},"rateLimitResetCredits":null}}`)
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
	other := quota.Result.RateLimitsByLimitID["codex_other"]
	if err != nil || quota.ID != AccountRateLimitsRequestID || quota.Result.RateLimits.Primary == nil ||
		quota.Result.RateLimits.Primary.UsedPercent != 25 || quota.Result.RateLimits.Secondary != nil ||
		len(quota.Result.RateLimitsByLimitID) != 2 || other.Primary == nil || other.Primary.WindowDurationMins != nil ||
		other.RateLimitReachedType == nil || *other.RateLimitReachedType != "rate_limit_reached" ||
		quota.Result.RateLimitResetCredits != nil {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
	encoded, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"queried_at", "plan", "five_hour", "seven_day", "remaining_ratio"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("legacy or sensitive field %q in normalized response: %s", forbidden, encoded)
		}
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

func TestInternalQuotaRejectsInvalidRPCResponses(t *testing.T) {
	validCodex := `{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null}`
	tests := []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "wrong response id", body: `{"id":5,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `},"rateLimitResetCredits":null}}`},
		{name: "string response id", body: `{"id":"6","result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `},"rateLimitResetCredits":null}}`},
		{name: "missing result", body: `{"id":6}`},
		{name: "missing rate limits", body: `{"id":6,"result":{"rateLimitsByLimitId":{"codex":` + validCodex + `},"rateLimitResetCredits":null}}`},
		{name: "null bucket map", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":null,"rateLimitResetCredits":null}}`},
		{name: "empty bucket map", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{},"rateLimitResetCredits":null}}`},
		{name: "missing codex bucket", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex_other":{"limitId":"codex_other","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "non-null reset credits", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `},"rateLimitResetCredits":{}}}`},
		{name: "missing limit name", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","primary":null,"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "upstream limit name", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":"Bearer-secret","primary":null,"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":"Bearer-secret","primary":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "missing primary", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "missing used percent", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "percent above 100", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":101,"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":101,"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "fractional percent", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25.5,"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25.5,"windowDurationMins":15,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "zero duration", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":0,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":0,"resetsAt":1730947200},"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "timestamp before lower bound", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":15,"resetsAt":1},"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":15,"resetsAt":1},"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "unknown reached type", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":"Bearer-secret"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":"Bearer-secret"}},"rateLimitResetCredits":null}}`},
		{name: "invalid bucket id", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `,"../../token":{"limitId":"../../token","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "map and snapshot id mismatch", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `,"codex_other":{"limitId":"different","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "single bucket mismatch", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":null,"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`},
		{name: "duplicate bucket id", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `,"codex":` + validCodex + `},"rateLimitResetCredits":null}}`},
		{name: "unknown sensitive field", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `},"rateLimitResetCredits":null,"access_token":"Bearer-secret"}}`},
		{name: "unknown window field", body: `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":25,"windowDurationMins":15,"resetsAt":1730947200,"token":"Bearer-secret"},"secondary":null,"rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":` + validCodex + `},"rateLimitResetCredits":null}}`},
		{name: "multiple JSON values", body: `{"id":6,"result":{"rateLimits":` + validCodex + `,"rateLimitsByLimitId":{"codex":` + validCodex + `},"rateLimitResetCredits":null}} {"access_token":"Bearer-secret"}`},
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
			if !errors.As(err, &internalErr) || internalErr.SafeCode() != "sidecar_invalid_response" ||
				strings.Contains(err.Error(), "Bearer-secret") || strings.Contains(fmt.Sprintf("%#v", internalErr), "Bearer-secret") {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestInternalQuotaAcceptsDynamicWindowsAndNullableMetadata(t *testing.T) {
	const body = `{"id":6,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":40,"windowDurationMins":300,"resetsAt":1730947200},"secondary":{"usedPercent":20,"windowDurationMins":10080,"resetsAt":1731552000},"rateLimitReachedType":"workspace_member_usage_limit_reached"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":40,"windowDurationMins":300,"resetsAt":1730947200},"secondary":{"usedPercent":20,"windowDurationMins":10080,"resetsAt":1731552000},"rateLimitReachedType":"workspace_member_usage_limit_reached"},"codex_other":{"limitId":"codex_other","limitName":null,"primary":{"usedPercent":0},"secondary":null,"rateLimitReachedType":null}},"rateLimitResetCredits":null}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)

	quota, err := NewWithHTTPClient(base, "secret", server.Client()).QueryUpstreamAccountQuota(context.Background(), "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if quota.Result.RateLimits.Primary == nil || quota.Result.RateLimits.Secondary == nil ||
		quota.Result.RateLimits.Primary.WindowDurationMins == nil ||
		quota.Result.RateLimits.Secondary.WindowDurationMins == nil ||
		*quota.Result.RateLimits.Primary.WindowDurationMins != 300 ||
		*quota.Result.RateLimits.Secondary.WindowDurationMins != 10080 {
		t.Fatalf("fixed windows were not preserved: %+v", quota.Result.RateLimits)
	}
	other := quota.Result.RateLimitsByLimitID["codex_other"]
	if other.Primary == nil || other.Primary.WindowDurationMins != nil || other.Primary.ResetsAt != nil || other.Secondary != nil {
		t.Fatalf("nullable window metadata was not preserved: %+v", other)
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

	t.Run("oversized quota response", func(t *testing.T) {
		body := `{"id":6}` + strings.Repeat(" ", maxInternalResponseBodyBytes)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()
		base, _ := url.Parse(server.URL)

		_, err := NewWithHTTPClient(base, "secret", server.Client()).QueryUpstreamAccountQuota(context.Background(), "0123456789abcdef")
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
