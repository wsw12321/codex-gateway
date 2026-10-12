package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

const subscriptionTestID = "550e8400-e29b-41d4-a716-446655440001"
const preferencesTestBody = `{"operation_id":"` + planTestOperationID + `","priority":42,"charge_enabled":false}`

type subscriptionTestRepository struct {
	puts        []store.PutSubscriptionParams
	deletes     []store.DeleteSubscriptionParams
	preferences []store.SetBillingSubscriptionPreferencesParams
	cash        []store.SetBillingCashPreferencesParams
	err         error
}

func (f *subscriptionTestRepository) PutSubscription(_ context.Context, p store.PutSubscriptionParams) (store.BillingSubscriptionState, error) {
	f.puts = append(f.puts, p)
	return store.BillingSubscriptionState{ID: p.SubscriptionID}, f.err
}
func (f *subscriptionTestRepository) DeleteSubscription(_ context.Context, p store.DeleteSubscriptionParams) (store.BillingSubscriptionState, error) {
	f.deletes = append(f.deletes, p)
	return store.BillingSubscriptionState{ID: p.SubscriptionID}, f.err
}
func (f *subscriptionTestRepository) SetBillingSubscriptionPreferences(_ context.Context, p store.SetBillingSubscriptionPreferencesParams) (store.BillingSubscriptionState, error) {
	f.preferences = append(f.preferences, p)
	return store.BillingSubscriptionState{ID: p.SubscriptionID, Priority: p.Priority, ChargeEnabled: p.ChargeEnabled}, f.err
}
func (f *subscriptionTestRepository) SetBillingCashPreferences(_ context.Context, p store.SetBillingCashPreferencesParams) (store.BillingCashPreferences, error) {
	f.cash = append(f.cash, p)
	return store.BillingCashPreferences{Priority: p.Priority, ChargeEnabled: p.ChargeEnabled}, f.err
}

func TestBillingPreferencesStrictInput(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, strings.Replace(preferencesTestBody, `42`, `0`, 1), strings.Replace(preferencesTestBody, `42`, `101`, 1),
		strings.Replace(preferencesTestBody, `42`, `1.5`, 1), strings.Replace(preferencesTestBody, `42`, `"1"`, 1),
		strings.Replace(preferencesTestBody, `false`, `null`, 1), strings.Replace(preferencesTestBody, `false`, `"false"`, 1),
		strings.TrimSuffix(preferencesTestBody, "}") + `,"user_id":"other"}`, strings.TrimSuffix(preferencesTestBody, "}") + `,"priority":1}`,
		strings.Replace(preferencesTestBody, `"priority"`, `"Priority"`, 1),
	} {
		w := httptest.NewRecorder()
		if _, ok := (&Server{}).decodeBillingPreferences(w, billingPlanTestRequest(http.MethodPut, "/", body)); ok || w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body)
		}
	}
	for _, priority := range []string{"1", "100"} {
		w := httptest.NewRecorder()
		if _, ok := (&Server{}).decodeBillingPreferences(w, billingPlanTestRequest(http.MethodPut, "/", strings.Replace(preferencesTestBody, "42", priority, 1))); !ok {
			t.Fatalf("rejected priority %s: %s", priority, w.Body)
		}
	}
}

func TestBillingPreferencesUseSessionIdentity(t *testing.T) {
	for _, role := range []string{store.UserRoleMember, store.UserRoleOwner} {
		now := time.Now()
		s, _ := newBillingSourceTestServer(t, role, &now)
		repo := &subscriptionTestRepository{}
		s.billingSubscriptionRepo = repo
		for _, path := range []string{"/admin/billing/me/cash/preferences", "/admin/billing/me/subscriptions/" + subscriptionTestID + "/preferences"} {
			r := billingPlanTestRequest(http.MethodPut, path, preferencesTestBody)
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("save: %d %s", w.Code, w.Body)
			}
		}
		if len(repo.cash) != 1 || repo.cash[0].UserID != "self-1" || repo.cash[0].ActorUserID != "self-1" || repo.cash[0].Priority != 42 || repo.cash[0].ChargeEnabled {
			t.Fatalf("cash identity: %+v", repo.cash)
		}
		if len(repo.preferences) != 1 || repo.preferences[0].UserID != "self-1" || repo.preferences[0].SubscriptionID != subscriptionTestID || repo.preferences[0].ActorSessionID != "session-1" {
			t.Fatalf("subscription identity: %+v", repo.preferences)
		}
	}
}

