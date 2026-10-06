//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

func modelPriceAcceptanceCatalog(model string, schema int, input string) config.UsagePricing {
	rule := config.ModelPricing{InputUSDPerMillion: input, CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0"}
	pricing := config.UsagePricing{SchemaVersion: schema, CatalogAsOf: "2026-10-06", Models: map[string]config.ModelPricing{}}
	if schema == config.PricingSchemaV2 {
		pricing.FallbackPolicy = config.PricingFallbackPolicy{UnknownServiceTier: config.FallbackMaxPublished,
			MissingPriceCombination: config.FallbackMaxPublished, MissingCacheWriteTokens: config.FallbackAllUncachedAsWrite}
		rule = config.ModelPricing{CacheWriteMode: config.CacheWriteIncludedInInput,
			MaxInputTokens: 2_000_000, LongContextThresholdTokens: 1_000_000,
			ServiceTiers: map[string]config.ServiceTierPricing{"standard": {
				Short: &config.TokenPricing{InputUSDPerMillion: input, CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0"},
				Long:  &config.TokenPricing{InputUSDPerMillion: input, CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0"},
			}}}
	}
	pricing.Models[model] = rule
	return pricing
}

func modelPriceAcceptanceWrite(t *testing.T, actor, model string, pricing config.UsagePricing, version int64, input string, now time.Time) SetModelPriceParams {
	t.Helper()
	structure, err := pricing.ModelPriceStructureID(model)
	if err != nil {
		t.Fatal(err)
	}
	rule := modelPriceAcceptanceCatalog(model, pricing.SchemaVersion, input).Models[model]
	return SetModelPriceParams{BillingWriteParams: billingIntegrationWrite(t, actor, "Model price acceptance", now),
		Pricing: pricing, Model: model, Action: "save", Version: version, StructureID: structure, Price: &rule}
}

func modelPriceAcceptanceAdmission(t *testing.T, user User, device Device, key APIKey, model, request string, pricing config.UsagePricing, now time.Time) AdmitRequestParams {
	t.Helper()
	params := billingIntegrationAdmission(user, device, key, request, now)
	params.Usage.Model, params.Billing.Model = model, model
	params.Usage.PricingRuleVersion, params.Billing.PricingRuleVersion = pricing.SchemaVersion, pricing.SchemaVersion
	rule := pricing.Models[model]
	if pricing.SchemaVersion == config.PricingSchemaV1 {
		params.Billing.InputUSDPerMillion, params.Billing.CachedInputUSDPerMillion, params.Billing.OutputUSDPerMillion = rule.InputUSDPerMillion, rule.CachedInputUSDPerMillion, rule.OutputUSDPerMillion
	} else {
		params.Billing.InputUSDPerMillion, params.Billing.CachedInputUSDPerMillion, params.Billing.OutputUSDPerMillion = "", "", ""
		params.Billing.PricingModel, params.Billing.PricingCatalogAsOf = model, pricing.CatalogAsOf
		params.Billing.CacheWriteMode, params.Billing.BillingMode = rule.CacheWriteMode, BillingModeOpenAIAPIEquivalent
		params.Billing.PricingSnapshot = billingIntegrationV2Snapshot(t, model, rule)
	}
	return params
}

func TestModelPriceSnapshotRecoveryPostgresIntegration(t *testing.T) {
	for _, schema := range []int{config.PricingSchemaV1, config.PricingSchemaV2} {
		t.Run(fmt.Sprintf("schema-%d", schema), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s := informationIntegrationStore(t, ctx)
			now := time.Now().UTC().Truncate(time.Microsecond)
			user, device, key := billingIntegrationPrincipal(t, ctx, s, "price-snapshot")
			if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Fund snapshot test", now), UserID: user.ID, USDAmount: "100"}); err != nil {
				t.Fatal(err)
			}
			const model = "price-snapshot"
			pricing := modelPriceAcceptanceCatalog(model, schema, "1")
			if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Snapshot multiplier", now), Model: model, Multiplier: "2"}); err != nil {
				t.Fatal(err)
			}
			old := modelPriceAcceptanceAdmission(t, user, device, key, model, "model-price-old-request", pricing, now)
			if _, err := s.AdmitRequest(ctx, old); err != nil {
				t.Fatal(err)
			}
			other := modelMultiplierIntegrationReopen(t, ctx, s)
			if _, err := other.SetModelPrice(ctx, modelPriceAcceptanceWrite(t, user.ID, model, pricing, 0, "3", now)); err != nil {
				t.Fatal(err)
			}
			fresh := modelPriceAcceptanceAdmission(t, user, device, key, model, "model-price-new-request", pricing, now.Add(time.Second))
			if _, err := s.AdmitRequest(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			if _, err := other.SetModelPrice(ctx, modelPriceAcceptanceWrite(t, user.ID, model, pricing, 1, "9", now)); err != nil {
				t.Fatal(err)
			}
			for _, request := range []string{"model-price-new-request", "model-price-old-request"} {
				if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: request, State: "completed", HTTPStatus: 200,
					InputTokens: 1_000_000, ActualServiceTier: "default", CompletedAt: now.Add(2 * time.Second)}); err != nil {
					t.Fatal(err)
				}
			}
			// Recovery must not consult the mutable price table, even when it is
			// unavailable; each reservation contains all data needed to charge.
			lock, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			if _, err := lock.ExecContext(ctx, `LOCK TABLE billing_model_prices IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			recoverCtx, recoverCancel := context.WithTimeout(ctx, 3*time.Second)
			count, err := other.RetryUnsettledRequests(recoverCtx, 100)
			recoverCancel()
			if err != nil || count != 2 {
				t.Fatalf("recover old price snapshots: count=%d err=%v", count, err)
			}
			blocked := modelPriceAcceptanceAdmission(t, user, device, key, model, "model-price-blocked-request", pricing, now.Add(3*time.Second))
			blockedCtx, blockedCancel := context.WithTimeout(ctx, 150*time.Millisecond)
			_, err = s.AdmitRequest(blockedCtx, blocked)
			blockedCancel()
			if err == nil {
				t.Fatal("price read failure silently admitted at deployment defaults")
			}
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			var artifacts int
			if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_reservations WHERE request_id='model-price-blocked-request')+
				(SELECT count(*) FROM quota_reservations WHERE request_id='model-price-blocked-request')+
				(SELECT count(*) FROM usage_requests WHERE request_id='model-price-blocked-request')`).Scan(&artifacts); err != nil || artifacts != 0 {
				t.Fatalf("failed admission left artifacts=%d err=%v", artifacts, err)
			}
			for request, want := range map[string]string{"model-price-old-request": "2.000000000000", "model-price-new-request": "6.000000000000"} {
				var amount string
				if err := s.db.QueryRowContext(ctx, `SELECT amount_usd::text FROM billing_ledger_entries WHERE request_id=$1`, request).Scan(&amount); err != nil || amount != want {
					t.Fatalf("%s cost=%s want=%s err=%v", request, amount, want, err)
				}
			}
		})
	}
}

func TestModelPriceAdmissionLockPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "price-concurrency")
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Fund concurrency test", now), UserID: user.ID, USDAmount: "10"}); err != nil {
		t.Fatal(err)
	}
	const model = "price-concurrency"
	pricing := modelPriceAcceptanceCatalog(model, config.PricingSchemaV1, "1")
	other := modelMultiplierIntegrationReopen(t, ctx, s)
	params := modelPriceAcceptanceAdmission(t, user, device, key, model, "price-lock-reader", pricing, now)
	reader, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	reservation, err := reserveBillingTx(ctx, reader, *params.Billing)
	if err != nil || !equalAllocationDecimal(reservation.InputUSDPerMillion, "1") {
		t.Fatalf("first shared snapshot: %+v %v", reservation, err)
	}
	write := modelPriceAcceptanceWrite(t, user.ID, model, pricing, 0, "3", now)
	done := make(chan error, 1)
	go func() { _, err := other.SetModelPrice(ctx, write); done <- err }()
	modelPriceAcceptanceWaitLock(t, ctx, s, model)
	select {
	case err := <-done:
		t.Fatalf("writer bypassed admission transaction: %v", err)
	default:
	}
	if err := reader.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Hold the same exclusive lock an admin uses: a new admission cannot
	// observe the price until the update transaction commits.
	writer, err := other.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing.model_price.'||$1,0))`, model); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE billing_model_prices SET override_price=jsonb_set(override_price,'{input_usd_per_million}','"7"'::jsonb) WHERE model=$1`, model); err != nil {
		t.Fatal(err)
	}
	newParams := modelPriceAcceptanceAdmission(t, user, device, key, model, "price-lock-writer", pricing, now.Add(time.Second))
	admitted := make(chan RequestAdmission, 1)
	go func() { result, err := s.AdmitRequest(ctx, newParams); admitted <- result; done <- err }()
	modelPriceAcceptanceWaitLock(t, ctx, s, model)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	result := <-admitted
	if result.Billing == nil || !equalAllocationDecimal(result.Billing.InputUSDPerMillion, "7") {
		t.Fatalf("admission did not observe committed override: %+v", result)
	}
}

func modelPriceAcceptanceWaitLock(t *testing.T, ctx context.Context, s *Store, model string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := s.db.QueryRowContext(waitCtx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted
			AND classid=((hashtextextended('billing.model_price.'||$1,0)>>32)&4294967295)::oid
			AND objid=(hashtextextended('billing.model_price.'||$1,0)&4294967295)::oid)`, model).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-waitCtx.Done():
			t.Fatal("operation did not wait on the model transaction lock")
		case <-ticker.C:
		}
	}
}

func TestModelPriceHistoryCleanupPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	actor := globalUsageIntegrationUser(t, ctx, s, "price-cleanup-actor", UserRoleMember)
	const model = "price-cleanup"
	pricing := modelPriceAcceptanceCatalog(model, config.PricingSchemaV1, "1")
	write := modelPriceAcceptanceWrite(t, actor.ID, model, pricing, 0, "3", now.AddDate(0, 0, -10))
	if _, err := s.SetModelPrice(ctx, write); err != nil {
		t.Fatal(err)
	}
	cutoff, err := InformationCutoff(now, 2)
	if err != nil {
		t.Fatal(err)
	}
	job := informationIntegrationDrain(t, ctx, s, actor.ID, cutoff)
	for _, table := range []string{"billing_operations", "billing_ledger_entries", "audit_events"} {
		if job.Report.DeleteCounts[table] != 1 {
			t.Fatalf("price history cleanup missed %s: %+v", table, job.Report)
		}
	}
	if _, err := s.SetModelPrice(ctx, write); !errors.Is(err, ErrConflict) {
		t.Fatalf("cleaned operation replayed: %v", err)
	}
	values, err := s.ListModelPrices(ctx, pricing)
	if err != nil || len(values) != 2 || values[1].Model != model || values[1].Version != 1 || values[1].EffectivePrice == nil || !equalAllocationDecimal(values[1].EffectivePrice.InputUSDPerMillion, "3") {
		t.Fatalf("cleanup lost current price: %+v %v", values, err)
	}
	candidates, err := s.ListDeletableInformationUsers(ctx, actor.Username, 100, 0)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("price actor reference was not protected: %+v %v", candidates, err)
	}
}
