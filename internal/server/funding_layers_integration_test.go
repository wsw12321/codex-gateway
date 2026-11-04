//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
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

// Replays the production incident end to end: two groups whose pools cannot
// serve the request, an account reachable only through personal funds, and a
// sidecar that serves it. Only CPA is replaced.
func TestFundingLayersGatewayPostgresIntegration(t *testing.T) {
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
	schema := "funding_layers_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	const model = "gemini-3.8-flash-high"
	if err := repo.SyncModelAccessCatalog(ctx, append(config.AntigravityModels(), "gpt-6.1-sol")); err != nil {
		t.Fatal(err)
	}
	user, err := repo.CreateUser(ctx, store.CreateUserParams{Username: "funding-layers", DisplayName: "Funding Layers", Role: store.UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	device, err := repo.CreateDevice(ctx, store.CreateDeviceParams{UserID: user.ID, Name: "layers"})
	if err != nil {
		t.Fatal(err)
	}
	h := newResponsesWebSocketTestHarness(t)
	if _, err := repo.CreateAPIKey(ctx, store.CreateAPIKeyParams{UserID: user.ID, DeviceID: device.ID, Name: "layers", PublicID: h.database.publicID, KeyPrefix: h.database.keyPrefix, KeyHash: h.database.keyHash, SecretCiphertext: h.database.secretCiphertext, ModelAllowlist: []string{model}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	write := func() store.BillingWriteParams {
		return store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: user.ID, Reason: "funding layer test"}
	}
	subscription, err := repo.PutSubscription(ctx, store.PutSubscriptionParams{BillingWriteParams: write(), UserID: user.ID, Name: "Personal", ModelIDs: []string{model}, Tier: store.BillingTierDay, AllowanceUSD: "1"})
	if err != nil {
		t.Fatal(err)
	}
	const exclusive, reauth = "1122334455667788", "2233445566778899"
	accounts := repo.WithUpstreamProvider(store.UpstreamProviderAntigravity)
	if err := accounts.SyncUpstreamAccounts(ctx, []store.UpstreamAccountSnapshot{
		{ID: exclusive, DisplayName: "exclusive", MaskedEmail: "e***@example.com", Status: "available"},
		{ID: reauth, DisplayName: "reauth", MaskedEmail: "r***@example.com", Status: "available"},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `UPDATE upstream_accounts SET access_mode='exclusive' WHERE id=$1`, exclusive); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO upstream_account_users(upstream_account_id,user_id) VALUES($1,$2)`, exclusive, user.ID); err != nil {
		t.Fatal(err)
	}
	group := func(name string, pool ...string) store.Group {
		g, err := repo.PutGroup(ctx, store.PutGroupParams{BillingWriteParams: write(), Name: name, LimitUSD: "10", Period: "day", ModelIDs: []string{model}, ModelIDsSet: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.SetGroupMembers(ctx, store.SetGroupMembersParams{BillingWriteParams: write(), GroupID: g.ID, Action: "add", UserIDs: []string{user.ID}}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.SetGroupUpstreamAccounts(ctx, store.SetGroupUpstreamAccountsParams{BillingWriteParams: write(), GroupID: g.ID,
			Provider: store.UpstreamProviderAntigravity, Strategy: "selected", AccountIDs: pool}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	emptyPool := group("Empty pool")
	reauthPool := group("Reauth pool", reauth)

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
	h.server.config.AntigravityModelRoutes = map[string]string{model: model}
	h.server.config.Limits = config.Limits{KeyRPM: 100, UserRPM: 100, KeyConcurrent: 3, UserConcurrent: 3, GlobalConcurrent: 10, KeyRequestsPerDay: 100}

	var mu sync.Mutex
	layered := true
	var groupsHeader string
	generation := func(w http.ResponseWriter) {
		w.Header().Set("X-Codex-Upstream-Account", exclusive)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"` + model + `","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"output_tokens":37,"total_tokens":137}}`))
	}
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/internal/antigravity-accounts":
			writeJSON(w, 200, map[string]any{"accounts": []gatewayproxy.UpstreamAccount{
				{ID: exclusive, DisplayName: "exclusive", MaskedEmail: "e***@example.com", Plan: "unknown", Status: "available", CliproxyStatus: "active", GatewayManualStatus: "enabled", GatewayQuotaStatus: "available", LastSyncedAt: time.Now().UTC()},
				{ID: reauth, DisplayName: "reauth", MaskedEmail: "r***@example.com", Plan: "unknown", Status: "available", CliproxyStatus: "active", GatewayManualStatus: "enabled", GatewayQuotaStatus: "available", LastSyncedAt: time.Now().UTC()},
			}})
		case "/internal/antigravity-accounts/capabilities":
			writeJSON(w, 200, map[string]string{"protocol": "upstream_account_access_v1", "group_upstream_account_scope": gatewayproxy.GroupUpstreamAccountScopeV1})
		case "/internal/antigravity-accounts/funding-layers":
			if !layered {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, 200, map[string]string{"protocol": gatewayproxy.GroupUpstreamAccountLayersV1})
		case "/v1/responses":
			groupsHeader = r.Header.Get("X-Codex-Gateway-Groups")
			if !layered {
				// A pre-layer sidecar confines the request to the first group's
				// pool, which cannot serve it: CPA reports no available account.
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{
					"message": "upstream_concurrency_exceeded: upstream account concurrency limit reached", "type": "rate_limit_error", "code": "upstream_concurrency_exceeded"}})
				return
			}
			generation(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()
	base, _ := url.Parse(cpa.URL)
	h.server.antigravity = gatewayproxy.NewCPAAntigravity(base, "internal-cpa")
	send := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"hello"}`))
		r.Header.Set("Authorization", "Bearer "+h.apiKey)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		return w
	}
	wantLayers := `[{"group_id":"` + reauthPool.ID + `","account_ids":["` + reauth + `"]},{"group_id":"00000000-0000-0000-0000-000000000000","all_accounts":true}]`

	w := send()
	if w.Code != http.StatusOK {
		t.Fatalf("layered request status %d %s", w.Code, w.Body)
	}
	if groupsHeader != wantLayers {
		t.Fatalf("groups header = %s\nwant %s", groupsHeader, wantLayers)
	}
	requestID := w.Header().Get(httpx.RequestIDHeader)
	var ledgerGroup *string
	var account, groupCharged, personalCharged string
	if err := repo.DB().QueryRowContext(ctx, `SELECT l.group_id::text, l.upstream_account_id, l.group_charged_usd::text, l.personal_charged_usd::text
		FROM billing_ledger_entries l WHERE l.request_id=$1 AND l.entry_type='usage_charge'`, requestID).Scan(&ledgerGroup, &account, &groupCharged, &personalCharged); err != nil {
		t.Fatal(err)
	}
	if ledgerGroup != nil || account != exclusive || groupCharged != "0.000000000000" || personalCharged == "0.000000000000" {
		t.Fatalf("ledger group=%v account=%s group=%s personal=%s", ledgerGroup, account, groupCharged, personalCharged)
	}
	var subscriptionUsed bool
	if err := repo.DB().QueryRowContext(ctx, `SELECT remaining_usd < 1 FROM billing_subscription_periods WHERE subscription_id=$1`, subscription.ID).Scan(&subscriptionUsed); err != nil || !subscriptionUsed {
		t.Fatalf("personal subscription was not charged: %v", err)
	}
	for _, g := range []store.Group{emptyPool, reauthPool} {
		got, err := repo.GetGroup(ctx, g.ID)
		if err != nil || got.UsedUSD != "0.000000000000" {
			t.Fatalf("group %s used=%s %v", g.Name, got.UsedUSD, err)
		}
	}

	// A sidecar without the layer protocol receives the first group only, and
	// its "no account available" answer reaches the client as such.
	mu.Lock()
	layered = false
	mu.Unlock()
	w = send()
	if groupsHeader != `[{"group_id":"`+reauthPool.ID+`","account_ids":["`+reauth+`"]}]` {
		t.Fatalf("pre-layer sidecar header = %s", groupsHeader)
	}
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || w.Code != http.StatusTooManyRequests ||
		failure.Error.Code != "upstream_concurrency_exceeded" || failure.Error.Message != "暂无可用的上游账号，请稍后重试" {
		t.Fatalf("pre-layer failure %d %s", w.Code, w.Body)
	}
}
