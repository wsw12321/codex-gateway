package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

const planTestOperationID = "550e8400-e29b-41d4-a716-446655440000"
const planTestPurchase = `{"operation_id":"` + planTestOperationID + `","plan_id":"plan-1","plan_version":2,"subscription_config_version":0,"period_count":3}`
const planTestRenewal = `{"operation_id":"` + planTestOperationID + `","plan_version":2,"subscription_config_version":7,"period_count":3}`
const planTestCreate = `{"operation_id":"` + planTestOperationID + `","reason":"Launch offer","name":"Daily","price_usd":"1.234567","tier":"day","allowance_usd":"8.765432"}`

type billingPlanTestRepository struct {
	plans           []store.BillingPlan
	includeInactive []bool
	creates         []store.CreateBillingPlanParams
	updates         []store.UpdateBillingPlanParams
	purchases       []store.PurchaseBillingPlanParams
	renewals        []store.RenewBillingSubscriptionParams
	transaction     store.BillingPlanTransaction
	err             error
}

func (r *billingPlanTestRepository) ListBillingPlans(_ context.Context, includeInactive bool) ([]store.BillingPlan, error) {
	r.includeInactive = append(r.includeInactive, includeInactive)
	return r.plans, r.err
}

func (r *billingPlanTestRepository) CreateBillingPlan(_ context.Context, p store.CreateBillingPlanParams) (store.BillingPlan, error) {
	r.creates = append(r.creates, p)
	return store.BillingPlan{ID: "plan-1", Name: p.Name, PriceUSD: p.PriceUSD, Tier: p.Tier, AllowanceUSD: p.AllowanceUSD, MinPeriodCount: p.MinPeriodCount, Active: p.Active, Version: 1}, r.err
}

func (r *billingPlanTestRepository) UpdateBillingPlan(_ context.Context, p store.UpdateBillingPlanParams) (store.BillingPlan, error) {
	r.updates = append(r.updates, p)
	return store.BillingPlan{ID: p.PlanID, Name: p.Name, PriceUSD: p.PriceUSD, Tier: p.Tier, AllowanceUSD: p.AllowanceUSD, MinPeriodCount: p.MinPeriodCount, Active: p.Active, Version: p.Version + 1}, r.err
}

func (r *billingPlanTestRepository) PurchaseBillingPlan(_ context.Context, p store.PurchaseBillingPlanParams) (store.BillingPlanTransaction, error) {
	r.purchases = append(r.purchases, p)
	return r.transaction, r.err
}

func (r *billingPlanTestRepository) RenewBillingSubscription(_ context.Context, p store.RenewBillingSubscriptionParams) (store.BillingPlanTransaction, error) {
	r.renewals = append(r.renewals, p)
	return r.transaction, r.err
}

func billingPlanTestRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://gateway.example")
	return r
}

func TestBillingPlanWriteRoutesEnforceBrowserSessionRecentVerificationAndOwner(t *testing.T) {
	for _, route := range []struct {
		method, path string
		ownerOnly    bool
	}{
		{http.MethodPost, "/admin/billing/plans", true},
		{http.MethodPut, "/admin/billing/plans/plan-1", true},
		{http.MethodPost, "/admin/billing/me/purchases", false},
		{http.MethodPost, "/admin/billing/me/subscriptions/day/renewals", false},
	} {
		for _, test := range []struct {
			name, origin, site, role, code string
			cookie, verified               bool
			age                            time.Duration
			status                         int
		}{
			{name: "no origin", code: "invalid_origin", status: 403},
			{name: "foreign origin", origin: "https://foreign.example", code: "invalid_origin", status: 403},
			{name: "cross site", origin: "https://gateway.example", site: "cross-site", code: "cross_site_request", status: 403},
			{name: "no session", origin: "https://gateway.example", code: "session_required", status: 401},
			{name: "unverified", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, code: "recent_identity_verification_required", status: 403},
			{name: "expired verification", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, verified: true, age: 6 * time.Minute, code: "recent_identity_verification_required", status: 403},
			{name: "verified member", origin: "https://gateway.example", cookie: true, role: store.UserRoleMember, verified: true, code: "invalid_billing_operation", status: 400},
			{name: "verified owner", origin: "https://gateway.example", cookie: true, role: store.UserRoleOwner, verified: true, code: "invalid_billing_operation", status: 400},
		} {
			t.Run(route.path+"/"+test.name, func(t *testing.T) {
				var verified *time.Time
				if test.verified {
					at := time.Now().Add(-test.age)
					verified = &at
				}
				s, conn := newBillingSourceTestServer(t, test.role, verified)
				r := billingPlanTestRequest(route.method, route.path, `{}`)
				r.Header.Set("Origin", test.origin)
				r.Header.Set("Sec-Fetch-Site", test.site)
				if test.cookie {
					addBillingSourceTestSession(t, r)
				}
				status, code := test.status, test.code
				if route.ownerOnly && test.name == "verified member" {
					status, code = http.StatusForbidden, "owner_required"
				}
				w := httptest.NewRecorder()
				s.mux.ServeHTTP(w, r)
				if w.Code != status || !strings.Contains(w.Body.String(), code) || len(conn.writes) != 0 {
					t.Fatalf("status=%d body=%s writes=%v", w.Code, w.Body, conn.writes)
				}
			})
		}
	}
}