func TestBillingPreferencesFailedAndCrossUserOperations(t *testing.T) {
	for _, err := range []error{store.ErrNotFound, errors.New("audit failure")} {
		now := time.Now()
		s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
		s.billingSubscriptionRepo = &subscriptionTestRepository{err: err}
		r := billingPlanTestRequest(http.MethodPut, "/admin/billing/me/subscriptions/"+subscriptionTestID+"/preferences", preferencesTestBody)
		addBillingSourceTestSession(t, r)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatalf("failure acknowledged: %s", w.Body)
		}
	}
	now := time.Now()
	s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
	repo := &subscriptionTestRepository{}
	s.billingSubscriptionRepo = repo
	for _, path := range []string{"/admin/billing/users/other/cash/preferences", "/admin/billing/users/other/subscriptions/" + subscriptionTestID + "/preferences", "/admin/billing/me/cash/preferences?user_id=other", "/admin/billing/me/sources/day/status"} {
		r := billingPlanTestRequest(http.MethodPut, path, preferencesTestBody)
		addBillingSourceTestSession(t, r)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code < 400 || len(repo.cash)+len(repo.preferences) > 0 {
			t.Fatalf("identity override accepted: %d %s", w.Code, w.Body)
		}
	}
}

func TestAdministratorSubscriptionsUseIndependentIDs(t *testing.T) {
	now := time.Now()
	s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
	repo := &subscriptionTestRepository{}
	s.billingSubscriptionRepo = repo
	body := `{"operation_id":"` + planTestOperationID + `","reason":"Grant","name":"Independent","tier":"custom","period_days":15,"period_count":0,"quota_usd":"12.123456789012","model_ids":["gpt-test"]}`
	for _, tc := range []struct{ method, path, body string }{{http.MethodPost, "/admin/billing/users/other/subscriptions", body}, {http.MethodPut, "/admin/billing/users/other/subscriptions/" + subscriptionTestID, body}, {http.MethodDelete, "/admin/billing/users/other/subscriptions/" + subscriptionTestID, `{"operation_id":"` + planTestOperationID + `","reason":"Stop"}`}} {
		r := billingPlanTestRequest(tc.method, tc.path, tc.body)
		addBillingSourceTestSession(t, r)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("admin operation: %d %s", w.Code, w.Body)
		}
	}
	if len(repo.puts) != 2 || repo.puts[0].SubscriptionID != "" || repo.puts[1].SubscriptionID != subscriptionTestID || repo.puts[0].UserID != "other" || repo.puts[0].PeriodDays != 15 || len(repo.deletes) != 1 || repo.deletes[0].SubscriptionID != subscriptionTestID {
		t.Fatalf("IDs not preserved: %+v %+v", repo.puts, repo.deletes)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		r := billingPlanTestRequest(method, "/admin/billing/users/other/subscriptions/day", body)
		addBillingSourceTestSession(t, r)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("retired tier mutation accepted")
		}
	}
}

func TestBillingSubscriptionsAreIndependentArrays(t *testing.T) {
	subscriptions := billingSubscriptionsResponse([]store.BillingSubscriptionState{{ID: "first", Tier: "day", Name: "First"}, {ID: "second", Tier: "day", Name: "Second"}})
	encoded, err := json.Marshal(subscriptions)
	if err != nil || len(subscriptions) != 2 || !strings.HasPrefix(string(encoded), "[") {
		t.Fatalf("same-tier subscriptions merged: %s %v", encoded, err)
	}
	empty, _ := json.Marshal(billingSubscriptionsResponse(nil))
	if string(empty) != "[]" {
		t.Fatalf("empty list: %s", empty)
	}
}
