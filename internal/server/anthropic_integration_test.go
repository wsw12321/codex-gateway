//go:build integration

package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestAnthropicMessagesAndCountTokensPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	s, repo, ctx := f.s, f.s.store, context.Background()
	h := newResponsesWebSocketTestHarness(t)
	s.config.KeyPepper = h.server.config.KeyPepper
	s.config.BodyLimit = 64 << 20
	s.config.Limits = config.Limits{KeyRPM: 100, UserRPM: 100, KeyConcurrent: 3, UserConcurrent: 3, GlobalConcurrent: 10, KeyRequestsPerDay: 100}
	const model = "claude-test"
	const account = "1122334455667788"
	if err := repo.SyncModelAccessCatalog(ctx, []string{model}); err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(`{"schema_version":2,"catalog_as_of":"2026-10-08","fx_as_of":"2026-10-08","usd_cny_rate":"7","fallback_policy":{"unknown_service_tier":"max_published","missing_price_combination":"max_published","missing_cache_write_tokens":"all_uncached_as_write"},"models":{"claude-test":{"cache_write_mode":"separate_by_ttl","max_input_tokens":200000,"long_context_threshold_tokens":200000,"service_tiers":{"standard":{"short":{"input_usd_per_million":"3","cached_input_usd_per_million":"0.3","cache_write_5m_usd_per_million":"3.75","cache_write_1h_usd_per_million":"6","output_usd_per_million":"15"}}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	s.config.UsagePricing = pricing
	device, err := repo.CreateDevice(ctx, store.CreateDeviceParams{UserID: f.owner.ID, Name: "Claude"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateAPIKey(ctx, store.CreateAPIKeyParams{UserID: f.owner.ID, DeviceID: device.ID, Name: "Claude", PublicID: h.database.publicID, KeyPrefix: h.database.keyPrefix, KeyHash: h.database.keyHash, SecretCiphertext: h.database.secretCiphertext, ModelAllowlist: []string{model}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var interruptStream atomic.Bool
	var upstreamCalls atomic.Int64
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer internal-cpa" {
			t.Error("missing internal credentials")
		}
		switch r.URL.Path {
		case "/internal/anthropic-accounts":
			writeJSON(w, 200, map[string]any{"accounts": []gatewayproxy.UpstreamAccount{{ID: account, DisplayName: "claude", MaskedEmail: "c***@example.com", Plan: "unknown", Status: "available", CliproxyStatus: "active", GatewayManualStatus: "enabled", GatewayQuotaStatus: "available", LastSyncedAt: time.Now().UTC()}}})
			return
		case "/internal/anthropic-accounts/capabilities":
			writeJSON(w, 200, map[string]string{"protocol": "upstream_account_access_v1", "anthropic_messages": "anthropic_messages_v1"})
			return
		}
		if r.Header.Get("X-Codex-Gateway-User") != f.owner.ID || r.Header.Get("X-Codex-Gateway-Affinity") != upstreamAffinityScope(s.config.KeyPepper, key.ID) {
			t.Error("identity missing")
		}
		eligible, err := repo.WithUpstreamProvider(store.UpstreamProviderAnthropic).EligibleUpstreamAccountLimits(ctx, f.owner.ID, []string{account})
		if err != nil {
			t.Error(err)
		}
		if len(eligible) == 0 {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"type":"error","error":{"type":"permission_error","message":"No eligible Claude account"}}`)
			return
		}
		w.Header().Set("X-Codex-Upstream-Account", account)
		if r.URL.Path == "/v1/messages/count_tokens" {
			fmt.Fprint(w, `{"input_tokens":60}`)
			return
		}
		data, _ := io.ReadAll(r.Body)
		usage := `{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":12,"ephemeral_1h_input_tokens":18},"output_tokens":7}`
		if strings.Contains(string(data), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":%s}}\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n", usage)
			if !interruptStream.Load() {
				fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}
			return
		}
		fmt.Fprintf(w, `{"type":"message","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":%s}`, usage)
	}))
	defer cpa.Close()
	base, _ := url.Parse(cpa.URL)
	s.anthropic = gatewayproxy.NewCPAAnthropic(base, "internal-cpa")
	send := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("X-Api-Key", h.apiKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	// Counting requires model/account access and request quota, even without funds.
	w := send("/v1/messages/count_tokens?beta=true", `{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != 200 || w.Body.String() != `{"input_tokens":60}` {
		t.Fatalf("count %d %s", w.Code, w.Body)
	}
	requestID := w.Header().Get(httpx.RequestIDHeader)
	var counts, charges, leases int
	if err := repo.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_requests WHERE request_id=$1 AND endpoint='messages.count_tokens' AND state='completed' AND input_tokens=0 AND output_tokens=0),(SELECT count(*) FROM billing_reservations WHERE request_id=$1),(SELECT count(*) FROM concurrency_leases WHERE request_id=$1)`, requestID).Scan(&counts, &charges, &leases); err != nil || counts != 1 || charges != 0 || leases != 0 {
		t.Fatalf("count settlement %d %d %d %v", counts, charges, leases, err)
	}
	// Speed changes generation billing only. Counting must remain free.
	w = send("/v1/messages/count_tokens", `{"model":"claude-test","speed":"fast","messages":[]}`)
	if w.Code != 200 || w.Body.String() != `{"input_tokens":60}` {
		t.Fatalf("count with speed %d %s", w.Code, w.Body)
	}
	if err := repo.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_reservations WHERE request_id=$1),(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1)`, w.Header().Get(httpx.RequestIDHeader)).Scan(&charges, &counts); err != nil || charges != 0 || counts != 0 {
		t.Fatalf("count with speed charges=%d ledger=%d: %v", charges, counts, err)
	}
	for _, stream := range []bool{false, true} {
		for _, speed := range []string{"fast", "turbo"} {
			before := upstreamCalls.Load()
			w = send("/v1/messages", fmt.Sprintf(`{"model":"claude-test","max_tokens":100,"stream":%t,"speed":%q,"messages":[]}`, stream, speed))
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"service_tier_not_supported"`) {
				t.Fatalf("unsupported speed=%s stream=%t: %d %s", speed, stream, w.Code, w.Body)
			}
			if upstreamCalls.Load() != before {
				t.Fatal("unsupported speed reached upstream")
			}
			if err := repo.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_requests WHERE request_id=$1),(SELECT count(*) FROM billing_reservations WHERE request_id=$1),(SELECT count(*) FROM concurrency_leases WHERE request_id=$1)`, w.Header().Get(httpx.RequestIDHeader)).Scan(&counts, &charges, &leases); err != nil || counts != 0 || charges != 0 || leases != 0 {
				t.Fatalf("unsupported speed usage=%d reservations=%d leases=%d: %v", counts, charges, leases, err)
			}
		}
	}
	if _, err := repo.PutSubscription(ctx, store.PutSubscriptionParams{BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: f.owner.ID, Reason: "Test Claude"}, UserID: f.owner.ID, Tier: store.BillingTierDay, AllowanceUSD: "1"}); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		for _, fields := range []string{"", `"speed":"standard",`, `"metadata":{"speed":"fast"},`, `"speed":"standard","service_tier":"standard_only",`} {
			w = send("/v1/messages?beta=true", fmt.Sprintf(`{"model":"claude-test","max_tokens":100,"stream":%t,%s"messages":[{"role":"user","content":"hello"}]}`, stream, fields))
			if w.Code != 200 {
				t.Fatalf("messages fields=%s stream=%t: %d %s", fields, stream, w.Code, w.Body)
			}
			var input, short, long, output int64
			var provider, mode, state string
			err := repo.DB().QueryRowContext(ctx, `SELECT u.input_tokens,u.cache_write_5m_tokens,u.cache_write_1h_tokens,u.output_tokens,a.provider,b.billing_mode,u.state FROM usage_requests u JOIN upstream_accounts a ON a.id=u.upstream_account_id JOIN billing_reservations b USING(request_id) WHERE request_id=$1`, w.Header().Get(httpx.RequestIDHeader)).Scan(&input, &short, &long, &output, &provider, &mode, &state)
			wantOutput := int64(7)
			if stream {
				wantOutput = 9
			}
			if err != nil || input != 60 || short != 12 || long != 18 || output != wantOutput || provider != "anthropic" || mode != store.BillingModeAnthropicAPIEquivalent || state != "completed" {
				t.Fatalf("billing %d %d %d %d %s %s %s %v", input, short, long, output, provider, mode, state, err)
			}
		}
	}
	// Transport truncation after a validated cumulative usage frame must
	// charge observed tokens once and release concurrency despite aborting SSE.
	interruptStream.Store(true)
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":100,"stream":true,"messages":[]}`))
	r.Header.Set("X-Api-Key", h.apiKey)
	r.Header.Set("Content-Type", "application/json")
	func() {
		defer func() {
			if caught := recover(); caught != http.ErrAbortHandler {
				t.Fatalf("interrupted stream did not abort connection: %v", caught)
			}
		}()
		s.Handler().ServeHTTP(w, r)
	}()
	interruptedID := w.Header().Get(httpx.RequestIDHeader)
	var interruptedState, interruptedCost string
	var interruptedInput, interruptedOutput, interruptedShort, interruptedLong int64
	var interruptedLeases, interruptedEntries int
	err = repo.DB().QueryRowContext(ctx, `SELECT u.state,u.input_tokens,u.output_tokens,u.cache_write_5m_tokens,u.cache_write_1h_tokens,
		b.actual_cost_usd::text,(SELECT count(*) FROM concurrency_leases WHERE request_id=$1),
		(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1)
		FROM usage_requests u JOIN billing_reservations b USING(request_id) WHERE request_id=$1`, interruptedID).
		Scan(&interruptedState, &interruptedInput, &interruptedOutput, &interruptedShort, &interruptedLong, &interruptedCost, &interruptedLeases, &interruptedEntries)
	if err != nil || interruptedState != "failed" || interruptedInput != 60 || interruptedOutput != 9 || interruptedShort != 12 || interruptedLong != 18 || interruptedCost != "0.000324000000" || interruptedLeases != 0 || interruptedEntries != 1 {
		t.Fatalf("interrupted settlement state=%s input=%d output=%d TTL=%d/%d cost=%s leases=%d ledger=%d: %v", interruptedState, interruptedInput, interruptedOutput, interruptedShort, interruptedLong, interruptedCost, interruptedLeases, interruptedEntries, err)
	}
	if err := repo.SettleRequest(ctx, interruptedID, time.Now().UTC()); err != nil {
		t.Fatalf("interrupted settlement replay: %v", err)
	}
	interruptStream.Store(false)

	// Explicit empty account scope denies counting too; a Codex account cannot
	// satisfy Anthropic scope through a globally shared account identifier.
	if _, err := repo.WithUpstreamProvider(store.UpstreamProviderAnthropic).SetUserUpstreamAccess(ctx, store.SetUserUpstreamAccessParams{UserID: f.owner.ID, Mode: "selected", AccountIDs: []string{}, Reason: "Disable Claude", ActorUserID: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	w = send("/v1/messages/count_tokens", `{"model":"claude-test","messages":[]}`)
	if w.Code != 403 {
		t.Fatalf("empty account scope %d %s", w.Code, w.Body)
	}
	w = send("/v1/messages", `{"model":"gpt-6.1-sol","max_tokens":1,"messages":[]}`)
	if w.Code != 400 {
		t.Fatalf("provider bypass %d %s", w.Code, w.Body)
	}
}