func TestBillingPlansCatalogLimitsInactivePlansToOwner(t *testing.T) {
	for _, test := range []struct {
		name, role, query string
		cookie            bool
		status            int
		includeInactive   bool
	}{
		{name: "anonymous", status: 401},
		{name: "member catalog", role: store.UserRoleMember, cookie: true, status: 200},
		{name: "owner catalog defaults active", role: store.UserRoleOwner, cookie: true, status: 200},
		{name: "owner all", role: store.UserRoleOwner, cookie: true, query: "?include_inactive=true", status: 200, includeInactive: true},
		{name: "member inactive forbidden", role: store.UserRoleMember, cookie: true, query: "?include_inactive=true", status: 403},
		{name: "member explicit active", role: store.UserRoleMember, cookie: true, query: "?include_inactive=false", status: 200},
		{name: "unknown query", role: store.UserRoleOwner, cookie: true, query: "?user_id=other", status: 400},
		{name: "duplicate query", role: store.UserRoleOwner, cookie: true, query: "?include_inactive=true&include_inactive=false", status: 400},
		{name: "ambiguous bool", role: store.UserRoleOwner, cookie: true, query: "?include_inactive=1", status: 400},
		{name: "malformed query", role: store.UserRoleOwner, cookie: true, query: "?include_inactive=true&ignored=%zz", status: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newBillingSourceTestServer(t, test.role, nil)
			repo := &billingPlanTestRepository{}
			s.billingPlanRepo = repo
			r := billingPlanTestRequest(http.MethodGet, "/admin/billing/plans"+test.query, "")
			if test.cookie {
				addBillingSourceTestSession(t, r)
			}
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if test.status == http.StatusOK {
				if !reflect.DeepEqual(repo.includeInactive, []bool{test.includeInactive}) || strings.TrimSpace(w.Body.String()) != `{"plans":[]}` {
					t.Fatalf("query=%v body=%s", repo.includeInactive, w.Body)
				}
			} else if len(repo.includeInactive) != 0 {
				t.Fatal("invalid request reached catalog storage")
			}
		})
	}
}

func TestBillingPlanTransactionsRejectIdentityAmountAndConfirmationOverrides(t *testing.T) {
	for _, purchasing := range []bool{true, false} {
		body := planTestRenewal
		if purchasing {
			body = planTestPurchase
		}
		for _, field := range []string{
			`"user_id":"other"`, `"actor_user_id":"other"`, `"reason":"client reason"`,
			`"price_usd":"0.000001"`, `"total_usd":"0.000001"`, `"period_count":1`,
			`"Period_Count":1`, `"\u0070eriod_count":1`,
		} {
			t.Run(body+field, func(t *testing.T) {
				r := billingPlanTestRequest(http.MethodPost, "/", strings.TrimSuffix(body, "}")+","+field+"}")
				w := httptest.NewRecorder()
				if _, ok := (&Server{}).decodeBillingPlanTransaction(w, r, purchasing); ok || w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_json") {
					t.Fatalf("override accepted: %d %s", w.Code, w.Body)
				}
			})
		}
		for _, invalid := range []string{
			``, `{}`, `null`, `[]`, body + `{}`, strings.Replace(body, `"period_count":3`, `"period_count":null`, 1),
			strings.Replace(body, `"period_count":3`, `"period_count":0`, 1),
			strings.Replace(body, `"period_count":3`, `"period_count":100`, 1),
			strings.Replace(body, `"period_count":3`, `"period_count":1.5`, 1),
			strings.Replace(body, `"period_count":3`, `"period_count":"3"`, 1),
			strings.Replace(body, `"plan_version":2`, `"plan_version":0`, 1),
			strings.Replace(body, `"plan_version":2`, `"plan_version":null`, 1),
			strings.Replace(body, planTestOperationID, "invalid-id", 1),
			strings.Replace(body, `"subscription_config_version":`, `"subscription_config_version":-`, 1),
		} {
			// Negative zero has the same integer value as zero and is valid for
			// a purchase with no previous subscription.
			if purchasing && strings.Contains(invalid, `"subscription_config_version":-0`) {
				continue
			}
			t.Run(invalid, func(t *testing.T) {
				w := httptest.NewRecorder()
				r := billingPlanTestRequest(http.MethodPost, "/", invalid)
				if _, ok := (&Server{}).decodeBillingPlanTransaction(w, r, purchasing); ok || w.Code != 400 {
					t.Fatalf("invalid purchase accepted: %d %s", w.Code, w.Body)
				}
			})
		}
	}
}

