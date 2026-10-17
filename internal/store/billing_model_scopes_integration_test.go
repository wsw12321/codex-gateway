//go:build integration

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestBillingModelScopesPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	if err := s.SyncModelAccessCatalog(ctx, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "model-scopes")
	write := func(reason string) BillingWriteParams {
		return billingIntegrationWrite(t, user.ID, reason, now)
	}
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{
		BillingWriteParams: write("scope test balance"), UserID: user.ID, USDAmount: "10",
	}); err != nil {
		t.Fatal(err)
	}
	// A pre-initialization group models an existing row migrated from 0035.
	group, err := s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: write("legacy group"), Name: "Model scopes", LimitUSD: "2", Period: "day",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{
		BillingWriteParams: write("scope membership"), GroupID: group.ID, Action: "add", UserIDs: []string{user.ID},
	}); err != nil {
		t.Fatal(err)
	}
	reserve := func(id, model string) (BillingReservation, error) {
		return s.ReserveBilling(ctx, BillingReservationParams{
			RequestID: id, UserID: user.ID, APIKeyID: key.ID, Model: model,
			InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: now,
		})
	}
	complete := func(id, model string, tokens int64) {
		t.Helper()
		if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{
			RequestID: id, UserID: user.ID, DeviceID: device.ID, APIKeyID: key.ID,
			Model: model, Endpoint: "responses", RequestedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: id, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second),
			InputTokens: tokens, ActualModel: model,
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertDenied := func(id, model string) {
		t.Helper()
		_, err := reserve(id, model)
		var insufficient *InsufficientFundsError
		if !errors.As(err, &insufficient) || insufficient.RetryAfter != 0 {
			t.Fatalf("%s must fail without a renewal hint: %v", id, err)
		}
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM billing_reservations WHERE request_id=$1`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected reservation persisted: count=%d err=%v", count, err)
		}
	}
	for _, catalog := range [][]string{nil, {}} {
		if err := s.InitializeBillingModelScopes(ctx, catalog); !errors.Is(err, ErrInvalid) {
			t.Fatalf("empty catalog initialized scopes: %v", err)
		}
	}
	assertDenied("scopes-before-startup", "scope-a")
	models := []string{"scope-a", "scope-b"}
	if err := s.SyncModelAccessCatalog(ctx, models); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingModelScopes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	settings, err := s.GetBillingSettings(ctx)
	if err != nil || !reflect.DeepEqual(settings.ModelIDs, models) {
		t.Fatalf("cash initialization: %+v %v", settings, err)
	}
	initialized, err := s.GetGroup(ctx, group.ID)
	if err != nil || !reflect.DeepEqual(initialized.ModelIDs, models) || initialized.PeriodID != group.PeriodID || !initialized.UpdatedAt.Equal(group.UpdatedAt) {
		t.Fatalf("group initialization changed existing period or metadata: %+v %v", initialized, err)
	}
	// Capture funding before removing this model from both scopes. Settlement
	// must keep the original group period and cash cutoff after the edit.
	inflight, err := reserve("scopes-inflight-request", "scope-a")
	if err != nil {
		t.Fatal(err)
	}
	if inflight.GroupPeriodID == nil || *inflight.GroupPeriodID != group.PeriodID || inflight.CashLotCutoff == nil {
		t.Fatalf("initial funding snapshot: %+v", inflight)
	}
	complete(inflight.RequestID, "scope-a", 3_000_000)
	change := SetBillingCashModelsParams{BillingWriteParams: write("select cash models"), ModelIDs: []string{"scope-b"}}
	changed, err := s.SetBillingCashModels(ctx, change)
	if err != nil || !reflect.DeepEqual(changed.ModelIDs, []string{"scope-b"}) {
		t.Fatalf("cash scope update: %+v %v", changed, err)
	}
	replay, err := s.SetBillingCashModels(ctx, change)
	if err != nil || !reflect.DeepEqual(changed, replay) {
		t.Fatalf("cash scope replay: %+v %v", replay, err)
	}
	conflict := change
	conflict.ModelIDs = []string{"scope-a"}
	if _, err := s.SetBillingCashModels(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("cash scope operation accepted changed replay: %v", err)
	}
	for _, invalid := range [][]string{nil, {}, {"unknown-model"}} {
		if _, err := s.SetBillingCashModels(ctx, SetBillingCashModelsParams{
			BillingWriteParams: write("invalid cash models"), ModelIDs: invalid,
		}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid cash model scope %v accepted: %v", invalid, err)
		}
	}
	group, err = s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: write("select group models"), GroupID: group.ID,
		Name: group.Name, LimitUSD: "2", Period: "day", ModelIDs: []string{"scope-b"},
	})
	if err != nil || !reflect.DeepEqual(group.ModelIDs, []string{"scope-b"}) {
		t.Fatalf("group scope update: %+v %v", group, err)
	}
	assertDenied("scopes-after-owner-edit", "scope-a")
	if err := s.SyncModelAccessCatalog(ctx, []string{"scope-a", "scope-b", "scope-c"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingModelScopes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	settings, err = s.GetBillingSettings(ctx)
	if err != nil || !reflect.DeepEqual(settings.ModelIDs, []string{"scope-b"}) {
		t.Fatalf("catalog change expanded cash scope: %+v %v", settings, err)
	}
	group, err = s.GetGroup(ctx, group.ID)
	if err != nil || !reflect.DeepEqual(group.ModelIDs, []string{"scope-b"}) {
		t.Fatalf("catalog change expanded group scope: %+v %v", group, err)
	}
	assertDenied("scopes-new-catalog-model", "scope-c")
	settled, err := s.SettleBilling(ctx, inflight.RequestID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	groupBillingAssertSplit(t, settled, "2", "1", "0")
	billingIntegrationAssertAllocations(t, ctx, s, inflight.RequestID,
		[]string{"group", "cash"}, []string{"2.000000000000", "1.000000000000"})
	// Subscription eligibility is independent of the cash and group scopes.
	if _, err := s.PutSubscription(ctx, PutSubscriptionParams{
		BillingWriteParams: write("independent subscription"), UserID: user.ID,
		Name: "Scope A", Tier: "day", AllowanceUSD: "1", ModelIDs: []string{"scope-a"},
	}); err != nil {
		t.Fatal(err)
	}
	reservation, err := reserve("scopes-subscription", "scope-a")
	if err != nil || reservation.GroupPeriodID != nil ||
		len(reservation.OrderedFundingSources) != 1 || reservation.OrderedFundingSources[0].SourceType != "subscription" {
		t.Fatalf("subscription funding inherited unrelated model scope: %+v %v", reservation, err)
	}
	complete(reservation.RequestID, "scope-a", 1_000_000)
	if _, err := s.SettleBilling(ctx, reservation.RequestID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	billingIntegrationAssertAllocations(t, ctx, s, reservation.RequestID,
		[]string{"subscription"}, []string{"1.000000000000"})
	var auditCount, ledgerCount int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM audit_events WHERE event_type='billing.cash_models_updated' AND metadata->>'operation_id'=$1),
		(SELECT count(*) FROM billing_ledger_entries WHERE entry_type='cash_models' AND operation_id=$1::uuid)`, change.OperationID).Scan(&auditCount, &ledgerCount); err != nil || auditCount != 1 || ledgerCount != 1 {
		t.Fatalf("cash model change audit/ledger count=%d/%d: %v", auditCount, ledgerCount, err)
	}
}
