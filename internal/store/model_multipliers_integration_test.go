//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
)

func TestModelMultipliersPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Keep settings and cleanup boundaries isolated from other billing suites.
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	actor := globalUsageIntegrationUser(t, ctx, s, "multiplier-actor", UserRoleMember)
	set := func(model, multiplier string) ModelMultiplier {
		t.Helper()
		value, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{
			BillingWriteParams: billingIntegrationWrite(t, actor.ID, "model multiplier regression", now),
			Model:              model, Multiplier: multiplier,
		})
		if err != nil {
			t.Fatalf("set multiplier %s=%s: %v", model, multiplier, err)
		}
		return value
	}
	credit := func(userID, amount string) {
		t.Helper()
		if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{
			BillingWriteParams: billingIntegrationWrite(t, actor.ID, "fund multiplier regression", now),
			UserID:             userID, USDAmount: amount,
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("snapshots survive out of order completion restart and changing current settings", func(t *testing.T) {
		const model = "multiplier-snapshot"
		user, device, key := billingIntegrationPrincipal(t, ctx, s, "multiplier-snapshot")
		credit(user.ID, "5")
		if _, err := s.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: billingIntegrationWrite(t, actor.ID, "subscription split", now),
			UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "0.5",
		}); err != nil {
			t.Fatal(err)
		}
		group, err := s.PutGroup(ctx, PutGroupParams{
			BillingWriteParams: billingIntegrationWrite(t, actor.ID, "shared multiplier budget", now),
			Name:               "Multiplier budget", LimitUSD: "3", Period: "day",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{
			BillingWriteParams: billingIntegrationWrite(t, actor.ID, "add multiplier member", now),
			GroupID:            group.ID, UserIDs: []string{user.ID}, Action: "add",
		}); err != nil {
			t.Fatal(err)
		}
		accounts := []UpstreamAccountSnapshot{
			{ID: upstreamIntegrationAccountID("multiplier-a"), MaskedEmail: "a***@multiplier.example", Status: UpstreamAccountStatusAvailable},
			{ID: upstreamIntegrationAccountID("multiplier-b"), MaskedEmail: "b***@multiplier.example", Status: UpstreamAccountStatusAvailable},
		}
		if err := s.SyncUpstreamAccounts(ctx, accounts, now); err != nil {
			t.Fatal(err)
		}
		admit := func(id, wantMultiplier, accountID string) {
			t.Helper()
			params := billingIntegrationAdmission(user, device, key, id, now.Add(time.Second))
			params.Usage.Model, params.Billing.Model = model, model
			result, err := s.AdmitRequest(ctx, params)
			if err != nil || result.Billing == nil || !equalAllocationDecimal(result.Billing.PricingMultiplier, wantMultiplier) {
				t.Fatalf("admission %s: %+v, %v; want multiplier %s", id, result, err, wantMultiplier)
			}
			if err := s.AttributeUsageRequest(ctx, id, accountID); err != nil {
				t.Fatal(err)
			}
		}
		requestA, requestB := "multiplier-snapshot-request-a", "multiplier-snapshot-request-b"
		admit(requestA, "1", accounts[0].ID)
		set(model, "2")
		admit(requestB, "2", accounts[1].ID)
		set(model, "3")
		complete := func(id string) {
			t.Helper()
			accountID := accounts[0].ID
			if id == requestB {
				accountID = accounts[1].ID
			}
			if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
				RequestID: id, State: "completed", HTTPStatus: 200, InputTokens: 1_000_000,
				ActualModel: model, CompletedAt: now.Add(2 * time.Second), UpstreamAccountID: accountID,
			}); err != nil {
				t.Fatal(err)
			}
		}
		complete(requestB)
		if err := s.SettleRequest(ctx, requestB, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		complete(requestA)
		// A separate pool/Store has only durable state, as after a process restart.
		restarted := modelMultiplierIntegrationReopen(t, ctx, s)
		if n, err := restarted.RetryUnsettledRequests(ctx, 100); err != nil || n != 1 {
			t.Fatalf("restart recovery settled %d: %v", n, err)
		}
		for _, id := range []string{requestA, requestB, requestA} {
			if err := restarted.SettleRequest(ctx, id, now.Add(4*time.Second)); err != nil {
				t.Fatalf("idempotent settlement %s: %v", id, err)
			}
		}
		billingIntegrationAssertAllocations(t, ctx, s, requestB, []string{"day", "cash"}, []string{"0.500000000000", "1.500000000000"})
		billingIntegrationAssertAllocations(t, ctx, s, requestA, []string{"cash"}, []string{"1.000000000000"})
		state, err := s.GetBillingState(ctx, user.ID, 20, 0)
		if err != nil || state.BalanceUSD != "2.500000000000" || state.Group == nil || state.Group.UsedUSD != "3.000000000000" {
			t.Fatalf("multiplied cash and group cost: %+v, %v", state, err)
		}
		charges := 0
		for _, entry := range state.Ledger {
			if entry.EntryType != "usage_charge" {
				continue
			}
			charges++
			want := "1"
			if entry.RequestID != nil && *entry.RequestID == requestB {
				want = "2"
			}
			if !equalAllocationDecimal(entry.PricingMultiplier, want) || !equalAllocationDecimal(entry.AmountUSD, want) {
				t.Fatalf("ledger lost request multiplier: %+v", entry)
			}
		}
		if charges != 2 {
			t.Fatalf("repeated settlement produced %d charges", charges)
		}
		checkReports := func() {
			t.Helper()
			rows, err := s.GlobalUsage(ctx, now.Add(-time.Second), now.Add(time.Hour), model, false, now.Add(-time.Second))
			if err != nil {
				t.Fatal(err)
			}
			row := findGlobalUsageIntegrationRow(t, rows, user.ID)
			if row.ActualCostUSD != "3.000000000000" || row.ChargedUSD != "3.000000000000" || row.UncoveredUSD != "0.000000000000" {
				t.Fatalf("report did not use multiplied immutable costs: %+v", row)
			}
			allocations, err := s.ListUpstreamAccountAllocations(ctx, now.Add(time.Hour))
			if err != nil || len(allocations) != 2 {
				t.Fatalf("upstream allocation statistics: %+v, %v", allocations, err)
			}
			for _, allocation := range allocations {
				want := "1"
				if allocation.AccountID == accounts[1].ID {
					want = "2"
				}
				if !equalAllocationDecimal(allocation.CostUSD, want) {
					t.Fatalf("upstream allocation ignored multiplier: %+v", allocation)
				}
			}
			selected, err := s.SelectUpstreamAccount(ctx, user.ID, []string{accounts[0].ID, accounts[1].ID}, now.Add(time.Hour))
			if err != nil || selected != accounts[0].ID {
				t.Fatalf("selection by multiplied cost: %s, %v", selected, err)
			}
		}
		checkReports()
		set(model, "0.5")
		checkReports()
		blocked := billingIntegrationAdmission(user, device, key, "multiplier-exhausted-group-request", now.Add(time.Minute))
		blocked.Usage.Model, blocked.Billing.Model = model, model
		var groupExceeded *GroupQuotaExceededError
		if _, err := s.AdmitRequest(ctx, blocked); !errors.As(err, &groupExceeded) {
			t.Fatalf("multiplied group total did not close admission: %v", err)
		}
	})

	t.Run("all pricing categories preserve base prices and round only once", func(t *testing.T) {
		price := func(input, cached, write, output string) *config.TokenPricing {
			return &config.TokenPricing{InputUSDPerMillion: input, CachedInputUSDPerMillion: cached,
				CacheWriteUSDPerMillion: &write, OutputUSDPerMillion: output}
		}
		for i, tc := range []struct {
			name, tier, wantTier, context, cost, fallback, multiplier string
			input                                                     int64
			omitLong, missingWrite, included, v1, round               bool
		}{
			{name: "standard-short", tier: "default", wantTier: "standard", context: "short", input: 100, cost: "0.000213000000"},
			{name: "standard-long", tier: "default", wantTier: "standard", context: "long", input: 101, cost: "0.000429000000"},
			{name: "flex-short", tier: "flex", wantTier: "flex", context: "short", input: 100, cost: "0.000106500000"},
			{name: "flex-long", tier: "flex", wantTier: "flex", context: "long", input: 101, cost: "0.000214500000"},
			{name: "fast-short", tier: "priority", wantTier: "fast", context: "short", input: 100, cost: "0.000639000000"},
			{name: "fast-long", tier: "fast", wantTier: "fast", context: "long", input: 101, cost: "0.001287000000"},
			{name: "missing-tier", wantTier: "max_published", context: "short", input: 100, cost: "0.000639000000", fallback: config.FallbackMissingServiceTier},
			{name: "unknown-tier", tier: "future", wantTier: "max_published", context: "short", input: 100, cost: "0.000639000000", fallback: config.FallbackUnknownServiceTier},
			{name: "missing-context", tier: "flex", wantTier: "max_published", context: "long", input: 101, cost: "0.001287000000", fallback: config.FallbackMissingPriceCombination, omitLong: true},
			{name: "over-model-limit", tier: "default", wantTier: "max_published", context: "long", input: 301, cost: "0.003087000000", fallback: config.FallbackMissingPriceCombination},
			{name: "missing-cache-writes", tier: "default", wantTier: "standard", context: "short", input: 100, cost: "0.000288000000", fallback: config.FallbackMissingCacheWriteTokens, missingWrite: true},
			{name: "writes-in-input", tier: "default", wantTier: "standard", context: "short", input: 100, cost: "0.000168000000", included: true},
			{name: "v1-cached-input", input: 100, cost: "0.000168000000", v1: true},
			{name: "v1-discount", input: 100, cost: "0.000028000000", v1: true, multiplier: "0.25"},
			{name: "v2-discount", tier: "default", wantTier: "standard", context: "short", input: 100, cost: "0.000035500000", multiplier: "0.25"},
			{name: "v1-single-rounding", input: 1, cost: "0.000000000001", v1: true, round: true},
			{name: "v2-single-rounding", tier: "default", wantTier: "standard", context: "short", input: 1, cost: "0.000000000001", round: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				model := "multiplier-" + tc.name
				user, device, key := billingIntegrationPrincipal(t, ctx, s, model)
				credit(user.ID, "1")
				multiplier := "1.5"
				if tc.multiplier != "" {
					multiplier = tc.multiplier
				}
				if tc.round {
					multiplier = "5"
				}
				set(model, multiplier)
				id := fmt.Sprintf("multiplier-pricing-request-%02d", i)
				params := billingIntegrationAdmission(user, device, key, id, now)
				params.Usage.Model, params.Billing.Model = model, model
				params.Billing.CachedInputUSDPerMillion, params.Billing.OutputUSDPerMillion = "0.1", "3"
				rule := config.ModelPricing{CacheWriteMode: config.CacheWriteSeparate, MaxInputTokens: 300, LongContextThresholdTokens: 100,
					ServiceTiers: map[string]config.ServiceTierPricing{
						"standard": {Short: price("1", "0.1", "2", "3"), Long: price("2", "0.2", "4", "6")},
						"flex":     {Short: price("0.5", "0.05", "1", "1.5"), Long: price("1", "0.1", "2", "3")},
						"fast":     {Short: price("3", "0.3", "6", "9"), Long: price("6", "0.6", "12", "18")},
					}}
				if tc.omitLong {
					tier := rule.ServiceTiers["flex"]
					tier.Long = nil
					rule.ServiceTiers["flex"] = tier
				}
				if tc.included || tc.round {
					rule.CacheWriteMode = config.CacheWriteIncludedInInput
					for _, tier := range rule.ServiceTiers {
						tier.Short.CacheWriteUSDPerMillion, tier.Long.CacheWriteUSDPerMillion = nil, nil
					}
				}
				if tc.round {
					params.Billing.InputUSDPerMillion = "0.0000001"
					rule.ServiceTiers["standard"].Short.InputUSDPerMillion = "0.0000001"
				}
				if !tc.v1 {
					params.Billing.InputUSDPerMillion, params.Billing.CachedInputUSDPerMillion, params.Billing.OutputUSDPerMillion = "", "", ""
					params.Usage.PricingRuleVersion = config.PricingSchemaV2
					params.Billing.PricingRuleVersion = config.PricingSchemaV2
					params.Billing.PricingCatalogAsOf = "2026-09-28"
					params.Billing.BillingMode = BillingModeOpenAIAPIEquivalent
					params.Billing.PricingModel = model
					params.Billing.CacheWriteMode = rule.CacheWriteMode
					params.Billing.PricingSnapshot = billingIntegrationV2Snapshot(t, model, rule)
				}
				if _, err := s.AdmitRequest(ctx, params); err != nil {
					t.Fatal(err)
				}
				completion := CompleteUsageRequestParams{RequestID: id, State: "completed", HTTPStatus: 200,
					CompletedAt: now.Add(time.Second), InputTokens: tc.input, CachedInputTokens: 20, CacheWriteTokens: 30,
					CacheWriteTokensPresent: !tc.missingWrite, OutputTokens: 10, ActualModel: model, ActualServiceTier: tc.tier}
				if tc.round {
					completion.CachedInputTokens, completion.CacheWriteTokens, completion.OutputTokens = 0, 0, 0
				}
				if _, err := s.CompleteUsageRequest(ctx, completion); err != nil {
					t.Fatal(err)
				}
				if err := s.SettleRequest(ctx, id, now.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				var cost, applied, snapshot, tier, class, fallback string
				if err := s.db.QueryRowContext(ctx, `SELECT l.amount_usd::text,COALESCE(l.applied_input_usd_per_million,r.input_usd_per_million)::text,
					l.pricing_multiplier::text,COALESCE(l.pricing_service_tier,''),COALESCE(l.context_class,''),COALESCE(l.pricing_fallback_reason,'')
					FROM billing_ledger_entries l JOIN billing_reservations r USING(request_id) WHERE l.request_id=$1`, id).Scan(&cost, &applied, &snapshot, &tier, &class, &fallback); err != nil {
					t.Fatal(err)
				}
				if cost != tc.cost || !equalAllocationDecimal(snapshot, multiplier) {
					t.Fatalf("cost=%s multiplier=%s, want %s and %s", cost, snapshot, tc.cost, multiplier)
				}
				if !tc.v1 {
					parsed, err := config.ParsePricingSnapshot(params.Billing.PricingSnapshot)
					if err != nil {
						t.Fatal(err)
					}
					decision, err := parsed.Select(tc.tier, tc.input)
					if err != nil || !equalAllocationDecimal(applied, decision.InputUSDPerMillion) || tier != tc.wantTier || class != tc.context || fallback != tc.fallback {
						t.Fatalf("base snapshot/decision changed: applied=%s tier=%s context=%s fallback=%s decision=%+v err=%v", applied, tier, class, fallback, decision, err)
					}
				} else if !equalAllocationDecimal(applied, params.Billing.InputUSDPerMillion) {
					t.Fatalf("v1 base price changed from %s to %s", params.Billing.InputUSDPerMillion, applied)
				}
			})
		}
	})

	t.Run("idempotency audit and validation are atomic", func(t *testing.T) {
		const model = "multiplier-idempotency"
		defaults, err := s.ListModelMultipliers(ctx, []string{model, "codex-auto-review"})
		if err != nil || len(defaults) != 2 {
			t.Fatalf("default multipliers: %+v %v", defaults, err)
		}
		for _, value := range defaults {
			if !equalAllocationDecimal(value.Multiplier, "1") || value.UpdatedAt != nil {
				t.Fatalf("unset/fixed model default: %+v", value)
			}
		}
		params := SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "discount test", now), Model: model, Multiplier: "0.25"}
		first, err := s.SetModelMultiplier(ctx, params)
		if err != nil || !equalAllocationDecimal(first.Multiplier, "0.25") || first.UpdatedAt == nil {
			t.Fatalf("discount write: %+v %v", first, err)
		}
		set(model, "2")
		replay, err := s.SetModelMultiplier(ctx, params)
		if err != nil || !equalAllocationDecimal(replay.Multiplier, "0.25") || replay.UpdatedAt == nil || !replay.UpdatedAt.Equal(*first.UpdatedAt) {
			t.Fatalf("replay must return its original result: %+v %v", replay, err)
		}
		changed := params
		changed.Multiplier = "0.5"
		if _, err := s.SetModelMultiplier(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("different multiplier replay: %v", err)
		}
		changed = params
		changed.Model = "multiplier-other-model"
		if _, err := s.SetModelMultiplier(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("different model replay: %v", err)
		}
		for _, value := range []string{"", "0", "-1", "1e2", "NaN", "1000000000000000000", "1.0000000000001"} {
			invalid := SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "invalid test", now), Model: model, Multiplier: value}
			if _, err := s.SetModelMultiplier(ctx, invalid); !errors.Is(err, ErrInvalid) {
				t.Errorf("invalid multiplier %q: %v", value, err)
			}
		}
		invalid := SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "fixed model test", now), Model: "codex-auto-review", Multiplier: "2"}
		if _, err := s.SetModelMultiplier(ctx, invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("internal zero model edit: %v", err)
		}
		failed := SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "failed audit test", now), Model: model, Multiplier: "9"}
		failed.SourceIP = "invalid-ip"
		if _, err := s.SetModelMultiplier(ctx, failed); err == nil {
			t.Fatal("invalid audit write unexpectedly succeeded")
		}
		var operations, ledger, audits, failedOperations int
		var previous, current, auditModel, reason, auditActor string
		if err := s.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM billing_operations WHERE operation_id=$1),
			(SELECT count(*) FROM billing_ledger_entries WHERE operation_id=$1),
			(SELECT count(*) FROM audit_events WHERE metadata->>'operation_id'=$1::text),
			(SELECT count(*) FROM billing_operations WHERE operation_id=$2),
			a.metadata->>'previous_multiplier',a.metadata->>'multiplier',a.metadata->>'model',a.metadata->>'reason',a.actor_user_id::text
			FROM audit_events a WHERE a.event_type='billing.model_multiplier_updated' AND a.metadata->>'operation_id'=$1::text`,
			params.OperationID, failed.OperationID).Scan(&operations, &ledger, &audits, &failedOperations, &previous, &current, &auditModel, &reason, &auditActor); err != nil {
			t.Fatal(err)
		}
		if operations != 1 || ledger != 1 || audits != 1 || failedOperations != 0 || !equalAllocationDecimal(previous, "1") ||
			!equalAllocationDecimal(current, "0.25") || auditModel != model || reason != params.Reason || auditActor != actor.ID {
			t.Fatalf("audit/idempotency mismatch: %d/%d/%d/%d %s/%s %s %s %s", operations, ledger, audits, failedOperations, previous, current, auditModel, reason, auditActor)
		}
		listed, err := s.ListModelMultipliers(ctx, []string{model})
		if err != nil || len(listed) != 1 || !equalAllocationDecimal(listed[0].Multiplier, "2") {
			t.Fatalf("replay or audit failure rewrote current multiplier: %+v %v", listed, err)
		}
		omitted, err := s.ListModelMultipliers(ctx, []string{"multiplier-unrelated"})
		if err != nil || len(omitted) != 1 || omitted[0].Model == model {
			t.Fatalf("removed configured model leaked into list: %+v %v", omitted, err)
		}
		listed, err = s.ListModelMultipliers(ctx, []string{model})
		if err != nil || len(listed) != 1 || !equalAllocationDecimal(listed[0].Multiplier, "2") {
			t.Fatalf("reintroduced model lost stored multiplier: %+v %v", listed, err)
		}
	})

	t.Run("multiplier database read failure rolls back admission", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, s, "multiplier-db-failure")
		credit(user.ID, "1")
		set("multiplier-db-failure", "3")
		pending := billingIntegrationAdmission(user, device, key, "multiplier-db-independent-settlement", now)
		pending.Usage.Model, pending.Billing.Model = "multiplier-db-failure", "multiplier-db-failure"
		if _, err := s.AdmitRequest(ctx, pending); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: pending.Quota.RequestID, State: "completed", HTTPStatus: 200,
			InputTokens: 100_000, CompletedAt: now.Add(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		set("multiplier-db-failure", "9")
		restarted := modelMultiplierIntegrationReopen(t, ctx, s)
		lock, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback()
		if _, err := lock.ExecContext(ctx, `LOCK TABLE billing_model_multipliers IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		// Retry must succeed even when the current setting is unreadable.
		recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 2*time.Second)
		n, recoveryErr := restarted.RetryUnsettledRequests(recoveryCtx, 100)
		recoveryCancel()
		if recoveryErr != nil || n != 1 {
			t.Fatalf("recovery read current multiplier table: count=%d err=%v", n, recoveryErr)
		}
		var recoveredCost string
		if err := s.db.QueryRowContext(ctx, `SELECT amount_usd::text FROM billing_ledger_entries WHERE request_id=$1`, pending.Quota.RequestID).Scan(&recoveredCost); err != nil || recoveredCost != "0.300000000000" {
			t.Fatalf("recovery lost original multiplier: cost=%s err=%v", recoveredCost, err)
		}
		params := billingIntegrationAdmission(user, device, key, "multiplier-db-failure-request", now)
		params.Usage.Model, params.Billing.Model = "multiplier-db-failure", "multiplier-db-failure"
		blocked, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		_, err = s.AdmitRequest(blocked, params)
		cancel()
		if err == nil {
			t.Fatal("admission defaulted multiplier after database read failure")
		}
		if err := lock.Rollback(); err != nil {
			t.Fatal(err)
		}
		var artifacts int
		if err := s.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM billing_reservations WHERE request_id=$1)+
			(SELECT count(*) FROM quota_reservations WHERE request_id=$1)+
			(SELECT count(*) FROM usage_requests WHERE request_id=$1)`, params.Quota.RequestID).Scan(&artifacts); err != nil || artifacts != 0 {
			t.Fatalf("failed multiplier read left %d admission artifacts: %v", artifacts, err)
		}
	})

	t.Run("internal zero model keeps multiplier one without funding", func(t *testing.T) {
		const model = "codex-auto-review"
		user, device, key := billingIntegrationPrincipal(t, ctx, s, "multiplier-internal-zero")
		params := billingIntegrationAdmission(user, device, key, "multiplier-internal-zero-request", now)
		params.Usage.Model, params.Usage.PricingRuleVersion = model, config.PricingSchemaV2
		params.Billing = &BillingReservationParams{
			RequestID: params.Quota.RequestID, UserID: user.ID, APIKeyID: key.ID, Model: model,
			PricingRuleVersion: config.PricingSchemaV2, BillingMode: BillingModeInternalZero,
			PricingCatalogAsOf: "2026-09-28", PricingModel: model, CacheWriteMode: config.CacheWriteIncludedInInput,
			PricingSnapshot: billingIntegrationV2Snapshot(t, model, config.ModelPricing{
				CacheWriteMode: config.CacheWriteIncludedInInput, MaxInputTokens: 1000, LongContextThresholdTokens: 1000,
				ServiceTiers: map[string]config.ServiceTierPricing{"standard": {Short: &config.TokenPricing{
					InputUSDPerMillion: "0", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0",
				}}},
			}), Now: now,
		}
		admission, err := s.AdmitRequest(ctx, params)
		if err != nil || admission.Billing == nil || !equalAllocationDecimal(admission.Billing.PricingMultiplier, "1") {
			t.Fatalf("internal model admission: %+v err=%v", admission, err)
		}
		if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: params.Quota.RequestID, State: "completed", HTTPStatus: 200,
			InputTokens: 100, OutputTokens: 50, ActualServiceTier: "default", CompletedAt: now.Add(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SettleRequest(ctx, params.Quota.RequestID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		state, err := s.GetBillingState(ctx, user.ID, 10, 0)
		if err != nil || len(state.Ledger) != 1 || state.Ledger[0].AmountUSD != "0.000000000000" ||
			!equalAllocationDecimal(state.Ledger[0].PricingMultiplier, "1") {
			t.Fatalf("internal model ledger: %+v err=%v", state, err)
		}
	})

	t.Run("multiplied cost overflow rolls back the entire settlement", func(t *testing.T) {
		user, device, key := billingIntegrationPrincipal(t, ctx, s, "multiplier-overflow")
		credit(user.ID, "1")
		set("multiplier-overflow", "2")
		params := billingIntegrationAdmission(user, device, key, "multiplier-overflow-request", now)
		params.Usage.Model, params.Billing.Model = "multiplier-overflow", "multiplier-overflow"
		params.Billing.InputUSDPerMillion = "999999999999999999"
		if _, err := s.AdmitRequest(ctx, params); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: params.Quota.RequestID, State: "completed", HTTPStatus: 200,
			InputTokens: 1_000_000, CompletedAt: now.Add(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SettleRequest(ctx, params.Quota.RequestID, now.Add(2*time.Second)); err == nil {
			t.Fatal("overflowed multiplied cost unexpectedly settled")
		}
		billingIntegrationAssertReservationStates(t, ctx, s, params.Quota.RequestID, "reserved", "reserved")
		var balance string
		var charges int
		if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text,
			(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$2) FROM billing_accounts WHERE user_id=$1`,
			user.ID, params.Quota.RequestID).Scan(&balance, &charges); err != nil || balance != "1.000000000000" || charges != 0 {
			t.Fatalf("overflow committed partial charges: balance=%s charges=%d err=%v", balance, charges, err)
		}
	})
}

