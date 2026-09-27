//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestGeminiBillingPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	const model = "gemini-3.1-pro-preview"
	snapshot, rule, ok, err := pricing.ModelSnapshot(model)
	if err != nil || !ok {
		t.Fatalf("Gemini snapshot: ok=%t err=%v", ok, err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	actor := globalUsageIntegrationUser(t, ctx, repository, "gemini-actor-"+suffix, UserRoleMember)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct {
		name        string
		endpoint    string
		inputTokens int64
		actualTier  string
		context     string
		cost        string
	}{
		{"short", "responses", 200_000, "default", config.ContextClassShort, "0.503200000000"},
		{"long", "responses", 200_001, "default", config.ContextClassLong, "0.945804000000"},
		{"short-missing-tier", "responses", 200_000, "", config.ContextClassShort, "0.503200000000"},
		{"long-missing-tier", "responses", 200_001, "", config.ContextClassLong, "0.945804000000"},
		{"native-short", "gemini.generateContent", 200_000, "default", config.ContextClassShort, "0.503200000000"},
		{"native-long", "gemini.generateContent", 200_001, "default", config.ContextClassLong, "0.945804000000"},
		{"native-stream-short", "gemini.streamGenerateContent", 200_000, "default", config.ContextClassShort, "0.503200000000"},
		{"native-stream-long", "gemini.streamGenerateContent", 200_001, "default", config.ContextClassLong, "0.945804000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, device, key := billingIntegrationPrincipal(t, ctx, repository, "gemini-"+tc.name+"-"+suffix)
			if _, err := repository.PutSubscription(ctx, PutSubscriptionParams{
				BillingWriteParams: billingIntegrationWrite(t, actor.ID, "fund Gemini billing regression", now),
				UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "5",
			}); err != nil {
				t.Fatal(err)
			}
			requestID := billingIntegrationRequestID(suffix, "gemini-"+tc.name, 1)
			params := billingIntegrationAdmission(user, device, key, requestID, now.Add(time.Second))
			params.Usage.Model = model
			params.Usage.Endpoint = tc.endpoint
			params.Usage.RequestedServiceTier = "default"
			params.Usage.PricingRuleVersion = config.PricingSchemaV2
			params.Billing = &BillingReservationParams{
				RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: model,
				PricingRuleVersion: config.PricingSchemaV2, BillingMode: BillingModeOpenAIAPIEquivalent,
				PricingCatalogAsOf: pricing.CatalogAsOf, PricingModel: model,
				PricingSnapshot: snapshot, CacheWriteMode: rule.CacheWriteMode,
				RequestedServiceTier: "default", Now: now.Add(time.Second),
			}
			if _, err := repository.AdmitRequest(ctx, params); err != nil {
				t.Fatalf("admit Gemini request: %v", err)
			}
			// The adapter reports 10,000 answer tokens plus 100 thinking tokens
			// as output=10,100, with reasoning=100 retained only as a breakdown.
			if _, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
				RequestID: requestID, State: "completed", HTTPStatus: 200,
				CompletedAt: now.Add(2 * time.Second), ActualModel: model, ActualServiceTier: tc.actualTier,
				InputTokens: tc.inputTokens, CachedInputTokens: 10_000,
				OutputTokens: 10_100, ReasoningTokens: 100,
			}); err != nil {
				t.Fatalf("complete Gemini usage: %v", err)
			}
			start := make(chan struct{})
			errorsByAttempt := make([]error, 2)
			var wait sync.WaitGroup
			for index := range errorsByAttempt {
				wait.Add(1)
				go func(index int) {
					defer wait.Done()
					<-start
					errorsByAttempt[index] = repository.SettleRequest(ctx, requestID, now.Add(3*time.Second))
				}(index)
			}
			close(start)
			wait.Wait()
			for _, err := range errorsByAttempt {
				if err != nil {
					t.Fatalf("concurrent Gemini settlement: %v", err)
				}
			}
			if err := repository.SettleRequest(ctx, requestID, now.Add(4*time.Second)); err != nil {
				t.Fatalf("repeat Gemini settlement: %v", err)
			}
			var ledgerCount int
			var amount, charged, uncovered, contextClass, pricingTier, fallback, cacheWriteMode string
			var outputTokens, reasoningTokens, quotaTokens, cacheWrites, completedRequests, usedTokens int64
			if err := repository.db.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1),
				l.amount_usd::text,l.charged_usd::text,l.uncovered_usd::text,
				l.context_class,l.pricing_service_tier,COALESCE(l.pricing_fallback_reason,''),
				l.cache_write_mode,l.output_tokens,u.reasoning_tokens,q.actual_tokens,l.cache_write_tokens,
				c.requests_completed,c.tokens_used
				FROM billing_ledger_entries l
				JOIN usage_requests u USING (request_id)
				JOIN quota_reservations q USING (request_id)
				JOIN quota_counters c ON c.scope_type='user' AND c.scope_id=$2 AND c.quota_day=$3::date
				WHERE l.request_id=$1`, requestID, user.ID, now,
			).Scan(&ledgerCount, &amount, &charged, &uncovered, &contextClass, &pricingTier, &fallback,
				&cacheWriteMode, &outputTokens, &reasoningTokens, &quotaTokens, &cacheWrites,
				&completedRequests, &usedTokens); err != nil {
				t.Fatalf("read Gemini settlement: %v", err)
			}
			wantTier, wantFallback := config.PricingTierStandard, ""
			if tc.actualTier == "" {
				wantTier, wantFallback = config.PricingTierMaxPublished, config.FallbackMissingServiceTier
			}
			if ledgerCount != 1 || amount != tc.cost || charged != tc.cost || uncovered != "0.000000000000" ||
				contextClass != tc.context || pricingTier != wantTier || fallback != wantFallback ||
				cacheWriteMode != config.CacheWriteIncludedInInput || cacheWrites != 0 ||
				outputTokens != 10_100 || reasoningTokens != 100 || quotaTokens != tc.inputTokens+10_100 ||
				completedRequests != 1 || usedTokens != tc.inputTokens+10_100 {
				t.Fatalf("Gemini settlement: ledger=%d amount=%s charged=%s uncovered=%s context=%s tier=%s fallback=%s cache=%s/%d output=%d reasoning=%d quota=%d requests=%d used=%d",
					ledgerCount, amount, charged, uncovered, contextClass, pricingTier, fallback,
					cacheWriteMode, cacheWrites, outputTokens, reasoningTokens, quotaTokens, completedRequests, usedTokens)
			}
			if err := repository.AggregateUsageDay(ctx, now, "UTC"); err != nil {
				t.Fatalf("aggregate Gemini day: %v", err)
			}
			if err := repository.AggregateUsageMonth(ctx, now, "UTC"); err != nil {
				t.Fatalf("aggregate Gemini month: %v", err)
			}
			for _, table := range []string{"usage_daily", "usage_monthly"} {
				var endpoint string
				var requests, input, cached, output, reasoning int64
				if err := repository.db.QueryRowContext(ctx, `SELECT endpoint,request_count,
					input_tokens,cached_input_tokens,output_tokens,reasoning_tokens FROM `+table+`
					WHERE user_id=$1`, user.ID).Scan(&endpoint, &requests, &input, &cached, &output, &reasoning); err != nil {
					t.Fatalf("read %s: %v", table, err)
				}
				if endpoint != tc.endpoint || requests != 1 || input != tc.inputTokens || cached != 10_000 || output != 10_100 || reasoning != 100 {
					t.Fatalf("%s rollup: endpoint=%s requests=%d input=%d cached=%d output=%d reasoning=%d", table, endpoint, requests, input, cached, output, reasoning)
				}
			}

			// A funded generation request that fails traffic quota must roll back
			// the billing reservation created earlier in the admission transaction.
			rejectedID := requestID + "-quota-rejected"
			params.Quota.RequestID, params.Usage.RequestID, params.Billing.RequestID = rejectedID, rejectedID, rejectedID
			params.Quota.Limits.KeyDailyRequests = 1
			if _, err := repository.AdmitRequest(ctx, params); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("Gemini quota rejection: %v", err)
			}
			var artifacts int
			if err := repository.db.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM quota_reservations WHERE request_id=$1) +
				(SELECT count(*) FROM usage_requests WHERE request_id=$1) +
				(SELECT count(*) FROM billing_reservations WHERE request_id=$1) +
				(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1)`, rejectedID).Scan(&artifacts); err != nil || artifacts != 0 {
				t.Fatalf("rejected Gemini request artifacts=%d err=%v", artifacts, err)
			}
		})
	}
}
