//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

// Exercise the real routes, authentication, provider selection and database
// queries together; replace only the private bridge's account management API.
func TestAntigravityAccountManagementPostgresIntegration(t *testing.T) {
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
	schema := "agy_accounts_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := repository.CreateUser(ctx, store.CreateUserParams{Username: "agy-owner", DisplayName: "AGY owner", Role: store.UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	member, err := repository.CreateUser(ctx, store.CreateUserParams{Username: "agy-member", DisplayName: "AGY member", Role: store.UserRoleMember})
	if err != nil {
		t.Fatal(err)
	}
	const codexID, agyID = "1111111111111111", "2222222222222222"
	now := time.Now().UTC()
	if err := repository.SyncUpstreamAccounts(ctx, []store.UpstreamAccountSnapshot{{
		ID: codexID, MaskedEmail: "c***@example.com", Plan: "plus", Status: "available",
	}}, now); err != nil {
		t.Fatal(err)
	}
	var enabled, unavailable atomic.Bool
	var bridgeCalls atomic.Int64
	enabled.Store(true)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer agy-integration-secret" {
			t.Error("Antigravity management used the wrong upstream credentials")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_api_key"})
			return
		}
		if unavailable.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "upstream_unavailable"})
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/internal/upstream-accounts/"+agyID+"/status" {
			var input struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			enabled.Store(input.Enabled)
		}
		status, manual := "available", "enabled"
		if !enabled.Load() {
			status, manual = "unavailable", "manual_disabled"
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/upstream-accounts":
			writeJSON(w, http.StatusOK, map[string]any{"accounts": []gatewayproxy.UpstreamAccount{{
				ID: agyID, DisplayName: "primary", Plan: "unknown", Status: status,
				CliproxyStatus: "active", GatewayManualStatus: manual, GatewayQuotaStatus: "available", LastSyncedAt: time.Now().UTC(),
			}}})
		case "GET /internal/upstream-accounts/concurrency":
			writeJSON(w, http.StatusOK, gatewayproxy.UpstreamConcurrency{SampledAt: time.Now().UTC(), Accounts: []gatewayproxy.UpstreamAccountConcurrency{{ID: agyID, ActiveRequests: 2}}})
		case "PUT /internal/upstream-accounts/" + agyID + "/status":
			writeJSON(w, http.StatusOK, gatewayproxy.UpstreamAccountStatus{ID: agyID, Status: status, CliproxyStatus: "active", GatewayManualStatus: manual, GatewayQuotaStatus: "available"})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "upstream_account_not_found"})
		}
	}))
	defer bridge.Close()
	bridgeURL, _ := url.Parse(bridge.URL)
	s := &Server{
		store: repository, mux: http.NewServeMux(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: config.Config{RPOrigins: []string{"https://gateway.test"}, TokenPepper: []byte(strings.Repeat("p", 32)), ReauthMaxAge: 5 * time.Minute,
			SidecarToken: "codex-integration-secret", AntigravityBridgeToken: "agy-integration-secret"},
		antigravity: gatewayproxy.NewAntigravity(bridgeURL, "agy-integration-secret"),
		upstream:    gatewayproxy.New(bridgeURL, "codex-integration-secret"),
	}
	s.routes()
	newSession := func(userID string, verified bool) string {
		t.Helper()
		token, err := security.GenerateOpaqueToken(security.SessionToken)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := security.PepperTokenDigest(s.config.TokenPepper, token.Digest)
		if err != nil {
			t.Fatal(err)
		}
		session, err := repository.CreateSession(ctx, store.CreateSessionParams{UserID: userID, TokenHash: digest[:], CSRFSecret: []byte(strings.Repeat("c", 32)), CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if verified {
			if err := repository.MarkSessionVerified(ctx, session.ID, now); err != nil {
				t.Fatal(err)
			}
		}
		return token.Token
	}
	ownerSession, memberSession, unverifiedSession := newSession(owner.ID, true), newSession(member.ID, true), newSession(owner.ID, false)
	send := func(method, path, body, session string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://gateway.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://gateway.test")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	const base = "/admin/antigravity-accounts"
	list := func() upstreamAccountsResponse {
		t.Helper()
		w := send(http.MethodGet, base, "", ownerSession)
		var result upstreamAccountsResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Accounts) != 1 || result.Accounts[0].ID != agyID {
			t.Fatalf("AGY account list: %d %s", w.Code, w.Body)
		}
		return result
	}
	initial := list()
	if row := initial.Accounts[0]; row.DisplayName != "primary" || row.EmailMasked != "" || !row.CanManage || row.Status != "available" || row.ConcurrentLimit != 1 || row.AllocationWeight != 1 || row.AccessMode != "shared" {
		t.Fatalf("AGY slot display/default controls = %+v", row)
	}
	codex, err := repository.ListUpstreamAccounts(ctx)
	if err != nil || len(codex) != 1 || codex[0].ID != codexID || codex[0].Status != "available" {
		t.Fatalf("AGY list changed Codex snapshot: %+v %v", codex, err)
	}
	for _, fixture := range []struct{ id, cost string }{{codexID, "9"}, {agyID, "2"}} {
		if _, err := repository.DB().ExecContext(ctx, `INSERT INTO billing_ledger_entries
			(user_id,entry_type,amount_usd,cash_delta_usd,request_id,upstream_account_id,model,actual_cost_usd,charged_usd,group_charged_usd,personal_charged_usd,uncovered_usd,usage_requested_at,created_at)
			VALUES ($1,'usage_charge',$2::numeric,0,$3,$4,'management-fixture',$2::numeric,0,0,0,$2::numeric,$5,$5)`, owner.ID, fixture.cost, uuid.NewString(), fixture.id, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if row := list().Accounts[0]; row.EquivalentCostUSD != "2.000000000000" || row.RollingCostUSD != "2.000000000000" {
		t.Fatalf("AGY API included Codex usage costs: %+v", row)
	}
	for _, control := range []struct{ path, body string }{
		{"allocation-weight", `{"weight":7}`},
		{"concurrent-limit", `{"concurrent_limit":3}`},
		{"access", fmt.Sprintf(`{"mode":"exclusive","user_ids":[%q],"reason":"Dedicated AGY account"}`, owner.ID)},
	} {
		for _, id := range []string{agyID, codexID} {
			w := send(http.MethodPut, base+"/"+id+"/"+control.path, control.body, ownerSession)
			want := http.StatusOK
			if id == codexID {
				want = http.StatusNotFound
			}
			if w.Code != want {
				t.Fatalf("AGY %s account=%s: %d %s", control.path, id, w.Code, w.Body)
			}
		}
	}
	if row := list().Accounts[0]; row.AllocationWeight != 7 || row.ConcurrentLimit != 3 || row.AccessMode != "exclusive" || len(row.AuthorizedUserIDs) != 1 || row.AuthorizedUserIDs[0] != owner.ID {
		t.Fatalf("AGY API preference round trip = %+v", row)
	}
	for _, enabled := range []bool{false, true} {
		w := send(http.MethodPut, base+"/"+agyID+"/status", fmt.Sprintf(`{"enabled":%t}`, enabled), ownerSession)
		if w.Code != http.StatusOK {
			t.Fatalf("AGY status edit: %d %s", w.Code, w.Body)
		}
		want := "unavailable"
		if enabled {
			want = "available"
		}
		if row := list().Accounts[0]; row.Status != want {
			t.Fatalf("AGY status confirmation = %+v", row)
		}
	}
	w := send(http.MethodPut, base+"/"+codexID+"/status", `{"enabled":false}`, ownerSession)
	if w.Code != http.StatusNotFound {
		t.Fatalf("AGY status route accepted a Codex account: %d %s", w.Code, w.Body)
	}
	w = send(http.MethodGet, base+"/concurrency", "", ownerSession)
	var concurrency gatewayproxy.UpstreamConcurrency
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &concurrency) != nil || len(concurrency.Accounts) != 1 || concurrency.Accounts[0].ID != agyID || concurrency.Accounts[0].ActiveRequests != 2 {
		t.Fatalf("AGY live concurrency: %d %s", w.Code, w.Body)
	}
	// Verify callback bearer selection and SQL scope with both providers in
	// the candidate set, including access denial and a foreign-only candidate.
	for _, tc := range []struct {
		provider, token, action, user string
		ids                           []string
		want                          int
		selected                      string
	}{
		{"antigravity", "agy-integration-secret", "select", owner.ID, []string{codexID, agyID}, 200, agyID},
		{"upstream", "codex-integration-secret", "select", owner.ID, []string{codexID, agyID}, 200, codexID},
		{"antigravity", "codex-integration-secret", "select", owner.ID, []string{agyID}, 401, ""},
		{"antigravity", "agy-integration-secret", "select", owner.ID, []string{codexID}, 429, ""},
		{"antigravity", "agy-integration-secret", "select", member.ID, []string{agyID}, 429, ""},
		{"antigravity", "agy-integration-secret", "eligible", owner.ID, []string{codexID, agyID}, 200, agyID},
	} {
		body, _ := json.Marshal(map[string]any{"user_id": tc.user, "account_ids": tc.ids})
		r := httptest.NewRequest(http.MethodPost, "/internal/"+tc.provider+"-accounts/"+tc.action, strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != tc.want || (tc.selected != "" && !strings.Contains(w.Body.String(), tc.selected)) {
			t.Fatalf("%s %s callback: %d %s", tc.provider, tc.action, w.Code, w.Body)
		}
		if tc.action == "eligible" && (strings.Contains(w.Body.String(), codexID) || !strings.Contains(w.Body.String(), `"concurrent_limit":3`)) {
			t.Fatalf("AGY eligibility scope/limit: %s", w.Body)
		}
	}
	beforeDenied := bridgeCalls.Load()
	for _, control := range []struct{ method, path, body string }{
		{http.MethodGet, base, ""}, {http.MethodGet, base + "/concurrency", ""},
		{http.MethodPut, base + "/" + agyID + "/status", `{"enabled":false}`},
		{http.MethodPut, base + "/" + agyID + "/allocation-weight", `{"weight":0}`},
		{http.MethodPut, base + "/" + agyID + "/concurrent-limit", `{"concurrent_limit":9}`},
		{http.MethodPut, base + "/" + agyID + "/access", `{"mode":"shared","user_ids":[],"reason":"denied"}`},
	} {
		w := send(control.method, control.path, control.body, memberSession)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "owner_required") {
			t.Fatalf("member reached AGY management: %d %s", w.Code, w.Body)
		}
	}
	w = send(http.MethodPut, base+"/"+agyID+"/status", `{"enabled":false}`, unverifiedSession)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "recent_identity_verification_required") || bridgeCalls.Load() != beforeDenied {
		t.Fatalf("AGY privilege/verification guard: %d %s", w.Code, w.Body)
	}
	foreign := httptest.NewRequest(http.MethodPut, "https://gateway.test"+base+"/"+agyID+"/status", strings.NewReader(`{"enabled":false}`))
	foreign.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ownerSession})
	foreign.Header.Set("Content-Type", "application/json")
	foreign.Header.Set("Origin", "https://foreign.test")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, foreign)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "invalid_origin") || bridgeCalls.Load() != beforeDenied {
		t.Fatalf("AGY foreign-origin guard: %d %s", w.Code, w.Body)
	}
	// A bridge outage preserves historical display/statistics but must never
	// expose the stale 'available' status or manufacture a zero live count.
	unavailable.Store(true)
	fallback := list()
	if row := fallback.Accounts[0]; fallback.SyncWarning == "" || row.CanManage || row.Status != "unknown" || row.DisplayName != "primary" || row.EquivalentCostUSD != "2.000000000000" {
		t.Fatalf("AGY unsafe history fallback: %+v", fallback)
	}
	w = send(http.MethodGet, base+"/concurrency", "", ownerSession)
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "active_requests") {
		t.Fatalf("AGY outage became a live concurrency count: %d %s", w.Code, w.Body)
	}
}