func modelMultiplierIntegrationReopen(t *testing.T, ctx context.Context, s *Store) *Store {
	t.Helper()
	var schema string
	if err := s.db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	configuration, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	configuration.RuntimeParams["search_path"] = schema
	reopened := New(stdlib.OpenDB(*configuration))
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func TestModelMultiplierHistoryCleanupPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	actor := globalUsageIntegrationUser(t, ctx, s, "multiplier-cleanup-actor", UserRoleMember)
	old := now.AddDate(0, 0, -10)
	params := SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, actor.ID, "historic multiplier", old), Model: "multiplier-cleanup", Multiplier: "0.8"}
	if _, err := s.SetModelMultiplier(ctx, params); err != nil {
		t.Fatal(err)
	}
	cutoff, err := InformationCutoff(now, 2)
	if err != nil {
		t.Fatal(err)
	}
	job := informationIntegrationDrain(t, ctx, s, actor.ID, cutoff)
	if job.Report.DeleteCounts["billing_operations"] != 1 || job.Report.DeleteCounts["billing_ledger_entries"] != 1 || job.Report.DeleteCounts["audit_events"] != 1 {
		t.Fatalf("multiplier history not included in cleanup: %+v", job.Report)
	}
	if _, err := s.SetModelMultiplier(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("cleaned multiplier operation was replayed: %v", err)
	}
	current, err := s.ListModelMultipliers(ctx, []string{params.Model})
	if err != nil || len(current) != 1 || !equalAllocationDecimal(current[0].Multiplier, params.Multiplier) || current[0].UpdatedAt == nil || !current[0].UpdatedAt.Equal(old) {
		t.Fatalf("cleanup deleted current multiplier configuration: %+v %v", current, err)
	}
}

func TestModelMultiplierMigrationPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Apply the old migration set in a disposable schema, insert real legacy
	// billing rows, then apply only the new migration and compare history.
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	configuration, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*configuration)
	defer admin.Close()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "multiplier_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop multiplier migration schema: %v", err)
		}
	}()
	configuration.RuntimeParams["search_path"] = schema
	s := New(stdlib.OpenDB(*configuration))
	defer s.Close()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var multiplierMigration Migration
	for _, migration := range migrations {
		if migration.Name == "0022_model_multipliers.sql" {
			multiplierMigration = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply legacy %s: %v", migration.Name, err)
		}
	}
	if multiplierMigration.Name == "" {
		t.Fatal("model multiplier migration missing")
	}
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "multiplier-legacy")
	now := time.Now().UTC().Truncate(time.Microsecond)
	requestID := "multiplier-legacy-settled-request"
	billingIntegrationComplete(t, ctx, s, user, device, key, requestID, now, 1_000_000, "billing-priced-model")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_reservations
		(request_id,user_id,api_key_id,requested_model,input_usd_per_million,cached_input_usd_per_million,output_usd_per_million,
		 cash_lot_cutoff,state,actual_input_tokens,actual_cached_input_tokens,actual_output_tokens,actual_cost_usd,charged_usd,uncovered_usd,settled_at)
		VALUES ($1,$2,$3,'billing-priced-model',1,0,0,1,'settled',1000000,0,0,1,0,1,$4)`, requestID, user.ID, key.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries
		(user_id,entry_type,amount_usd,request_id,model,actual_cost_usd,charged_usd,uncovered_usd,usage_requested_at,created_at)
		VALUES ($1,'usage_charge',1,$2,'billing-priced-model',1,0,1,$3,$3)`, user.ID, requestID, now); err != nil {
		t.Fatal(err)
	}
	before, err := s.GlobalUsage(ctx, now.Add(-time.Second), now.Add(time.Hour), "billing-priced-model", false, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, multiplierMigration.SQL); err != nil {
		t.Fatalf("apply multiplier migration: %v", err)
	}
	var reservationMultiplier, ledgerMultiplier, reservationCost, ledgerCost string
	if err := s.db.QueryRowContext(ctx, `SELECT r.pricing_multiplier::text,l.pricing_multiplier::text,r.actual_cost_usd::text,l.amount_usd::text
		FROM billing_reservations r JOIN billing_ledger_entries l USING(request_id) WHERE r.request_id=$1`, requestID).
		Scan(&reservationMultiplier, &ledgerMultiplier, &reservationCost, &ledgerCost); err != nil {
		t.Fatal(err)
	}
	if !equalAllocationDecimal(reservationMultiplier, "1") || !equalAllocationDecimal(ledgerMultiplier, "1") || reservationCost != "1.000000000000" || ledgerCost != "1.000000000000" {
		t.Fatalf("legacy snapshot/cost changed: %s/%s %s/%s", reservationMultiplier, ledgerMultiplier, reservationCost, ledgerCost)
	}
	// The migration under test has been verified above. Install the unrelated
	// plan schema before exercising current billing helpers for replay.
	for _, migration := range migrations {
		if migration.Name == "0024_subscription_plans.sql" {
			if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
				t.Fatalf("apply billing helper schema: %v", err)
			}
		}
	}
	if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "post migration multiplier", now), Model: "billing-priced-model", Multiplier: "9",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleBilling(ctx, requestID, now); err != nil {
		t.Fatal(err)
	}
	after, err := s.GlobalUsage(ctx, now.Add(-time.Second), now.Add(time.Hour), "billing-priced-model", false, now.Add(-time.Second))
	if err != nil || fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("migration/settings/replay changed historical report: before=%+v after=%+v err=%v", before, after, err)
	}
}
