//go:build integration

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestAnthropicTTLSettlementAndRetentionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "anthropic-ttl")
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Fund Claude TTL billing", now), UserID: user.ID, USDAmount: "10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Claude multiplier", now), Model: "claude-test", Multiplier: "1.5"}); err != nil {
		t.Fatal(err)
	}
	five, hour := "3.75", "6"
	pricing := modelPriceAcceptanceCatalog("claude-test", config.PricingSchemaV2, "3")
	pricing.Models["claude-test"] = config.ModelPricing{
		CacheWriteMode: config.CacheWriteSeparateByTTL, MaxInputTokens: 200000, LongContextThresholdTokens: 200000,
		ServiceTiers: map[string]config.ServiceTierPricing{config.PricingTierStandard: {Short: &config.TokenPricing{
			InputUSDPerMillion: "3", CachedInputUSDPerMillion: "0.3", OutputUSDPerMillion: "15",
			CacheWrite5mUSDPerMillion: &five, CacheWrite1hUSDPerMillion: &hour,
		}}},
	}
	// The second request omits the TTL breakdown. Its quantity must remain
	// unknown while billing conservatively uses the captured higher price.
	for index, tc := range []struct {
		id                 string
		present            bool
		wantCost, fallback string
	}{
		{"claude-observed-request", true, "0.007132500000", ""},
		{"claude-unobserved-request", false, "0.008145000000", config.FallbackMissingCacheWriteTTL},
	} {
		at := now.Add(time.Duration(index) * time.Second)
		params := modelPriceAcceptanceAdmission(t, user, device, key, "claude-test", tc.id, pricing, at)
		params.Usage.Endpoint = "messages"
		params.Billing.BillingMode = BillingModeAnthropicAPIEquivalent
		if _, err := s.AdmitRequest(ctx, params); err != nil {
			t.Fatal(err)
		}
		completion := CompleteUsageRequestParams{RequestID: tc.id, State: "completed", HTTPStatus: 200,
			InputTokens: 1000, CachedInputTokens: 100, CacheWriteTokens: 500, CacheWriteTokensPresent: true,
			OutputTokens: 80, ActualModel: "claude-test", ActualServiceTier: "standard", CompletedAt: at.Add(time.Second),
			CacheWriteTTLPresent: tc.present}
		if tc.present {
			completion.CacheWrite5mTokens, completion.CacheWrite1hTokens = 300, 200
		}
		if _, err := s.CompleteUsageRequest(ctx, completion); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteUsageRequest(ctx, completion); err != nil {
			t.Fatalf("terminal replay: %v", err)
		}
		if tc.present {
			changed := completion
			changed.CacheWrite5mTokens, changed.CacheWrite1hTokens = 200, 300
			if _, err := s.CompleteUsageRequest(ctx, changed); !errors.Is(err, ErrConflict) {
				t.Fatalf("contradictory replay = %v", err)
			}
		}
		// Editing a deployment rule after admission cannot change the snapshot.
		*pricing.Models["claude-test"].ServiceTiers[config.PricingTierStandard].Short.CacheWrite1hUSDPerMillion = "999"
		for attempt := 0; attempt < 2; attempt++ {
			if err := s.SettleRequest(ctx, tc.id, at.Add(2*time.Second)); err != nil {
				t.Fatalf("settlement %d: %v", attempt, err)
			}
		}
		*pricing.Models["claude-test"].ServiceTiers[config.PricingTierStandard].Short.CacheWrite1hUSDPerMillion = "6"
		reservation, err := scanBillingReservation(s.db.QueryRowContext(ctx, `SELECT `+billingReservationColumns+` FROM billing_reservations WHERE request_id=$1`, tc.id))
		if err != nil {
			t.Fatal(err)
		}
		if reservation.ActualCostUSD == nil || *reservation.ActualCostUSD != tc.wantCost || reservation.ActualCacheWriteTTLPresent == nil || *reservation.ActualCacheWriteTTLPresent != tc.present || reservation.AppliedCacheWrite1hUSDPerMillion == nil || *reservation.AppliedCacheWrite1hUSDPerMillion != "6.000000000000" {
			t.Fatalf("settled snapshot = %+v", reservation)
		}
		entry, err := scanBillingLedgerEntry(s.db.QueryRowContext(ctx, `SELECT `+billingLedgerColumns+` FROM billing_ledger_entries WHERE request_id=$1 AND entry_type='usage_charge'`, tc.id))
		if err != nil {
			t.Fatal(err)
		}
		if entry.CacheWriteTokens == nil || *entry.CacheWriteTokens != 500 || entry.CacheWriteTTLPresent == nil || *entry.CacheWriteTTLPresent != tc.present {
			t.Fatalf("ledger metadata = %+v", entry)
		}
		if tc.present {
			if entry.CacheWrite5mTokens == nil || *entry.CacheWrite5mTokens != 300 || entry.CacheWrite1hTokens == nil || *entry.CacheWrite1hTokens != 200 {
				t.Fatalf("observed ledger = %+v", entry)
			}
		} else if entry.CacheWrite5mTokens != nil || entry.CacheWrite1hTokens != nil {
			t.Fatal("missing TTL breakdown was invented")
		}
		if tc.fallback == "" && entry.PricingFallbackReason != nil || tc.fallback != "" && (entry.PricingFallbackReason == nil || *entry.PricingFallbackReason != tc.fallback) {
			t.Fatalf("fallback = %v", entry.PricingFallbackReason)
		}
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1`, tc.id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("ledger count = %d, %v", count, err)
		}
	}
	if err := s.AggregateUsageDay(ctx, now, "UTC"); err != nil {
		t.Fatal(err)
	}
	if err := s.AggregateUsageMonth(ctx, now, "UTC"); err != nil {
		t.Fatal(err)
	}
	checkAggregates := func() {
		t.Helper()
		for _, table := range []string{"usage_daily", "usage_monthly"} {
			var total, five, hour int64
			if err := s.db.QueryRowContext(ctx, `SELECT sum(cache_write_tokens),sum(cache_write_5m_tokens),sum(cache_write_1h_tokens) FROM `+table+` WHERE user_id=$1`, user.ID).Scan(&total, &five, &hour); err != nil {
				t.Fatal(err)
			}
			if total != 1000 || five != 300 || hour != 200 {
				t.Fatalf("%s TTL aggregate = %d/%d/%d", table, total, five, hour)
			}
		}
	}
	checkAggregates()
	from, until := now.Add(-time.Minute), now.Add(time.Hour)
	rows, err := s.GlobalUsage(ctx, from, until, "claude-test", false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.UserID == user.ID {
			found = true
			if row.CacheWrite5mTokens != 300 || row.CacheWrite1hTokens != 200 {
				t.Fatalf("global TTL summary: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("missing global usage row")
	}
	// Simulate normal detail expiration, then rebuild from retained daily data.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM usage_requests WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.withTx(ctx, nil, func(tx *sql.Tx) error { return rebuildInformationMonthTx(ctx, tx, now.AddDate(0, 0, -1), "UTC") }); err != nil {
		t.Fatal(err)
	}
	checkAggregates()
	breakdown, err := s.GlobalPricingBreakdown(ctx, from, until, "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range breakdown {
		if row.Dimension == "service_tier" && (row.CacheWrite5mTokens != 300 || row.CacheWrite1hTokens != 200) {
			t.Fatalf("ledger TTL summary after cleanup: %+v", row)
		}
	}
}

func TestAnthropicCountTokensDoesNotChargePostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "claude-count")
	const requestID = "claude-count-no-charge"
	params := billingIntegrationAdmission(user, device, key, requestID, now)
	params.Usage.Endpoint, params.Usage.Model = "messages.count_tokens", "claude-test"
	params.Billing = nil
	params.Quota.ReservedTokens = 0
	if _, err := s.AdmitRequest(ctx, params); err != nil {
		t.Fatal(err)
	}
	var leases int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM concurrency_leases WHERE request_id=$1`, requestID).Scan(&leases); err != nil || leases != 1 {
		t.Fatalf("count tokens lease: %d, %v", leases, err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: requestID, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleRequest(ctx, requestID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var billed, ledger, active int
	var tokens, requests int64
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM billing_reservations WHERE request_id=$1),
		(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1),
		(SELECT count(*) FROM concurrency_leases WHERE request_id=$1),
		tokens_used, requests_completed FROM quota_counters WHERE scope_type='user' AND scope_id=$2`, requestID, user.ID).Scan(&billed, &ledger, &active, &tokens, &requests); err != nil {
		t.Fatal(err)
	}
	if billed != 0 || ledger != 0 || active != 0 || tokens != 0 || requests != 1 {
		t.Fatalf("count tokens settlement: billing=%d ledger=%d lease=%d tokens=%d requests=%d", billed, ledger, active, tokens, requests)
	}
}

func TestScopedCompletionDiscardsForeignAttributionAndSettlesPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "foreign-attribution")
	if _, err := s.AdjustUserBalance(ctx, AdjustUserBalanceParams{BillingWriteParams: billingIntegrationWrite(t, user.ID, "Fund trace isolation", now), UserID: user.ID, USDAmount: "10"}); err != nil {
		t.Fatal(err)
	}
	const foreignID = "0011223344556677"
	if err := s.EnsureUpstreamAccount(ctx, foreignID, now); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{UpstreamProviderAnthropic, UpstreamProviderAntigravity} {
		requestID := "scoped-attribution-" + provider
		params := billingIntegrationAdmission(user, device, key, requestID, now)
		params.Billing.InputUSDPerMillion, params.Billing.CachedInputUSDPerMillion, params.Billing.OutputUSDPerMillion = "1", "0", "0"
		if _, err := s.AdmitRequest(ctx, params); err != nil {
			t.Fatal(err)
		}
		completion := CompleteUsageRequestParams{RequestID: requestID, State: "completed", HTTPStatus: 200, InputTokens: 1_000_000,
			UpstreamAccountID: foreignID, CompletedAt: now.Add(time.Second)}
		for replay := 0; replay < 2; replay++ {
			row, err := s.WithUpstreamProvider(provider).CompleteUsageRequest(ctx, completion)
			if err != nil || row.UpstreamAccountID != nil || row.InputTokens != 1_000_000 || row.State != "failed" || row.ErrorCode == nil || *row.ErrorCode != "upstream_account_provider_mismatch" {
				t.Fatalf("foreign trace completion/replay = %+v, %v", row, err)
			}
			if err := s.SettleRequest(ctx, requestID, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		var account *string
		var cost string
		var active, entries int
		if err := s.db.QueryRowContext(ctx, `SELECT upstream_account_id, actual_cost_usd::text,
			(SELECT count(*) FROM concurrency_leases WHERE request_id=$1),
			(SELECT count(*) FROM billing_ledger_entries WHERE request_id=$1)
			FROM billing_ledger_entries WHERE request_id=$1 AND entry_type='usage_charge'`, requestID).Scan(&account, &cost, &active, &entries); err != nil {
			t.Fatal(err)
		}
		if account != nil || cost != "1.000000000000" || active != 0 || entries != 1 {
			t.Fatalf("foreign trace settlement account=%v cost=%s leases=%d entries=%d", account, cost, active, entries)
		}
	}
}
