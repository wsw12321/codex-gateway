//go:build integration

package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestModelPriceOverrideMatrixSettlementPostgresIntegration(t *testing.T) {
	// Each cell has distinct prices so selecting the wrong tier, context, token
	// category, or deployment default changes both the applied prices and cost.
	rows := []struct {
		tier, context, actualTier    string
		input, cached, write, output string
		separateUSD, includedUSD     string
	}{
		{"standard", "short", "default", "1.1", "0", "1.3", "0", "0.000188000000", "0.000176000000"},
		{"standard", "long", "default", "2.1", "2.2", "0", "2.4", "0.001330000000", "0.001456000000"},
		{"flex", "short", "flex", "3.1", "3.2", "3.3", "3.4", "0.000908000000", "0.000896000000"},
		{"flex", "long", "flex", "4.1", "4.2", "4.3", "4.4", "0.002828000000", "0.002816000000"},
		{"fast", "short", "priority", "5.1", "5.2", "5.3", "5.4", "0.001468000000", "0.001456000000"},
		{"fast", "long", "priority", "6.1", "6.2", "6.3", "6.4", "0.004188000000", "0.004176000000"},
	}
	for _, mode := range []string{config.CacheWriteSeparate, config.CacheWriteIncludedInInput} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s := informationIntegrationStore(t, ctx)
			now := time.Now().UTC().Truncate(time.Microsecond)
			user, device, key := billingIntegrationPrincipal(t, ctx, s, "price-matrix")
			if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Fund price matrix", now), UserID: user.ID, USDAmount: "10"}); err != nil {
				t.Fatal(err)
			}
			const model = "price-matrix"
			pricing := modelPriceAcceptanceCatalog(model, config.PricingSchemaV2, "99")
			configured := config.ModelPricing{CacheWriteMode: mode, MaxInputTokens: 1000,
				LongContextThresholdTokens: 200, ServiceTiers: map[string]config.ServiceTierPricing{}}
			override := configured
			override.ServiceTiers = map[string]config.ServiceTierPricing{}
			for _, row := range rows {
				base := &config.TokenPricing{InputUSDPerMillion: "99", CachedInputUSDPerMillion: "99", OutputUSDPerMillion: "99"}
				price := &config.TokenPricing{InputUSDPerMillion: row.input, CachedInputUSDPerMillion: row.cached, OutputUSDPerMillion: row.output}
				if mode == config.CacheWriteSeparate {
					baseWrite, overrideWrite := "99", row.write
					base.CacheWriteUSDPerMillion, price.CacheWriteUSDPerMillion = &baseWrite, &overrideWrite
				}
				baseTier, priceTier := configured.ServiceTiers[row.tier], override.ServiceTiers[row.tier]
				if row.context == config.ContextClassShort {
					baseTier.Short, priceTier.Short = base, price
				} else {
					baseTier.Long, priceTier.Long = base, price
				}
				configured.ServiceTiers[row.tier], override.ServiceTiers[row.tier] = baseTier, priceTier
			}
			pricing.Models[model] = configured
			structureID, err := pricing.ModelPriceStructureID(model)
			if err != nil {
				t.Fatal(err)
			}
			// The writer and admission reader use independent Store instances.
			writer := modelMultiplierIntegrationReopen(t, ctx, s)
			if _, err := writer.SetModelPrice(ctx, SetModelPriceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Override complete matrix", now),
				Pricing: pricing, Model: model, Action: "save", StructureID: structureID, Price: &override}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.SetModelMultiplier(ctx, SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Matrix multiplier", now), Model: model, Multiplier: "2"}); err != nil {
				t.Fatal(err)
			}
			for index := 0; index < len(rows)+2; index++ {
				row := rows[index%len(rows)]
				actualTier, expectedTier, fallback := row.actualTier, row.tier, ""
				if index >= len(rows) {
					// A missing actual tier selects the component maxima from this
					// context. Here all four maxima belong to the fast-tier cell.
					row = rows[4+index-len(rows)]
					actualTier, expectedTier, fallback = "", config.PricingTierMaxPublished, config.FallbackMissingServiceTier
				}
				t.Run(fmt.Sprintf("%s-%s-%d", expectedTier, row.context, index), func(t *testing.T) {
					requestID := fmt.Sprintf("price-matrix-request-%d", index)
					at := now.Add(time.Duration(index) * time.Second)
					params := modelPriceAcceptanceAdmission(t, user, device, key, model, requestID, pricing, at)
					params.Usage.RequestedServiceTier, params.Billing.RequestedServiceTier = row.actualTier, row.actualTier
					admitted, err := s.AdmitRequest(ctx, params)
					if err != nil {
						t.Fatal(err)
					}
					if admitted.Billing == nil {
						t.Fatal("admission omitted billing snapshot")
					}
					snapshot, err := config.ParsePricingSnapshot(admitted.Billing.PricingSnapshot)
					if err != nil || !reflect.DeepEqual(snapshot.Rule, override) {
						t.Fatalf("admission did not snapshot the complete override: %+v error=%v", snapshot, err)
					}
					inputTokens := int64(100)
					if row.context == config.ContextClassLong {
						inputTokens = 300
					}
					if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: requestID, State: "completed", HTTPStatus: 200,
						InputTokens: inputTokens, CachedInputTokens: 20, CacheWriteTokens: 30, CacheWriteTokensPresent: true,
						OutputTokens: 40, ActualServiceTier: actualTier, ActualModel: model, CompletedAt: at.Add(time.Second)}); err != nil {
						t.Fatal(err)
					}
					if err := s.SettleRequest(ctx, requestID, at.Add(2*time.Second)); err != nil {
						t.Fatal(err)
					}
					var cost, charged, uncovered, input, cached, write, output, tier, contextClass, cacheMode, reason, multiplier string
					if err := s.db.QueryRowContext(ctx, `SELECT amount_usd::text,charged_usd::text,uncovered_usd::text,
						applied_input_usd_per_million::text,applied_cached_input_usd_per_million::text,
						applied_cache_write_usd_per_million::text,applied_output_usd_per_million::text,
						pricing_service_tier,context_class,cache_write_mode,coalesce(pricing_fallback_reason,''),pricing_multiplier::text
						FROM billing_ledger_entries WHERE request_id=$1`, requestID).
						Scan(&cost, &charged, &uncovered, &input, &cached, &write, &output, &tier, &contextClass, &cacheMode, &reason, &multiplier); err != nil {
						t.Fatal(err)
					}
					wantCost, wantWrite := row.separateUSD, row.write
					if mode == config.CacheWriteIncludedInInput {
						wantCost, wantWrite = row.includedUSD, "0"
					}
					if cost != wantCost || charged != wantCost || uncovered != "0.000000000000" ||
						!equalAllocationDecimal(input, row.input) || !equalAllocationDecimal(cached, row.cached) ||
						!equalAllocationDecimal(write, wantWrite) || !equalAllocationDecimal(output, row.output) ||
						tier != expectedTier || contextClass != row.context || cacheMode != mode || reason != fallback || !equalAllocationDecimal(multiplier, "2") {
						t.Fatalf("settlement cost=%s/%s/%s want=%s prices=%s/%s/%s/%s want=%s/%s/%s/%s tier=%s context=%s cache=%s fallback=%s multiplier=%s",
							cost, charged, uncovered, wantCost, input, cached, write, output, row.input, row.cached, wantWrite, row.output, tier, contextClass, cacheMode, reason, multiplier)
					}
				})
			}
		})
	}
}
