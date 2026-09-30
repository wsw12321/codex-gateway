//go:build integration

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// Tests the real Gateway admission, native JSON/SSE adapter, provider-scoped
// attribution, settlement and lease release. Only CPA/Google is replaced.
func TestCPANativeGatewayPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*pg)
	defer admin.Close()
	schema := "cpa_native_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	pg.RuntimeParams["search_path"] = schema
	repo := store.New(stdlib.OpenDB(*pg))
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repo.SyncModelAccessCatalog(ctx, append(config.AntigravityModels(), "gpt-6.1-sol")); err != nil {
		t.Fatal(err)
	}
	user, err := repo.CreateUser(ctx, store.CreateUserParams{Username: "cpa-native", DisplayName: "CPA Native", Role: store.UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	device, err := repo.CreateDevice(ctx, store.CreateDeviceParams{UserID: user.ID, Name: "native"})
	if err != nil {
		t.Fatal(err)
	}
	h := newResponsesWebSocketTestHarness(t)
	key, err := repo.CreateAPIKey(ctx, store.CreateAPIKeyParams{UserID: user.ID, DeviceID: device.ID, Name: "native", PublicID: h.database.publicID, KeyPrefix: h.database.keyPrefix, KeyHash: h.database.keyHash, SecretCiphertext: h.database.secretCiphertext, ModelAllowlist: []string{"gemini-pro-agent"}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PutSubscription(ctx, store.PutSubscriptionParams{BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "native test"}, UserID: user.ID, Tier: store.BillingTierDay, AllowanceUSD: "1"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(string(data))
	if err != nil {
		t.Fatal(err)
	}
	h.server.store = repo
	h.server.modelAccessRepo = repo
	h.server.config.UsagePricing = pricing
	h.server.config.BodyLimit = 64 << 20
	h.server.config.AntigravityTransport = "cpa"
	h.server.config.AntigravityModelRoutes = map[string]string{"gemini-pro-agent": "gemini-pro-agent"}
	h.server.config.Limits = config.Limits{KeyRPM: 100, UserRPM: 100, KeyConcurrent: 3, UserConcurrent: 3, GlobalConcurrent: 10, KeyRequestsPerDay: 100}
	var missingUsage atomic.Bool
	var calls atomic.Int64
	const account = "1122334455667788"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer internal-cpa" || r.Header.Get("X-Provider") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
			t.Error("untrusted provider or credential header")
		}
		switch r.URL.Path {
		case "/internal/antigravity-accounts":
			writeJSON(w, 200, map[string]any{"accounts": []gatewayproxy.UpstreamAccount{{ID: account, DisplayName: "migrated", MaskedEmail: "a***@example.com", Plan: "unknown", Status: "available", CliproxyStatus: "active", GatewayManualStatus: "enabled", GatewayQuotaStatus: "available", LastSyncedAt: time.Now().UTC()}}})
			return
		case "/internal/antigravity-accounts/capabilities":
			writeJSON(w, 200, map[string]string{"protocol": "upstream_account_access_v1"})
			return
		}
		calls.Add(1)
		if r.Header.Get("X-Codex-Gateway-User") != user.ID || r.Header.Get("X-Codex-Gateway-Affinity") != upstreamAffinityScope(h.server.config.KeyPepper, key.ID) {
			t.Error("caller identity lost")
		}
		w.Header().Set("X-Codex-Upstream-Account", account)
		if r.URL.Path == "/v1/responses" {
			usage := `,"usage":{"input_tokens":100,"output_tokens":37,"total_tokens":137,"input_tokens_details":{"cached_tokens":20},"output_tokens_details":{"reasoning_tokens":7}}`
			if missingUsage.Load() {
				usage = ""
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"model":"gemini-pro-agent","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]%s}`, usage)
			return
		}
		usage := `,"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":30,"thoughtsTokenCount":7,"cachedContentTokenCount":20,"totalTokenCount":137}`
		if missingUsage.Load() {
			usage = ""
		}
		body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok","thoughtSignature":"opaque"}]},"finishReason":"STOP"}]` + usage + `}`
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: "+body+"\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, body)
		}
	}))
	defer cpa.Close()
	base, _ := url.Parse(cpa.URL)
	h.server.antigravity = gatewayproxy.NewCPAAntigravity(base, "internal-cpa")
	send := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+h.apiKey)
		r.Header.Set("X-Provider", "codex")
		r.Header.Set("X-Codex-Gateway-Affinity", strings.Repeat("x", 43))
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/v1beta/models/gemini-3.1-pro-high:generateContent", "/v1beta/models/gemini-3.1-pro-preview:generateContent"} {
		if w := send(path, nativeGeminiText); w.Code != 404 {
			t.Fatalf("retired alias accepted %d", w.Code)
		}
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1beta/models/gemini-pro-agent:generateContent", nativeGeminiText},
		{"/v1beta/models/gemini-pro-agent:streamGenerateContent?alt=sse", nativeGeminiText},
		{"/v1/responses", `{"model":"gemini-pro-agent","input":"hello"}`},
	} {
		w := send(tc.path, tc.body)
		if w.Code != 200 {
			t.Fatalf("native status %d %s", w.Code, w.Body)
		}
		var state, provider, mode, cost string
		var input, cached, output, reasoning int64
		err := repo.DB().QueryRowContext(ctx, `SELECT u.state,a.provider,u.input_tokens,u.cached_input_tokens,u.output_tokens,u.reasoning_tokens,b.billing_mode,l.amount_usd::text FROM usage_requests u JOIN upstream_accounts a ON a.id=u.upstream_account_id JOIN billing_reservations b USING(request_id) JOIN billing_ledger_entries l USING(request_id) WHERE u.request_id=$1`, w.Header().Get(httpx.RequestIDHeader)).Scan(&state, &provider, &input, &cached, &output, &reasoning, &mode, &cost)
		if err != nil {
			t.Fatal(err)
		}
		if state != "completed" || provider != "antigravity" || input != 100 || cached != 20 || output != 37 || reasoning != 7 || mode != store.BillingModeGeminiAPIEquivalent || cost != "0.000608000000" {
			t.Fatalf("settlement %s %s %d %d %d %d %s %s", state, provider, input, cached, output, reasoning, mode, cost)
		}
	}
	missingUsage.Store(true)
	w := send("/v1beta/models/gemini-pro-agent:generateContent", nativeGeminiText)
	if w.Code != 502 {
		t.Fatalf("missing usage status %d %s", w.Code, w.Body)
	}
	var state string
	if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM usage_requests WHERE request_id=$1`, w.Header().Get(httpx.RequestIDHeader)).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("missing usage settlement %s %v", state, err)
	}
	var active int
	if err := repo.DB().QueryRowContext(ctx, `SELECT count(*) FROM quota_reservations WHERE state='reserved'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 || calls.Load() != 4 {
		t.Fatalf("leases=%d generation calls=%d", active, calls.Load())
	}
}
