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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/antigravity"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

type nativeLifecycleExecutor struct {
	response          string
	responseForPrompt func(string) string
	failure           *antigravity.Failure
	calls             atomic.Int64
	model             atomic.Value
	started           chan struct{}
}

func (e *nativeLifecycleExecutor) Check(context.Context) ([]string, error) {
	return config.LegacyAntigravityModels(), nil
}
func (e *nativeLifecycleExecutor) Run(ctx context.Context, model, prompt string) (antigravity.Result, *antigravity.Failure) {
	e.calls.Add(1)
	e.model.Store(model)
	if e.started != nil {
		close(e.started)
		<-ctx.Done()
		return antigravity.Result{}, &antigravity.Failure{Code: "client_disconnected"}
	}
	response := e.response
	if e.responseForPrompt != nil {
		response = e.responseForPrompt(prompt)
	}
	return antigravity.Result{Status: "success", Response: response, NumTurns: 1, Usage: &antigravity.Usage{
		InputTokens: 100, CacheReadTokens: 20, OutputTokens: 30, ThinkingTokens: 7, TotalTokens: 130,
	}}, e.failure
}

func (e *nativeLifecycleExecutor) RunStream(ctx context.Context, model, prompt string, emit func(string) error) (antigravity.Result, *antigravity.Failure) {
	if emit("") != nil {
		return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled"}
	}
	result, failure := e.Run(ctx, model, prompt)
	if failure == nil {
		for _, part := range strings.SplitAfter(result.Response, " ") {
			if part != "" && emit(part) != nil {
				return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled"}
			}
		}
	}
	return result, failure
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
	if err := repository.SyncModelAccessCatalog(ctx, []string{config.LegacyAntigravityPublicModel}); err != nil {
		t.Fatal(err)
	}
	user, err := repository.CreateUser(ctx, store.CreateUserParams{Username: "native-test", DisplayName: "Native test", Role: store.UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUserModelAccess(ctx, store.SetUserModelAccessParams{
		ModelAccessWriteParams: store.ModelAccessWriteParams{ActorUserID: user.ID, Reason: "authorize native lifecycle test"},
		Model:                  config.LegacyAntigravityPublicModel, Enabled: true, Scope: store.ModelAccessScopeSelected, UserIDs: []string{user.ID},
	}); err != nil {
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
		ModelAllowlist: []string{config.LegacyAntigravityPublicModel}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
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
		BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "Pro model discount"},
		Model:              config.LegacyAntigravityPublicModel, Multiplier: "0.5",
	}); err != nil {
		t.Fatal(err)
	}
	h.server.config.UsagePricing = nativeGeminiPricing(t)
	h.server.config.AntigravityModelRoutes = map[string]string{config.LegacyAntigravityPublicModel: config.LegacyAntigravityPublicModel}
	h.server.config.Limits = config.Limits{KeyRPM: 100, UserRPM: 100, KeyConcurrent: 3, UserConcurrent: 3, GlobalConcurrent: 10, KeyRequestsPerDay: 4}
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
		bridgeModel, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":")
		if !strings.HasPrefix(r.URL.Path, "/v1beta/models/") || !config.IsLegacyAntigravityModel(bridgeModel) {
			t.Error("noncanonical bridge path")
		}
		bridge.ServeHTTP(w, r)
	}))
	defer bridgeServer.Close()
	bridgeURL, _ := url.Parse(bridgeServer.URL)
	h.server.antigravity = gatewayproxy.NewAntigravity(bridgeURL, "internal-bridge-token")
	sendWithKey := func(apiKey, model, action, body string) *httptest.ResponseRecorder {
		t.Helper()
		path := "/v1beta/models/" + model + ":" + action
		if action == "streamGenerateContent" {
			path += "?alt=sse"
		}
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("X-Goog-Api-Key", apiKey)
		r.Header.Set("X-Codex-Gateway-Affinity", strings.Repeat("x", 43))
		r.Header.Set("X-Codex-Conversation-Hash", "conv-0123456789abcdef0123456789abcdef")
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		for _, header := range []string{"X-Codex-Gateway-Affinity", "X-Codex-Conversation-Hash", "X-Codex-Upstream-Account"} {
			if w.Header().Get(header) != "" {
				t.Errorf("internal header leaked to client: %s", header)
			}
		}
		return w
	}
	send := func(model, action, body string) *httptest.ResponseRecorder {
		return sendWithKey(h.apiKey, model, action, body)
	}
	// Only the actual model can grant access. Neither a client alias in the
	// key allowlist nor an enabled API key can bypass the user's model grant.
	aliases := []string{"gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.1-flash-lite-preview"}
	if _, err := repository.DB().ExecContext(ctx, `UPDATE api_keys SET model_allowlist=$2 WHERE id=$1`, key.ID, aliases); err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases {
		checkNativeGeminiError(t, send(alias, "generateContent", nativeGeminiText), 403, "PERMISSION_DENIED")
		if config.IsLegacyAntigravityModel(alias) {
			t.Fatalf("native alias leaked into model catalog: %s", alias)
		}
	}
	if _, err := repository.DB().ExecContext(ctx, `UPDATE api_keys SET model_allowlist=ARRAY[$2] WHERE id=$1`, key.ID, config.LegacyAntigravityPublicModel); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		if _, err := repository.SetUserModelAccess(ctx, store.SetUserModelAccessParams{
			ModelAccessWriteParams: store.ModelAccessWriteParams{ActorUserID: user.ID, Reason: "verify native alias model permission"},
			Model:                  config.LegacyAntigravityPublicModel, Enabled: enabled, Scope: store.ModelAccessScopeSelected, UserIDs: []string{user.ID},
		}); err != nil {
			t.Fatal(err)
		}
		if !enabled {
			for _, alias := range aliases {
				checkNativeGeminiError(t, send(alias, "generateContent", nativeGeminiText), 403, "PERMISSION_DENIED")
			}
		}
	}
	var rejectedUsage, rejectedQuota, rejectedBilling int
	if err := repository.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_requests), (SELECT count(*) FROM quota_reservations), (SELECT count(*) FROM billing_reservations)`).Scan(&rejectedUsage, &rejectedQuota, &rejectedBilling); err != nil {
		t.Fatal(err)
	}
	if rejectedUsage != 0 || rejectedQuota != 0 || rejectedBilling != 0 || executor.calls.Load() != 0 {
		t.Fatalf("denied alias reserved resources: usage=%d quota=%d billing=%d calls=%d", rejectedUsage, rejectedQuota, rejectedBilling, executor.calls.Load())
	}
	withConversation := func(body, conversationID string) string {
		// Generation defaults captured from official AGY 1.2.12 selecting Pro high.
		return `{"generationConfig":{"maxOutputTokens":65535,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":-1}},"systemInstruction":{"parts":[{"text":"Conversation ID: ` + conversationID + `"}]},` + strings.TrimPrefix(body, "{")
	}
	const conversationID = "12345678-1234-5678-9012-123456789abc"
	conversationHash := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var hash string
		if err := repository.DB().QueryRowContext(ctx, `SELECT COALESCE(conversation_hash,'') FROM usage_requests WHERE request_id=$1`, w.Header().Get(httpx.RequestIDHeader)).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	textResponse := send("gemini-3.1-pro-preview", "generateContent", withConversation(nativeGeminiText, conversationID))
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
	toolResponse := send("gemini-3.1-pro-preview-customtools", "streamGenerateContent", withConversation(toolBody, conversationID))
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
	toolEvents := strings.Split(strings.TrimSpace(toolResponse.Body.String()), "\n\n")
	if len(toolEvents) < 2 || !strings.Contains(toolEvents[0], `"parts":[]`) {
		t.Fatal("tool response did not send early keepalive")
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(toolEvents[len(toolEvents)-1], "data: "))), &toolResult); err != nil {
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
	loopResponse := send("gemini-3.1-pro-preview-customtools", "generateContent", withConversation(loopBody, strings.ToUpper(conversationID)))
	if loopResponse.Code != 200 {
		t.Fatalf("loop: %d %s", loopResponse.Code, loopResponse.Body)
	}
	mainConversationHash := conversationHash(textResponse)
	if !store.ValidConversationHash(mainConversationHash) || conversationHash(toolResponse) != mainConversationHash || conversationHash(loopResponse) != mainConversationHash {
		t.Fatalf("main and tool rounds were not grouped: main=%q tool=%q loop=%q", mainConversationHash, conversationHash(toolResponse), conversationHash(loopResponse))
	}
	executor.response = "Gateway documentation"
	titleResponse := send("gemini-3.1-flash-lite-preview", "generateContent", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":-1}},"contents":[{"role":"user","parts":[{"text":"Generate a short title for this conversation: read the gateway README."}]}]}`)
	if titleResponse.Code != 200 || conversationHash(titleResponse) != "" {
		t.Fatalf("title: %d %s", titleResponse.Code, titleResponse.Body)
	}
	if executor.model.Load() != config.LegacyAntigravityPublicModel {
		t.Fatalf("executor received alias %q", executor.model.Load())
	}
	for i, w := range []*httptest.ResponseRecorder{textResponse, toolResponse, loopResponse, titleResponse} {
		if !strings.Contains(w.Body.String(), `"modelVersion":"`+config.LegacyAntigravityPublicModel+`"`) {
			t.Fatalf("response did not identify actual model: %s", w.Body)
		}
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
		if endpoint != wantEndpoint || model != config.LegacyAntigravityPublicModel || state != "completed" || input != 100 || cached != 20 || output != 30 || reasoning != 7 || cost != "0.000262000000" || count != 1 || multiplier != "0.500000000000" {
			t.Fatalf("settlement %s %s %s %d %d %d %d %s count=%d multiplier=%s", endpoint, model, state, input, cached, output, reasoning, cost, count, multiplier)
		}
		var mode, tier, fallback string
		if err := repository.DB().QueryRowContext(ctx, `SELECT billing_mode,pricing_service_tier,COALESCE(pricing_fallback_reason,'') FROM billing_reservations WHERE request_id=$1`, requestID).Scan(&mode, &tier, &fallback); err != nil {
			t.Fatal(err)
		}
		if mode != store.BillingModeGeminiAPIEquivalent || tier != config.PricingTierStandard || fallback != "" {
			t.Fatalf("billing metadata mode=%q tier=%q fallback=%q", mode, tier, fallback)
		}
	}
	for _, model := range append(aliases, config.LegacyAntigravityPublicModel) {
		quotaResponse := send(model, "generateContent", nativeGeminiText)
		checkNativeGeminiError(t, quotaResponse, 429, "RESOURCE_EXHAUSTED")
		if quotaResponse.Header().Get("Retry-After") == "" {
			t.Fatal("quota lost Retry-After")
		}
	}
	unsupportedTitleResponse := send("gemini-2.5-flash-lite", "generateContent", nativeGeminiText)
	checkNativeGeminiError(t, unsupportedTitleResponse, 404, "NOT_FOUND")
	var admitted int
	if err := repository.DB().QueryRowContext(ctx, `SELECT count(*) FROM usage_requests WHERE api_key_id=$1`, key.ID).Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	if admitted != 4 || executor.calls.Load() != 4 || h.upstreamCalls.Load() != 0 {
		t.Fatalf("admitted=%d executor=%d primary=%d", admitted, executor.calls.Load(), h.upstreamCalls.Load())
	}
	var reserved, completed, tokens int
	if err := repository.DB().QueryRowContext(ctx, `SELECT requests_reserved,requests_completed,tokens_used FROM quota_counters WHERE scope_type='key' AND scope_id=$1`, key.ID).Scan(&reserved, &completed, &tokens); err != nil {
		t.Fatal(err)
	}
	if reserved != 4 || completed != 4 || tokens != 520 {
		t.Fatalf("aliases did not share quota: reserved=%d completed=%d tokens=%d", reserved, completed, tokens)
	}
	h.server.config.Limits.KeyRequestsPerDay = 0
	otherConversation := send(config.LegacyAntigravityPublicModel, "generateContent", withConversation(nativeGeminiText, "87654321-4321-8765-2109-cba987654321"))
	if otherConversation.Code != 200 || !store.ValidConversationHash(conversationHash(otherConversation)) || conversationHash(otherConversation) == mainConversationHash {
		t.Fatalf("new conversation was not isolated: %d %s", otherConversation.Code, otherConversation.Body)
	}
	generated, err := security.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := security.HashAPIKey(h.server.config.KeyPepper, generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := security.EncryptAPIKeySecret(h.server.config.APIKeyEncryptionKey, user.ID, generated.PublicID, generated.Token)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := repository.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		UserID: user.ID, DeviceID: device.ID, Name: "AGY second key", PublicID: generated.PublicID,
		KeyPrefix: generated.Prefix, KeyHash: digest[:], SecretCiphertext: ciphertext,
		ModelAllowlist: []string{config.LegacyAntigravityPublicModel}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherKeyResponse := sendWithKey(generated.Token, config.LegacyAntigravityPublicModel, "generateContent", withConversation(nativeGeminiText, conversationID))
	if otherKey.ID == key.ID || otherKeyResponse.Code != 200 || !store.ValidConversationHash(conversationHash(otherKeyResponse)) || conversationHash(otherKeyResponse) == mainConversationHash {
		t.Fatalf("API key conversation scope was not isolated: %d %s", otherKeyResponse.Code, otherKeyResponse.Body)
	}
	for range 2 {
		title := send("gemini-3.1-flash-lite-preview", "generateContent", nativeGeminiText)
		if title.Code != 200 || conversationHash(title) != "" {
			t.Fatalf("unmarked title request was grouped: %d %s", title.Code, title.Body)
		}
	}
	executor.failure = &antigravity.Failure{Status: 504, Code: "upstream_timeout", Message: "private provider detail"}
	timeoutResponse := send(config.LegacyAntigravityPublicModel, "generateContent", nativeGeminiText)
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
	cancelled := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+config.LegacyAntigravityPublicModel+":streamGenerateContent?alt=sse", strings.NewReader(nativeGeminiText)).WithContext(cancelCtx)
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
	exhausted := send(config.LegacyAntigravityPublicModel, "generateContent", nativeGeminiText)
	checkNativeGeminiError(t, exhausted, 429, "RESOURCE_EXHAUSTED")
	if err := repository.DB().QueryRowContext(ctx, `SELECT count(*) FROM usage_requests WHERE api_key_id=$1`, key.ID).Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	if admitted != 9 || executor.calls.Load() != 10 {
		t.Fatalf("funding rejection reserved a request: admitted=%d calls=%d", admitted, executor.calls.Load())
	}
	t.Run("AGY CLI 1.2.12", func(t *testing.T) {
		testNativeAGYCLI(t, h.handler, repository, user.ID, key.ID, h.apiKey, executor)
	})
	t.Run("stream failure settles before HTTP abort", func(t *testing.T) {
		executor.started = nil
		executor.failure = &antigravity.Failure{Status: 502, Code: "upstream_protocol_error", Message: "private failure detail"}
		defer func() { executor.failure = nil }()
		if _, err := repository.PutSubscription(ctx, store.PutSubscriptionParams{
			BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "stream failure settlement"},
			UserID:             user.ID, Tier: store.BillingTierDay, AllowanceUSD: "1",
		}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+config.LegacyAntigravityPublicModel+":streamGenerateContent?alt=sse", strings.NewReader(nativeGeminiText))
		r.Header.Set("Authorization", "Bearer "+h.apiKey)
		w := httptest.NewRecorder()
		var aborted any
		func() {
			defer func() { aborted = recover() }()
			h.handler.ServeHTTP(w, r)
		}()
		if aborted != http.ErrAbortHandler || strings.Contains(w.Body.String(), "private failure") || strings.Contains(w.Body.String(), "finishReason") {
			t.Fatalf("unsafe failed stream: abort=%v body=%s", aborted, w.Body)
		}
		requestID := w.Header().Get(httpx.RequestIDHeader)
		var state, reservation, cost string
		var status, leases, count int
		if err := repository.DB().QueryRowContext(ctx, `SELECT u.state,u.http_status,q.state,l.amount_usd::text,
			(SELECT count(*) FROM concurrency_leases WHERE request_id=$1),
			(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1)
			FROM usage_requests u JOIN quota_reservations q USING(request_id) JOIN billing_ledger_entries l USING(request_id) WHERE u.request_id=$1`, requestID).Scan(&state, &status, &reservation, &cost, &leases, &count); err != nil {
			t.Fatal(err)
		}
		if state != "failed" || status != 502 || reservation != "settled" || cost != "0.000000000000" || leases != 0 || count != 1 {
			t.Fatalf("failed stream settlement: %s %d %s cost=%s leases=%d count=%d", state, status, reservation, cost, leases, count)
		}
	})
	t.Run("Flash client presets", func(t *testing.T) {
		executor.started, executor.failure, executor.response = nil, nil, "OK"
		if _, err := repository.PutSubscription(ctx, store.PutSubscriptionParams{
			BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "Flash preset test"},
			UserID:             user.ID, Tier: store.BillingTierDay, AllowanceUSD: "1",
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.SyncModelAccessCatalog(ctx, config.LegacyAntigravityModels()); err != nil {
			t.Fatal(err)
		}
		for _, preset := range []struct{ model, body string }{
			{"gemini-3.8-flash-high", nativeFlashHigh},
			{"gemini-3.8-flash-medium", nativeFlashMedium},
		} {
			h.server.config.AntigravityModelRoutes[preset.model] = preset.model
			// CPA migration grants the shared Flash-high ID by default. Revoke
			// it explicitly before exercising rollback permission rejection.
			if _, err := repository.SetUserModelAccess(ctx, store.SetUserModelAccessParams{
				ModelAccessWriteParams: store.ModelAccessWriteParams{ActorUserID: user.ID, Reason: "exercise revoked Flash access"},
				Model:                  preset.model, Enabled: false, Scope: store.ModelAccessScopeSelected, UserIDs: []string{user.ID},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.DB().ExecContext(ctx, `UPDATE api_keys SET model_allowlist=$2 WHERE id=$1`, key.ID, []string{preset.model}); err != nil {
				t.Fatal(err)
			}
			checkNativeGeminiError(t, send("gemini-3.8-flash", "generateContent", preset.body), 403, "PERMISSION_DENIED")
			if _, err := repository.SetUserModelAccess(ctx, store.SetUserModelAccessParams{
				ModelAccessWriteParams: store.ModelAccessWriteParams{ActorUserID: user.ID, Reason: "authorize Flash preset test"},
				Model:                  preset.model, Enabled: true, Scope: store.ModelAccessScopeSelected, UserIDs: []string{user.ID},
			}); err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"generateContent", "streamGenerateContent"} {
				response := send("gemini-3.8-flash", action, preset.body)
				if response.Code != 200 || executor.model.Load() != preset.model || !strings.Contains(response.Body.String(), `"modelVersion":"`+preset.model+`"`) {
					t.Fatalf("Flash model mismatch: %d %s executor=%v", response.Code, response.Body, executor.model.Load())
				}
				requestID := response.Header().Get(httpx.RequestIDHeader)
				if err := repository.SettleRequest(ctx, requestID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				var model, requested, state, cost string
				var count int
				if err := repository.DB().QueryRowContext(ctx, `SELECT u.model,u.requested_model,u.state,l.amount_usd::text,
					(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1)
					FROM usage_requests u JOIN billing_ledger_entries l USING(request_id) WHERE u.request_id=$1`, requestID).Scan(&model, &requested, &state, &cost, &count); err != nil {
					t.Fatal(err)
				}
				if model != preset.model || requested != preset.model || state != "completed" || cost != "0.000174000000" || count != 1 {
					t.Fatalf("Flash settlement: model=%s requested=%s state=%s cost=%s count=%d", model, requested, state, cost, count)
				}
			}
		}
	})
}