func TestBillingPlanTransactionProtocolLimits(t *testing.T) {
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		func(r *http.Request) { r.URL.RawQuery = "user_id=other" },
	} {
		r := billingPlanTestRequest(http.MethodPost, "/", planTestPurchase)
		mutate(r)
		w := httptest.NewRecorder()
		if _, ok := (&Server{}).decodeBillingPlanTransaction(w, r, true); ok || w.Code != 400 {
			t.Fatalf("bad protocol accepted: %d %s", w.Code, w.Body)
		}
	}
	for _, length := range []int64{-1, 33 << 10} {
		r := billingPlanTestRequest(http.MethodPost, "/", planTestPurchase+strings.Repeat(" ", 33<<10))
		r.ContentLength = length
		w := httptest.NewRecorder()
		if _, ok := (&Server{}).decodeBillingPlanTransaction(w, r, true); ok || w.Code != 413 {
			t.Fatalf("oversized body accepted: %d %s", w.Code, w.Body)
		}
	}
}

func TestBillingPlanCreationDefaultsAndDecimalStrings(t *testing.T) {
	now := time.Now()
	s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
	repo := &billingPlanTestRepository{}
	s.billingPlanRepo = repo
	r := billingPlanTestRequest(http.MethodPost, "/admin/billing/plans", planTestCreate)
	addBillingSourceTestSession(t, r)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != 200 || len(repo.creates) != 1 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	p := repo.creates[0]
	if p.MinPeriodCount != 1 || p.Active || p.ActorUserID != "self-1" || p.ActorSessionID != "session-1" || p.PriceUSD != "1.234567" || p.AllowanceUSD != "8.765432" {
		t.Fatalf("creation defaults/attribution: %+v", p)
	}
	for _, bad := range []string{
		strings.Replace(planTestCreate, `"price_usd":"1.234567"`, `"price_usd":1.234567`, 1),
		strings.Replace(planTestCreate, `"allowance_usd":"8.765432"`, `"allowance_usd":null`, 1),
		strings.TrimSuffix(planTestCreate, "}") + `,"min_period_count":0}`,
		strings.TrimSuffix(planTestCreate, "}") + `,"min_period_count":100}`,
		strings.TrimSuffix(planTestCreate, "}") + `,"active":null}`,
		strings.TrimSuffix(planTestCreate, "}") + `,"version":1}`,
	} {
		w := httptest.NewRecorder()
		if _, ok := s.decodeBillingPlan(w, billingPlanTestRequest(http.MethodPost, "/", bad), false); ok {
			t.Fatalf("invalid create accepted: %s", bad)
		}
	}
}

func TestBillingPlanUpdateRequiresConfirmedVersionAndFullConfiguration(t *testing.T) {
	for _, suffix := range []string{"", `,"version":0,"min_period_count":1,"active":false`, `,"version":1`, `,"version":1,"min_period_count":1`, `,"version":1,"active":false`} {
		w := httptest.NewRecorder()
		body := strings.TrimSuffix(planTestCreate, "}") + suffix + "}"
		if _, ok := (&Server{}).decodeBillingPlan(w, billingPlanTestRequest(http.MethodPut, "/", body), true); ok || w.Code != 400 {
			t.Fatalf("unconfirmed edit accepted: %d %s", w.Code, w.Body)
		}
	}
}

