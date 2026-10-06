//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestModelPricesPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	actor := globalUsageIntegrationUser(t, ctx, s, "prices-actor", UserRoleMember)
	const model = "matrix-model"
	rule := config.ModelPricing{CacheWriteMode: config.CacheWriteSeparate, MaxInputTokens: 1000000, LongContextThresholdTokens: 200000, ServiceTiers: map[string]config.ServiceTierPricing{}}
	for _, tier := range []string{config.PricingTierStandard, config.PricingTierFlex, config.PricingTierFast} {
		makePrice := func() *config.TokenPricing {
			cache := "0.75"
			return &config.TokenPricing{InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0.1", CacheWriteUSDPerMillion: &cache, OutputUSDPerMillion: "4"}
		}
		rule.ServiceTiers[tier] = config.ServiceTierPricing{Short: makePrice(), Long: makePrice()}
	}
	pricing := config.UsagePricing{SchemaVersion: 2, FallbackPolicy: config.PricingFallbackPolicy{UnknownServiceTier: config.FallbackMaxPublished, MissingPriceCombination: config.FallbackMaxPublished, MissingCacheWriteTokens: config.FallbackAllUncachedAsWrite}, Models: map[string]config.ModelPricing{model: rule}}
	structure, err := pricing.ModelPriceStructureID(model)
	if err != nil {
		t.Fatal(err)
	}
	get := func() ModelPrice {
		t.Helper()
		values, err := s.ListModelPrices(ctx, pricing)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range values {
			if v.Model == model {
				return v
			}
		}
		t.Fatal("model missing")
		return ModelPrice{}
	}
	initial := get()
	if initial.Source != "config" || initial.Version != 0 || initial.EffectivePrice == nil {
		t.Fatalf("initial %+v", initial)
	}
	price, err := config.NormalizeModelPrice(2, model, rule)
	if err != nil {
		t.Fatal(err)
	}
	for _, contexts := range price.ServiceTiers {
		for _, p := range []*config.TokenPricing{contexts.Short, contexts.Long} {
			p.InputUSDPerMillion = "0"
			p.CachedInputUSDPerMillion = "0.000000000001"
			*p.CacheWriteUSDPerMillion = "999999999999999999.999999999999"
			p.OutputUSDPerMillion = "2.00"
		}
	}
	write := func() BillingWriteParams { return billingIntegrationWrite(t, actor.ID, "pricing regression", now) }
	params := SetModelPriceParams{BillingWriteParams: write(), Pricing: pricing, Model: model, Action: "save", StructureID: structure, Price: &price}
	saved, err := s.SetModelPrice(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 || saved.Source != "override" || saved.UpdatedAt == nil || !saved.UpdatedAt.Equal(now) {
		t.Fatalf("save %+v", saved)
	}
	for _, contexts := range saved.EffectivePrice.ServiceTiers {
		for _, p := range []*config.TokenPricing{contexts.Short, contexts.Long} {
			if p.InputUSDPerMillion != "0" || p.OutputUSDPerMillion != "2" || p.CachedInputUSDPerMillion != "0.000000000001" || *p.CacheWriteUSDPerMillion != "999999999999999999.999999999999" {
				t.Fatalf("matrix price %+v", p)
			}
		}
	}
	if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{BillingWriteParams: write(), Model: model, Multiplier: "2"}); err != nil {
		t.Fatal(err)
	}
	stale := params
	stale.BillingWriteParams = write()
	if _, err := s.SetModelPrice(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version err=%v", err)
	}
	restore := SetModelPriceParams{BillingWriteParams: write(), Pricing: pricing, Model: model, Action: "restore", StructureID: structure, Version: 1}
	restored, err := s.SetModelPrice(ctx, restore)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Version != 2 || restored.Source != "config" || !equalAllocationDecimal(restored.Multiplier, "2") {
		t.Fatalf("restore %+v", restored)
	}
	state, err := s.GetBillingState(ctx, actor.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, entry := range state.Ledger {
		if entry.EntryType != "model_price" {
			continue
		}
		var snapshot struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(entry.TransactionSnapshot, &snapshot); err != nil {
			t.Fatal(err)
		}
		if entry.Model == nil || *entry.Model != model || entry.ActorUserID == nil || *entry.ActorUserID != actor.ID {
			t.Fatalf("price ledger attribution: %+v", entry)
		}
		actions[snapshot.Action]++
	}
	if actions["save"] != 1 || actions["restore"] != 1 {
		t.Fatalf("owner ledger did not show save and restore: %+v", actions)
	}
	replayed, err := s.SetModelPrice(ctx, params)
	if err != nil {
		t.Fatalf("replay first model price response: %v", err)
	}
	if replayed.UpdatedAt == nil || saved.UpdatedAt == nil || !replayed.UpdatedAt.Equal(*saved.UpdatedAt) {
		t.Fatalf("first response replay changed update time: got=%v want=%v", replayed.UpdatedAt, saved.UpdatedAt)
	}
	// Database scans and JSON replay can use different locations for the same
	// instant. Compare the timestamp semantically, then every remaining field.
	replayedFields, savedFields := replayed, saved
	replayedFields.UpdatedAt, savedFields.UpdatedAt = nil, nil
	if !reflect.DeepEqual(replayedFields, savedFields) {
		t.Fatalf("first response replay changed: %+v %+v", replayed, saved)
	}
	if got := get(); got.Version != 2 || got.Source != "config" {
		t.Fatalf("replay rewrote newer config: %+v", got)
	}
	bad := params
	bad.BillingWriteParams = write()
	bad.Version = 2
	bad.SourceIP = "invalid-ip"
	if _, err := s.SetModelPrice(ctx, bad); err == nil {
		t.Fatal("invalid audit unexpectedly committed")
	}
	if got := get(); got.Version != 2 || got.Source != "config" {
		t.Fatalf("audit failure partially committed %+v", got)
	}
	var artifacts int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_operations WHERE operation_id=$1)+(SELECT count(*) FROM billing_ledger_entries WHERE operation_id=$1)+(SELECT count(*) FROM audit_events WHERE metadata->>'operation_id'=$1::text)`, bad.OperationID).Scan(&artifacts); err != nil || artifacts != 0 {
		t.Fatalf("audit rollback artifacts=%d err=%v", artifacts, err)
	}
	params.BillingWriteParams = write()
	params.Version = 2
	if _, err := s.SetModelPrice(ctx, params); err != nil {
		t.Fatal(err)
	}
	changedRule := rule
	changedRule.LongContextThresholdTokens++
	pricing.Models = map[string]config.ModelPricing{model: changedRule}
	conflict := get()
	if conflict.Source != "conflict" || !conflict.Conflict || conflict.EffectivePrice != nil || conflict.Version != 3 {
		t.Fatalf("config shape conflict %+v", conflict)
	}
	// A committed operation still replays when the process has a new catalog.
	params.Pricing = pricing
	replayed, err = s.SetModelPrice(ctx, params)
	if err != nil || replayed.Version != 3 || replayed.Source != "override" {
		t.Fatalf("replay after config change %+v %v", replayed, err)
	}
	resolve := SetModelPriceParams{BillingWriteParams: write(), Pricing: pricing, Model: model, Action: "restore", StructureID: conflict.StructureID, Version: 3}
	resolved, err := s.SetModelPrice(ctx, resolve)
	if err != nil || resolved.Version != 4 || resolved.Conflict || resolved.Source != "config" {
		t.Fatalf("restore conflict %+v %v", resolved, err)
	}
	// A model removed from the deployment keeps the restored monotonic version.
	without, err := s.ListModelPrices(ctx, config.UsagePricing{SchemaVersion: 1, Models: map[string]config.ModelPricing{}})
	if err != nil || len(without) != 1 || without[0].Model != config.InternalGovernanceModel || without[0].Editable || without[0].EffectivePrice.InputUSDPerMillion != "0" {
		t.Fatalf("removed catalog %+v %v", without, err)
	}
	if got := get(); got.Version != 4 {
		t.Fatalf("reintroduced model lost version %+v", got)
	}
}

func TestModelPriceStructureConflictAdmissionPostgresIntegration(t *testing.T) {
	for name, change := range map[string]func(*config.ModelPricing){
		"threshold": func(rule *config.ModelPricing) { rule.LongContextThresholdTokens-- },
		"maximum":   func(rule *config.ModelPricing) { rule.MaxInputTokens++ },
		"tier": func(rule *config.ModelPricing) {
			rule.ServiceTiers[config.PricingTierFlex] = rule.ServiceTiers[config.PricingTierStandard]
		},
		"context": func(rule *config.ModelPricing) {
			contexts := rule.ServiceTiers[config.PricingTierStandard]
			contexts.Long = nil
			rule.ServiceTiers[config.PricingTierStandard] = contexts
		},
		"cache": func(rule *config.ModelPricing) {
			rule.CacheWriteMode = config.CacheWriteSeparate
			for _, contexts := range rule.ServiceTiers {
				for _, price := range []*config.TokenPricing{contexts.Short, contexts.Long} {
					cache := "1.25"
					price.CacheWriteUSDPerMillion = &cache
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s := informationIntegrationStore(t, ctx)
			now := time.Now().UTC().Truncate(time.Microsecond)
			user, device, key := billingIntegrationPrincipal(t, ctx, s, "shape-conflict")
			if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "fund conflict regression", now), UserID: user.ID, USDAmount: "10"}); err != nil {
				t.Fatal(err)
			}
			const model = "shape-conflict"
			original := modelPriceAcceptanceCatalog(model, config.PricingSchemaV2, "1")
			if _, err := s.SetModelPrice(ctx, modelPriceAcceptanceWrite(t, user.ID, model, original, 0, "3", now)); err != nil {
				t.Fatal(err)
			}
			changed := modelPriceAcceptanceCatalog(model, config.PricingSchemaV2, "1")
			rule := changed.Models[model]
			change(&rule)
			changed.Models[model] = rule
			assertConflict := func(request string, pricing config.UsagePricing) {
				t.Helper()
				params := modelPriceAcceptanceAdmission(t, user, device, key, model, request, pricing, now)
				if _, err := s.AdmitRequest(ctx, params); !errors.Is(err, ErrConflict) {
					t.Fatalf("changed %s did not block admission: %v", name, err)
				}
				var artifacts int
				if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_reservations WHERE request_id=$1)+
					(SELECT count(*) FROM quota_reservations WHERE request_id=$1)+
					(SELECT count(*) FROM usage_requests WHERE request_id=$1)+
					(SELECT count(*) FROM concurrency_leases WHERE request_id=$1)`, request).Scan(&artifacts); err != nil || artifacts != 0 {
					t.Fatalf("structure conflict left %d artifacts: %v", artifacts, err)
				}
			}
			assertConflict("shape-conflict-before-save", changed)
			structure, err := changed.ModelPriceStructureID(model)
			if err != nil {
				t.Fatal(err)
			}
			override, err := config.NormalizeModelPrice(config.PricingSchemaV2, model, rule)
			if err != nil {
				t.Fatal(err)
			}
			for _, contexts := range override.ServiceTiers {
				for _, price := range []*config.TokenPricing{contexts.Short, contexts.Long} {
					if price != nil {
						price.InputUSDPerMillion = "4"
					}
				}
			}
			saved, err := s.SetModelPrice(ctx, SetModelPriceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "accept new price structure", now),
				Pricing: changed, Model: model, Action: "save", Version: 1, StructureID: structure, Price: &override})
			if err != nil || saved.Conflict || saved.Version != 2 {
				t.Fatalf("save conflict recovery: %+v %v", saved, err)
			}
			assertAdmission := func(request string, pricing config.UsagePricing, want string) {
				t.Helper()
				admitted, err := s.AdmitRequest(ctx, modelPriceAcceptanceAdmission(t, user, device, key, model, request, pricing, now))
				if err != nil || admitted.Billing == nil {
					t.Fatalf("admission after resolving structure: %+v %v", admitted, err)
				}
				snapshot, err := config.ParsePricingSnapshot(admitted.Billing.PricingSnapshot)
				if err != nil || snapshot.Rule.ServiceTiers[config.PricingTierStandard].Short.InputUSDPerMillion != want {
					t.Fatalf("resolved snapshot: %+v %v", snapshot, err)
				}
			}
			assertAdmission("shape-conflict-after-save", changed, "4")
			assertConflict("shape-conflict-before-restore", original)
			structure, err = original.ModelPriceStructureID(model)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := s.SetModelPrice(ctx, SetModelPriceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "restore original price structure", now),
				Pricing: original, Model: model, Action: "restore", Version: 2, StructureID: structure})
			if err != nil || restored.Conflict || restored.Version != 3 {
				t.Fatalf("restore conflict recovery: %+v %v", restored, err)
			}
			assertAdmission("shape-conflict-after-restore", original, "1")
		})
	}
}
