//go:build integration

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestBillingPlansPostgresHTTPIntegration(t *testing.T) {
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
	schema := "billing_plans_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := repository.CreateUser(ctx, store.CreateUserParams{Username: "plans-owner", DisplayName: "Owner", Role: store.UserRoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	member, err := repository.CreateUser(ctx, store.CreateUserParams{Username: "plans-member", DisplayName: "Member", Role: store.UserRoleMember})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store: repository, mux: http.NewServeMux(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: config.Config{UsagePricing: config.UsagePricing{Models: map[string]config.ModelPricing{"gpt-test": {}, "claude-test": {}}}, RPOrigins: []string{"https://gateway.example"}, TokenPepper: []byte(strings.Repeat("p", 32)), ReauthMaxAge: 5 * time.Minute},
	}
	if err := repository.SyncModelAccessCatalog(ctx, s.config.UsagePricing.ManageableModelNames()); err != nil {
		t.Fatal(err)
	}
	s.routes()
	now := time.Now().UTC()
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
		session, err := repository.CreateSession(ctx, store.CreateSessionParams{
			UserID: userID, TokenHash: digest[:], CSRFSecret: []byte(strings.Repeat("c", 32)),
			CreatedAt: now, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour),
		})
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
	ownerToken, memberToken := newSession(owner.ID, true), newSession(member.ID, true)
	send := func(method, path string, body any, token string, status int) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := billingPlanTestRequest(method, path, string(raw))
		if token != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, status, w.Body)
		}
		return w
	}
	planBody := map[string]any{
		"operation_id": uuid.NewString(), "reason": "Integration plan launch", "name": "Daily",
		"price_usd": "1.25", "tier": "day", "model_ids": []string{"gpt-test"}, "allowance_usd": "12", "min_period_count": 2, "active": true,
	}
	send(http.MethodPost, "/admin/billing/plans", planBody, memberToken, http.StatusForbidden)
	response := send(http.MethodPost, "/admin/billing/plans", planBody, ownerToken, http.StatusOK)
	var plan store.BillingPlan
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.ID == "" || plan.Version < 1 || plan.PriceUSD != "1.250000000000" {
		t.Fatalf("plan response: %+v", plan)
	}
	// Catalog membership is checked for new writes, after immutable replay.
	firstPlanSnapshot := response.Body.String()
	delete(s.config.UsagePricing.Models, "gpt-test")
	if err := repository.SyncModelAccessCatalog(ctx, s.config.UsagePricing.ManageableModelNames()); err != nil {
		t.Fatal(err)
	}
	if got := send(http.MethodPost, "/admin/billing/plans", planBody, ownerToken, http.StatusOK).Body.String(); got != firstPlanSnapshot {
		t.Fatal("catalog edit changed committed plan response")
	}
	previousPlanOperation := planBody["operation_id"]
	planBody["operation_id"] = uuid.NewString()
	send(http.MethodPost, "/admin/billing/plans", planBody, ownerToken, http.StatusBadRequest)
	planBody["operation_id"] = previousPlanOperation
	s.config.UsagePricing.Models["gpt-test"] = config.ModelPricing{}
	if err := repository.SyncModelAccessCatalog(ctx, s.config.UsagePricing.ManageableModelNames()); err != nil {
		t.Fatal(err)
	}
	send(http.MethodGet, "/admin/billing/plans?include_inactive=true", nil, memberToken, http.StatusForbidden)
	send(http.MethodGet, "/admin/billing/plans?include_inactive=true", nil, ownerToken, http.StatusOK)
	send(http.MethodPost, "/admin/billing/users/"+member.ID+"/adjustments", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Integration balance", "usd_amount": "100",
	}, ownerToken, http.StatusOK)
	send(http.MethodPost, "/admin/billing/users/"+member.ID+"/subscriptions", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Manual initial subscription", "name": "Manual", "tier": "day", "model_ids": []string{"gpt-test"}, "quota_usd": "50", "period_count": 3,
	}, ownerToken, http.StatusOK)
	getSubscription := func(id string) store.BillingSubscriptionState {
		t.Helper()
		state, err := repository.GetBillingState(ctx, member.ID, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, subscription := range state.Subscriptions {
			if id == "" || subscription.ID == id {
				return subscription
			}
		}
		t.Fatal("missing day subscription")
		return store.BillingSubscriptionState{}
	}
	manual := getSubscription("")
	if manual.Plan != nil || manual.CanRenew || manual.ConfigVersion < 1 {
		t.Fatalf("manual binding: %+v", manual)
	}
	purchase := map[string]any{
		"operation_id": uuid.NewString(), "plan_id": plan.ID, "plan_version": plan.Version,
		"period_count": 2,
	}
	send(http.MethodPost, "/admin/billing/me/purchases", purchase, "", http.StatusUnauthorized)
	send(http.MethodPost, "/admin/billing/me/purchases", purchase, newSession(member.ID, false), http.StatusForbidden)
	purchase["user_id"] = owner.ID
	send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusBadRequest)
	delete(purchase, "user_id")
	response = send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusOK)
	var bought store.BillingPlanTransaction
	if err := json.Unmarshal(response.Body.Bytes(), &bought); err != nil {
		t.Fatal(err)
	}
	if bought.BalanceUSD != "97.500000000000" || bought.TotalUSD != "2.500000000000" || bought.Subscription.Plan == nil || !bought.Subscription.CanRenew || bought.Subscription.RemainingUSD != "12.000000000000" || bought.Subscription.PeriodCount != 2 || bought.Subscription.CurrentPeriodNumber != 1 {
		t.Fatalf("purchase did not create independent entitlement: %+v", bought)
	}
	if bought.Subscription.ID == manual.ID || getSubscription(manual.ID).RemainingUSD != "50.000000000000" {
		t.Fatal("purchase replaced existing subscription")
	}
	duplicatePurchase := map[string]any{"operation_id": uuid.NewString(), "plan_id": plan.ID, "plan_version": plan.Version, "period_count": 2}
	duplicateResponse := send(http.MethodPost, "/admin/billing/me/purchases", duplicatePurchase, memberToken, http.StatusOK)
	var duplicate store.BillingPlanTransaction
	if err := json.Unmarshal(duplicateResponse.Body.Bytes(), &duplicate); err != nil || duplicate.Subscription.ID == bought.Subscription.ID {
		t.Fatal("duplicate purchase did not create new subscription", err)
	}
	send(http.MethodPut, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/preferences", map[string]any{"operation_id": uuid.NewString(), "priority": 42, "charge_enabled": false}, memberToken, http.StatusOK)
	send(http.MethodPut, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/preferences", map[string]any{"operation_id": uuid.NewString(), "priority": 2, "charge_enabled": true}, ownerToken, http.StatusNotFound)
	replayed := send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusOK)
	if replayed.Body.String() != response.Body.String() {
		t.Fatalf("purchase replay changed response: %s != %s", replayed.Body, response.Body)
	}
	firstSnapshot := response.Body.String()
	renewal := map[string]any{
		"operation_id": uuid.NewString(), "plan_version": plan.Version,
		"subscription_config_version": bought.Subscription.ConfigVersion, "period_count": 1,
	}
	send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusBadRequest)
	renewal["period_count"] = 99
	response = send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusBadRequest)
	if !strings.Contains(response.Body.String(), "insufficient_balance") {
		t.Fatalf("expected insufficient balance: %s", response.Body)
	}
	// The failed operation rolls back its idempotency claim and can be retried
	// unchanged after funding the account.
	send(http.MethodPost, "/admin/billing/users/"+member.ID+"/adjustments", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Fund renewal", "usd_amount": "100",
	}, ownerToken, http.StatusOK)
	response = send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusOK)
	var renewed store.BillingPlanTransaction
	if err := json.Unmarshal(response.Body.Bytes(), &renewed); err != nil {
		t.Fatal(err)
	}
	if renewed.Subscription.PeriodCount != 101 || renewed.Subscription.CurrentPeriodNumber != 1 || renewed.Subscription.RemainingUSD != bought.Subscription.RemainingUSD || !reflect.DeepEqual(renewed.Subscription.PeriodID, bought.Subscription.PeriodID) || !renewed.Subscription.ExpiresAt.Equal(bought.Subscription.ExpiresAt.Add(99*24*time.Hour)) {
		t.Fatalf("renewal reset active period: bought=%+v renewed=%+v", bought.Subscription, renewed.Subscription)
	}
	renewedSnapshot := response.Body.String()
	planBody["operation_id"], planBody["version"], planBody["name"] = uuid.NewString(), plan.Version, "Daily revised"
	send(http.MethodPut, "/admin/billing/plans/"+plan.ID, planBody, ownerToken, http.StatusOK)
	bound := getSubscription(bought.Subscription.ID)
	if bound.Plan == nil || !bound.CanRenew || bound.PeriodCount != 101 || !bound.ExpiresAt.Equal(*renewed.Subscription.ExpiresAt) || bound.RemainingUSD != renewed.Subscription.RemainingUSD || bound.Priority != 42 || bound.ChargeEnabled {
		t.Fatalf("plan edit changed purchased rights: %+v", bound)
	}
	if got := send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusOK).Body.String(); got != firstSnapshot {
		t.Fatalf("edited plan broke successful purchase replay: %s != %s", got, firstSnapshot)
	}
	if got := send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusOK).Body.String(); got != renewedSnapshot {
		t.Fatalf("edited plan broke renewal replay: %s != %s", got, renewedSnapshot)
	}
	renewal["operation_id"] = uuid.NewString()
	send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusConflict)
	purchase["operation_id"] = uuid.NewString()
	send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusConflict)
	// Changing all commercial terms keeps the binding and preserves purchased rights.
	planBody["operation_id"], planBody["version"], planBody["tier"], planBody["period_days"] = uuid.NewString(), bound.Plan.Version, "custom", 15
	planBody["model_ids"], planBody["allowance_usd"], planBody["price_usd"], planBody["active"] = []string{"claude-test"}, "25", "0.5", false
	editedResponse := send(http.MethodPut, "/admin/billing/plans/"+plan.ID, planBody, ownerToken, http.StatusOK)
	var edited store.BillingPlan
	if err := json.Unmarshal(editedResponse.Body.Bytes(), &edited); err != nil {
		t.Fatal(err)
	}
	beforeAppend := getSubscription(bought.Subscription.ID)
	renewal["operation_id"], renewal["plan_version"], renewal["subscription_config_version"], renewal["period_count"] = uuid.NewString(), edited.Version, beforeAppend.ConfigVersion, 2
	appendedResponse := send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusOK)
	var appended store.BillingPlanTransaction
	if err := json.Unmarshal(appendedResponse.Body.Bytes(), &appended); err != nil {
		t.Fatal(err)
	}
	if appended.Subscription.Tier != "day" || appended.Subscription.PeriodDays != 1 || !reflect.DeepEqual(appended.Subscription.ModelIDs, []string{"gpt-test"}) || appended.Subscription.RemainingUSD != bought.Subscription.RemainingUSD || len(appended.Subscription.Entitlements) != 3 || !appended.Subscription.ExpiresAt.Equal(beforeAppend.ExpiresAt.Add(30*24*time.Hour)) {
		t.Fatalf("append changed current rights: %+v", appended.Subscription)
	}
	send(http.MethodDelete, "/admin/billing/plans/"+plan.ID, map[string]any{"operation_id": uuid.NewString(), "reason": "Retire", "version": edited.Version}, ownerToken, http.StatusOK)
	unbound := getSubscription(bought.Subscription.ID)
	if unbound.Plan != nil || unbound.CanRenew || unbound.RemainingUSD != appended.Subscription.RemainingUSD || !unbound.ExpiresAt.Equal(*appended.Subscription.ExpiresAt) {
		t.Fatalf("deletion changed purchased rights: %+v", unbound)
	}
	if got := send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusOK).Body.String(); got != appendedResponse.Body.String() {
		t.Fatal("deleted plan changed historical response")
	}
	renewal["operation_id"] = uuid.NewString()
	send(http.MethodPost, "/admin/billing/me/subscriptions/"+bought.Subscription.ID+"/renewals", renewal, memberToken, http.StatusConflict)
	// Reopening targets exactly one independent subscription and removes all future rights.
	send(http.MethodPut, "/admin/billing/users/"+member.ID+"/subscriptions/"+bought.Subscription.ID, map[string]any{"operation_id": uuid.NewString(), "reason": "Replace rights", "name": "Reopened", "tier": "week", "period_count": 0, "quota_usd": "9", "model_ids": []string{"claude-test"}}, ownerToken, http.StatusOK)
	reopened := getSubscription(bought.Subscription.ID)
	if reopened.Plan != nil || reopened.CanRenew || reopened.ExpiresAt != nil || len(reopened.Entitlements) != 1 || getSubscription(manual.ID).RemainingUSD != "50.000000000000" {
		t.Fatalf("reopen did not isolate subscription: %+v", reopened)
	}
	send(http.MethodDelete, "/admin/billing/users/"+owner.ID+"/subscriptions/"+bought.Subscription.ID, map[string]any{"operation_id": uuid.NewString(), "reason": "Cross user"}, ownerToken, http.StatusNotFound)
	send(http.MethodDelete, "/admin/billing/users/"+member.ID+"/subscriptions/"+bought.Subscription.ID, map[string]any{"operation_id": uuid.NewString(), "reason": "Disable"}, ownerToken, http.StatusOK)
	visible, err := repository.GetBillingState(ctx, member.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, subscription := range visible.Subscriptions {
		if subscription.ID == bought.Subscription.ID {
			t.Fatal("disabled subscription remains visible")
		}
	}
	ownerState, err := repository.GetBillingState(ctx, owner.ID, 100, 0)
	if err != nil || ownerState.BalanceUSD != "0.000000000000" {
		t.Fatalf("member purchase affected owner: %+v %v", ownerState, err)
	}
}