func TestBillingPlanTransactionsUseOnlySessionIdentityAndReturnExactSnapshot(t *testing.T) {
	for _, role := range []string{store.UserRoleOwner, store.UserRoleMember} {
		for _, purchasing := range []bool{true, false} {
			now := time.Now()
			s, _ := newBillingSourceTestServer(t, role, &now)
			repo := &billingPlanTestRepository{transaction: store.BillingPlanTransaction{
				Plan:        store.BillingPlan{ID: "plan-1", PriceUSD: "1.234567000000", Version: 2},
				PeriodCount: 3, TotalUSD: "3.703701000000", BalanceUSD: "9.123456789012",
				Subscription: store.BillingSubscriptionState{Tier: "day", PeriodCount: 102, CurrentPeriodNumber: 98, ConfigVersion: 8},
			}}
			s.billingPlanRepo = repo
			path, body := "/admin/billing/me/subscriptions/day/renewals", planTestRenewal
			if purchasing {
				path, body = "/admin/billing/me/purchases", planTestPurchase
			}
			r := billingPlanTestRequest(http.MethodPost, path, body)
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			var got store.BillingPlanTransaction
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, repo.transaction) {
				t.Fatalf("transaction response changed snapshot: %d %s", w.Code, w.Body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("financial response must not be cached")
			}
			if purchasing {
				if len(repo.purchases) != 1 {
					t.Fatalf("purchase calls: %+v", repo.purchases)
				}
				p := repo.purchases[0]
				if p.UserID != "self-1" || p.ActorUserID != "self-1" || p.ActorSessionID != "session-1" || p.PlanID != "plan-1" || p.PlanVersion != 2 || p.SubscriptionConfigVersion != 0 || p.PeriodCount != 3 || p.OperationID != planTestOperationID {
					t.Fatalf("purchase parameters: %+v", p)
				}
			} else {
				if len(repo.renewals) != 1 {
					t.Fatalf("renewal calls: %+v", repo.renewals)
				}
				p := repo.renewals[0]
				if p.UserID != "self-1" || p.ActorUserID != "self-1" || p.ActorSessionID != "session-1" || p.Tier != "day" || p.PlanVersion != 2 || p.SubscriptionConfigVersion != 7 || p.PeriodCount != 3 || p.OperationID != planTestOperationID {
					t.Fatalf("renewal parameters: %+v", p)
				}
			}
		}
	}
}

func TestBillingPlanTransactionsDoNotAcknowledgeFailedWrites(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{&store.InsufficientFundsError{}, 400, "insufficient_balance"},
		{store.ErrConflict, 409, "billing_operation_conflict"},
		{store.ErrBillingPlanChanged, 409, "billing_plan_changed"},
		{store.ErrBillingOperationCleaned, 409, "billing_operation_cleaned"},
		{store.ErrInvalid, 400, "invalid_billing_operation"},
		{errors.New("database unavailable"), 500, "internal_error"},
	} {
		now := time.Now()
		s, _ := newBillingSourceTestServer(t, store.UserRoleMember, &now)
		s.billingPlanRepo = &billingPlanTestRepository{err: test.err}
		r := billingPlanTestRequest(http.MethodPost, "/admin/billing/me/purchases", planTestPurchase)
		addBillingSourceTestSession(t, r)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) {
			t.Fatalf("failed transaction: %d %s", w.Code, w.Body)
		}
	}
}

func TestBillingSubscriptionResponseIncludesBindingsAndUnboundedTotals(t *testing.T) {
	plan := &store.BillingPlan{ID: "plan-1", Name: "Monthly", Version: 9, PriceUSD: "3.500000000000"}
	values := billingSubscriptionsResponse([]store.BillingSubscriptionState{
		{Tier: "day", PeriodCount: 135, CurrentPeriodNumber: 120, ConfigVersion: 7, Plan: plan, CanRenew: true},
		{Tier: "month", ConfigVersion: 2},
	})
	if values["day"]["period_count"] != 135 || values["day"]["current_period_number"] != 120 || values["day"]["plan"] != plan || values["day"]["config_version"] != int64(7) || values["day"]["can_renew"] != true {
		t.Fatalf("bound state: %+v", values["day"])
	}
	body, err := json.Marshal(values["month"])
	if err != nil || !strings.Contains(string(body), `"plan":null`) || !strings.Contains(string(body), `"can_renew":false`) {
		t.Fatalf("unbound state: %s %v", body, err)
	}
}
