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
		config: config.Config{RPOrigins: []string{"https://gateway.example"}, TokenPepper: []byte(strings.Repeat("p", 32)), ReauthMaxAge: 5 * time.Minute},
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
		"price_usd": "1.25", "tier": "day", "allowance_usd": "12", "min_period_count": 2, "active": true,
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
	send(http.MethodGet, "/admin/billing/plans?include_inactive=true", nil, memberToken, http.StatusForbidden)
	send(http.MethodGet, "/admin/billing/plans?include_inactive=true", nil, ownerToken, http.StatusOK)
	send(http.MethodPost, "/admin/billing/users/"+member.ID+"/adjustments", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Integration balance", "usd_amount": "100",
	}, ownerToken, http.StatusOK)
	send(http.MethodPut, "/admin/billing/users/"+member.ID+"/subscriptions/day", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Manual initial subscription", "quota_usd": "50", "period_count": 3,
	}, ownerToken, http.StatusOK)
	getSubscription := func() store.BillingSubscriptionState {
		t.Helper()
		state, err := repository.GetBillingState(ctx, member.ID, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, subscription := range state.Subscriptions {
			if subscription.Tier == "day" {
				return subscription
			}
		}
		t.Fatal("missing day subscription")
		return store.BillingSubscriptionState{}
	}
	manual := getSubscription()
	if manual.Plan != nil || manual.CanRenew || manual.ConfigVersion < 1 {
		t.Fatalf("manual binding: %+v", manual)
	}
	purchase := map[string]any{
		"operation_id": uuid.NewString(), "plan_id": plan.ID, "plan_version": plan.Version,
		"subscription_config_version": manual.ConfigVersion, "period_count": 2,
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
		t.Fatalf("purchase did not replace manual entitlement: %+v", bought)
	}
	replayed := send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusOK)
	if replayed.Body.String() != response.Body.String() {
		t.Fatalf("purchase replay changed response: %s != %s", replayed.Body, response.Body)
	}
	firstSnapshot := response.Body.String()
	renewal := map[string]any{
		"operation_id": uuid.NewString(), "plan_version": plan.Version,
		"subscription_config_version": bought.Subscription.ConfigVersion, "period_count": 1,
	}
	send(http.MethodPost, "/admin/billing/me/subscriptions/day/renewals", renewal, memberToken, http.StatusBadRequest)
	renewal["period_count"] = 99
	response = send(http.MethodPost, "/admin/billing/me/subscriptions/day/renewals", renewal, memberToken, http.StatusBadRequest)
	if !strings.Contains(response.Body.String(), "insufficient_balance") {
		t.Fatalf("expected insufficient balance: %s", response.Body)
	}
	// The failed operation rolls back its idempotency claim and can be retried
	// unchanged after funding the account.
	send(http.MethodPost, "/admin/billing/users/"+member.ID+"/adjustments", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Fund renewal", "usd_amount": "100",
	}, ownerToken, http.StatusOK)
	response = send(http.MethodPost, "/admin/billing/me/subscriptions/day/renewals", renewal, memberToken, http.StatusOK)
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
	unbound := getSubscription()
	if unbound.Plan != nil || unbound.CanRenew || unbound.PeriodCount != 101 || !unbound.ExpiresAt.Equal(*renewed.Subscription.ExpiresAt) || unbound.RemainingUSD != renewed.Subscription.RemainingUSD {
		t.Fatalf("plan edit changed purchased rights: %+v", unbound)
	}
	if got := send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusOK).Body.String(); got != firstSnapshot {
		t.Fatalf("edited plan broke successful purchase replay: %s != %s", got, firstSnapshot)
	}
	if got := send(http.MethodPost, "/admin/billing/me/subscriptions/day/renewals", renewal, memberToken, http.StatusOK).Body.String(); got != renewedSnapshot {
		t.Fatalf("edited plan broke renewal replay: %s != %s", got, renewedSnapshot)
	}
	renewal["operation_id"] = uuid.NewString()
	send(http.MethodPost, "/admin/billing/me/subscriptions/day/renewals", renewal, memberToken, http.StatusConflict)
	purchase["operation_id"] = uuid.NewString()
	send(http.MethodPost, "/admin/billing/me/purchases", purchase, memberToken, http.StatusConflict)
	ownerState, err := repository.GetBillingState(ctx, owner.ID, 100, 0)
	if err != nil || ownerState.BalanceUSD != "0.000000000000" {
		t.Fatalf("member purchase affected owner: %+v %v", ownerState, err)
	}
}
