//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/antigravity"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

type nativeLifecycleExecutor struct {
	response string
	failure  *antigravity.Failure
	calls    int
	started  chan struct{}
}

func (e *nativeLifecycleExecutor) Check(context.Context) error { return nil }
func (e *nativeLifecycleExecutor) Run(ctx context.Context, _ string) (antigravity.Result, *antigravity.Failure) {
	e.calls++
	if e.started != nil {
		close(e.started)
		<-ctx.Done()
		return antigravity.Result{}, &antigravity.Failure{Code: "client_disconnected"}
	}
	return antigravity.Result{Status: "success", Response: e.response, NumTurns: 1, Usage: &antigravity.Usage{
		InputTokens: 100, CacheReadTokens: 20, OutputTokens: 30, ThinkingTokens: 7, TotalTokens: 130,
	}}, e.failure
}

// Exercises the real HTTP handlers, bridge adapter and PostgreSQL transaction
// lifecycle together. A deterministic executor replaces only the Google process.
func TestNativeGeminiLifecyclePostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pgConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*pgConfig)
	defer admin.Close()
	schema := "native_gemini_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	pgConfig.RuntimeParams["search_path"] = schema
	repository := store.New(stdlib.OpenDB(*pgConfig))
	defer repository.Close()
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.SyncModelAccessCatalog(ctx, []string{config.AntigravityPublicModel}); err != nil {
		t.Fatal(err)
	}
	user, err := repository.CreateUser(ctx, store.CreateUserParams{Username: "native-test", DisplayName: "Native test", Role: store.UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	device, err := repository.CreateDevice(ctx, store.CreateDeviceParams{UserID: user.ID, Name: "native-cli"})
	if err != nil {
		t.Fatal(err)
	}
	h := newResponsesWebSocketTestHarness(t)
	key, err := repository.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		UserID: user.ID, DeviceID: device.ID, Name: "AGY test", PublicID: h.database.publicID,
		KeyPrefix: h.database.keyPrefix, KeyHash: h.database.keyHash, SecretCiphertext: h.database.secretCiphertext,
		ModelAllowlist: []string{config.AntigravityPublicModel}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PutSubscription(ctx, store.PutSubscriptionParams{
		BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "native lifecycle test"},
		UserID:             user.ID, Tier: store.BillingTierDay, AllowanceUSD: "1",
	}); err != nil {
		t.Fatal(err)
	}
	h.server.store = repository
	if _, err := repository.SetModelMultiplier(ctx, store.SetModelMultiplierParams{
		BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "shared Gemini alias discount"},
		Model:              config.AntigravityPublicModel, Multiplier: "0.5",
	}); err != nil {
		t.Fatal(err)
	}
	h.server.config.UsagePricing = nativeGeminiPricing(t)
	h.server.config.AntigravityModelRoutes = map[string]string{config.AntigravityPublicModel: config.AntigravityCLIModel}
	h.server.config.Limits = config.Limits{KeyRPM: 100, UserRPM: 100, KeyConcurrent: 3, UserConcurrent: 3, GlobalConcurrent: 10, KeyRequestsPerDay: 3}
	executor := &nativeLifecycleExecutor{response: "Hello"}
	bridge := antigravity.NewServer(executor, "internal-bridge-token")
	bridge.Refresh(ctx)
	bridgeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer internal-bridge-token" || r.Header.Get("X-Goog-Api-Key") != "" || r.URL.Query().Has("key") {
			t.Error("client credentials reached bridge")
		}
		if r.URL.Path == "/internal/upstream-accounts" {
			writeJSON(w, 200, map[string]any{"accounts": []gatewayproxy.UpstreamAccount{{
				ID: "aabbccddeeff0011", DisplayName: "default", MaskedEmail: "a***@example.com", Plan: "unknown",
				Status: "available", CliproxyStatus: "active", GatewayManualStatus: "enabled", GatewayQuotaStatus: "available", LastSyncedAt: time.Now().UTC(),
			}}})
			return
		}
		if r.URL.Path == "/internal/upstream-accounts/capabilities" {
			writeJSON(w, 200, map[string]string{"protocol": "upstream_account_access_v1"})
			return
		}
		if r.Header.Get("X-Codex-Gateway-User") != user.ID {
			t.Error("bridge is missing authenticated user identity")
		}
		w.Header().Set("X-Codex-Upstream-Account", "aabbccddeeff0011")
		if !strings.HasPrefix(r.URL.Path, "/v1beta/models/"+config.AntigravityPublicModel+":") {
			t.Error("noncanonical bridge path")
		}
		bridge.ServeHTTP(w, r)
	}))
	defer bridgeServer.Close()
	bridgeURL, _ := url.Parse(bridgeServer.URL)
	h.server.antigravity = gatewayproxy.NewAntigravity(bridgeURL, "internal-bridge-token")
	send := func(model, action, body string) *httptest.ResponseRecorder {
		t.Helper()
		path := "/v1beta/models/" + model + ":" + action
		if action == "streamGenerateContent" {
			path += "?alt=sse"
		}
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("X-Goog-Api-Key", h.apiKey)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		return w
	}
	textResponse := send(config.AntigravityPublicModel, "generateContent", nativeGeminiText)
	if textResponse.Code != 200 {
		t.Fatalf("text: %d %s", textResponse.Code, textResponse.Body)
	}
	agyAccounts, err := repository.WithUpstreamProvider(store.UpstreamProviderAntigravity).ListUpstreamAccounts(ctx)
	if err != nil || len(agyAccounts) != 1 || agyAccounts[0].ID != "aabbccddeeff0011" {
		t.Fatalf("missing automatically synchronized AGY account: %+v %v", agyAccounts, err)
	}
	codexAccounts, err := repository.ListUpstreamAccounts(ctx)
	if err != nil || len(codexAccounts) != 0 {
		t.Fatalf("AGY account leaked into Codex: %+v %v", codexAccounts, err)
	}
	var accountID string
	if err := repository.DB().QueryRowContext(ctx, "SELECT upstream_account_id FROM usage_requests WHERE api_key_id=$1 AND state='completed'", key.ID).Scan(&accountID); err != nil || accountID != "aabbccddeeff0011" {
		t.Fatalf("AGY attribution missing: %q %v", accountID, err)
	}
	executor.response = `{"type":"function_call","name":"read_file","arguments":{"path":"README.md"}}`
	toolBody := `{"contents":[{"role":"user","parts":[{"text":"read README"}]}],"tools":[{"functionDeclarations":[{"name":"read_file","parametersJsonSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}]}`
	toolResponse := send("gemini-3.1-pro-preview-customtools", "streamGenerateContent", toolBody)
	if toolResponse.Code != 200 {
		t.Fatalf("tool: %d %s", toolResponse.Code, toolResponse.Body)
	}
	if strings.Contains(toolResponse.Body.String(), "[DONE]") || !strings.HasPrefix(toolResponse.Body.String(), "data: ") {
		t.Fatalf("invalid Gemini SSE %s", toolResponse.Body)
	}
	var toolResult struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					FunctionCall struct {
						ID   string         `json:"id"`
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(toolResponse.Body.String(), "data: "))), &toolResult); err != nil {
		t.Fatal(err)
	}
	if len(toolResult.Candidates) != 1 || len(toolResult.Candidates[0].Content.Parts) != 1 {
		t.Fatal("missing tool result")
	}
	call := toolResult.Candidates[0].Content.Parts[0].FunctionCall
	if call.Name != "read_file" || call.ID == "" || call.Args["path"] != "README.md" {
		t.Fatalf("call=%+v", call)
	}
	executor.response = "The README describes the gateway."
	loopBody := fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"read README"}]},{"role":"model","parts":[{"functionCall":{"id":%q,"name":"read_file","args":{"path":"README.md"}}}]},{"role":"model","parts":[{"functionResponse":{"id":%q,"name":"read_file","response":{"output":"Gateway documentation"}}}]}],"tools":[{"functionDeclarations":[{"name":"read_file","parameters":{"type":"OBJECT","properties":{"path":{"type":"STRING"}},"required":["path"]}}]}]}`, call.ID, call.ID)
	loopResponse := send(config.AntigravityPublicModel, "generateContent", loopBody)
	if loopResponse.Code != 200 {
		t.Fatalf("loop: %d %s", loopResponse.Code, loopResponse.Body)
	}
	for i, w := range []*httptest.ResponseRecorder{textResponse, toolResponse, loopResponse} {
		requestID := w.Header().Get(httpx.RequestIDHeader)
		if err := repository.SettleRequest(ctx, requestID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var endpoint, model, state, cost, multiplier string
		var input, cached, output, reasoning, count int
		err := repository.DB().QueryRowContext(ctx, `SELECT u.endpoint,u.model,u.state,u.input_tokens,u.cached_input_tokens,u.output_tokens,u.reasoning_tokens,l.amount_usd::text,
		(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1),l.pricing_multiplier::text FROM usage_requests u JOIN billing_ledger_entries l USING(request_id) WHERE u.request_id=$1`, requestID).Scan(&endpoint, &model, &state, &input, &cached, &output, &reasoning, &cost, &count, &multiplier)
		if err != nil {
			t.Fatal(err)
		}
		wantEndpoint := "gemini.generateContent"
		if i == 1 {
			wantEndpoint = "gemini.streamGenerateContent"
		}
		if endpoint != wantEndpoint || model != config.AntigravityPublicModel || state != "completed" || input != 100 || cached != 20 || output != 30 || reasoning != 7 || cost != "0.000262000000" || count != 1 || multiplier != "0.500000000000" {
			t.Fatalf("settlement %s %s %s %d %d %d %d %s count=%d multiplier=%s", endpoint, model, state, input, cached, output, reasoning, cost, count, multiplier)
		}
	}
	quotaResponse := send(config.AntigravityPublicModel, "generateContent", nativeGeminiText)
	checkNativeGeminiError(t, quotaResponse, 429, "RESOURCE_EXHAUSTED")
	if quotaResponse.Header().Get("Retry-After") == "" {
		t.Fatal("quota lost Retry-After")
	}
	titleResponse := send("gemini-2.5-flash-lite", "generateContent", nativeGeminiText)
	checkNativeGeminiError(t, titleResponse, 404, "NOT_FOUND")
	var admitted int
	if err := repository.DB().QueryRowContext(ctx, `SELECT count(*) FROM usage_requests WHERE api_key_id=$1`, key.ID).Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	if admitted != 3 || executor.calls != 3 || h.upstreamCalls.Load() != 0 {
		t.Fatalf("admitted=%d executor=%d primary=%d", admitted, executor.calls, h.upstreamCalls.Load())
	}
	h.server.config.Limits.KeyRequestsPerDay = 0
	executor.failure = &antigravity.Failure{Status: 504, Code: "upstream_timeout", Message: "private provider detail"}
	timeoutResponse := send(config.AntigravityPublicModel, "generateContent", nativeGeminiText)
	checkNativeGeminiError(t, timeoutResponse, 504, "DEADLINE_EXCEEDED")
	if strings.Contains(timeoutResponse.Body.String(), "private provider") {
		t.Fatal("provider details exposed")
	}
	var usageState, reservationState string
	if err := repository.DB().QueryRowContext(ctx, `SELECT u.state,q.state FROM usage_requests u JOIN quota_reservations q USING(request_id) WHERE u.request_id=$1`, timeoutResponse.Header().Get(httpx.RequestIDHeader)).Scan(&usageState, &reservationState); err != nil {
		t.Fatal(err)
	}
	if usageState != "failed" || reservationState != "settled" {
		t.Fatalf("timeout leaked lease: %s %s", usageState, reservationState)
	}
	executor.failure, executor.started = nil, make(chan struct{})
	cancelCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	cancelled := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+config.AntigravityPublicModel+":streamGenerateContent?alt=sse", strings.NewReader(nativeGeminiText)).WithContext(cancelCtx)
	cancelled.Header.Set("Authorization", "Bearer "+h.apiKey)
	cancelledResponse := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { defer close(finished); h.handler.ServeHTTP(cancelledResponse, cancelled) }()
	select {
	case <-executor.started:
	case <-ctx.Done():
		t.Fatal("cancelled request did not reach executor")
	}
	cancelRequest()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("cancelled request did not settle")
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT u.state,q.state FROM usage_requests u JOIN quota_reservations q USING(request_id) WHERE u.request_id=$1`, cancelledResponse.Header().Get(httpx.RequestIDHeader)).Scan(&usageState, &reservationState); err != nil {
		t.Fatal(err)
	}
	if usageState != "cancelled" || reservationState != "settled" {
		t.Fatalf("cancellation leaked lease: %s %s", usageState, reservationState)
	}
	if _, err := repository.DeleteSubscription(ctx, store.DeleteSubscriptionParams{
		BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "test exhausted funding"},
		UserID:             user.ID, Tier: store.BillingTierDay,
	}); err != nil {
		t.Fatal(err)
	}
	exhausted := send(config.AntigravityPublicModel, "generateContent", nativeGeminiText)
	checkNativeGeminiError(t, exhausted, 429, "RESOURCE_EXHAUSTED")
	if err := repository.DB().QueryRowContext(ctx, `SELECT count(*) FROM usage_requests WHERE api_key_id=$1`, key.ID).Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	if admitted != 5 || executor.calls != 5 {
		t.Fatalf("funding rejection reserved a request: admitted=%d calls=%d", admitted, executor.calls)
	}
}
