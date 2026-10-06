//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
)

func TestGeminiOfficialModelsMigrationPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	schema := "gemini_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop Gemini migration schema: %v", err)
		}
	}()
	configuration.RuntimeParams["search_path"] = schema
	s := New(stdlib.OpenDB(*configuration))
	defer s.Close()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var geminiMigration Migration
	for _, migration := range migrations {
		if migration.Name == "0023_gemini_official_models.sql" {
			geminiMigration = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply legacy %s: %v", migration.Name, err)
		}
	}
	if geminiMigration.Name == "" {
		t.Fatal("Gemini migration missing")
	}
	// Current billing helpers need the unrelated plan, funding and price schemas while
	// this test leaves the Gemini migration unapplied until after seeding.
	for _, migration := range migrations {
		if migration.Name == "0024_subscription_plans.sql" || migration.Name == "0027_group_priority_billing.sql" || migration.Name == "0029_model_prices.sql" {
			if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
				t.Fatalf("apply billing helper schema: %v", err)
			}
		}
	}
	const oldModel = "gemini-3.1-pro-preview"
	models := []string{
		"gemini-3.8-flash-high", "gemini-3.8-flash-medium",
		"gemini-3.7-flash-high", "gemini-3.7-flash-medium",
		"gemini-3.6-flash-high", "gemini-3.6-flash-medium", "gemini-3.1-pro-high",
	}
	// Cover a prior operator-created grant as well as new rows: every target
	// model must require reauthorization, including disabled user accounts.
	if err := s.SyncModelAccessCatalog(ctx, append([]string{oldModel}, models...)); err != nil {
		t.Fatal(err)
	}
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "gemini-migration")
	disabled := globalUsageIntegrationUser(t, ctx, s, "gemini-migration-disabled", UserRoleMember)
	if err := s.DisableUser(ctx, disabled.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET model_allowlist=ARRAY[$2] WHERE id=$1`, key.ID, oldModel); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "legacy Gemini multiplier", now),
		Model:              oldModel, Multiplier: "2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSubscription(ctx, PutSubscriptionParams{
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "fund legacy Gemini", now),
		UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "10",
	}); err != nil {
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
	// Reuse equivalent current prices while retaining the historical model ID.
	rule := pricing.Models["gemini-pro-agent"]
	snapshot := billingIntegrationV2Snapshot(t, oldModel, rule)
	requestIDs := []string{"gemini-before-migration-settled", "gemini-before-migration-inflight"}
	for index, requestID := range requestIDs {
		params := billingIntegrationAdmission(user, device, key, requestID, now)
		params.Usage.Model = oldModel
		params.Usage.RequestedServiceTier = "default"
		params.Usage.PricingRuleVersion = config.PricingSchemaV2
		params.Billing = &BillingReservationParams{
			RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: oldModel,
			PricingRuleVersion: config.PricingSchemaV2, BillingMode: BillingModeOpenAIAPIEquivalent,
			PricingCatalogAsOf: "2026-09-16", PricingModel: oldModel,
			PricingSnapshot: snapshot, CacheWriteMode: config.CacheWriteIncludedInInput,
			RequestedServiceTier: "default", Now: now,
		}
		if _, err := s.AdmitRequest(ctx, params); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: requestID, State: "completed", HTTPStatus: 200, ActualModel: oldModel,
			ActualServiceTier: "default", InputTokens: 200_000, CachedInputTokens: 10_000,
			OutputTokens: 10_100, ReasoningTokens: 100, CompletedAt: now.Add(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if err := s.SettleRequest(ctx, requestID, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	readHistory := func() string {
		t.Helper()
		var history string
		if err := s.db.QueryRowContext(ctx, `SELECT jsonb_build_object('reservation',to_jsonb(r),'ledger',to_jsonb(l))::text
			FROM billing_reservations r JOIN billing_ledger_entries l USING(request_id) WHERE r.request_id=$1`, requestIDs[0]).Scan(&history); err != nil {
			t.Fatal(err)
		}
		return history
	}
	before := readHistory()
	if _, err := s.db.ExecContext(ctx, geminiMigration.SQL); err != nil {
		t.Fatalf("apply Gemini migration: %v", err)
	}
	if err := s.SyncModelAccessCatalog(ctx, models); err != nil {
		t.Fatal(err)
	}
	newUser := globalUsageIntegrationUser(t, ctx, s, "gemini-migration-new", UserRoleMember)
	for _, model := range models {
		for _, userID := range []string{user.ID, disabled.ID, newUser.ID} {
			if err := s.RequireModelAccess(ctx, userID, model); !errors.Is(err, ErrModelNotAllowed) {
				t.Fatalf("model %s user %s retained a grant: %v", model, userID, err)
			}
		}
	}
	catalog, err := s.ListModelAccessModels(ctx)
	if err != nil || len(catalog) != 7 {
		t.Fatalf("migrated catalog: %+v, %v", catalog, err)
	}
	for _, model := range catalog {
		if model.DefaultEnabled || model.EnabledUserCount != 0 || model.DisabledUserCount != 3 {
			t.Fatalf("Gemini defaults were not disabled: %+v", model)
		}
	}
	if err := s.RequireModelAccess(ctx, user.ID, oldModel); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("retired alias is still effective: %v", err)
	}
	var oldEnabled, oldActive bool
	var allowlist string
	if err := s.db.QueryRowContext(ctx, `SELECT a.enabled,d.catalog_active,array_to_string(k.model_allowlist,',')
		FROM user_model_access a JOIN model_access_defaults d USING(model) JOIN api_keys k ON k.user_id=a.user_id
		WHERE a.user_id=$1 AND a.model=$2 AND k.id=$3`, user.ID, oldModel, key.ID).Scan(&oldEnabled, &oldActive, &allowlist); err != nil {
		t.Fatal(err)
	}
	if !oldEnabled || oldActive || allowlist != oldModel {
		t.Fatalf("legacy permission/key history changed: enabled=%t active=%t allowlist=%s", oldEnabled, oldActive, allowlist)
	}
	multipliers, err := s.ListModelMultipliers(ctx, append([]string{oldModel}, models...))
	if err != nil {
		t.Fatal(err)
	}
	for index, multiplier := range multipliers {
		want := "1"
		if index == 0 {
			want = "2"
		}
		if !equalAllocationDecimal(multiplier.Multiplier, want) {
			t.Fatalf("multiplier inherited or lost: %+v, want %s", multiplier, want)
		}
	}
	// Changing the retained setting cannot change in-flight billing snapshots.
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_model_multipliers SET multiplier=9 WHERE model=$1`, oldModel); err != nil {
		t.Fatal(err)
	}
	for _, requestID := range requestIDs {
		if err := s.SettleRequest(ctx, requestID, now.Add(3*time.Second)); err != nil {
			t.Fatalf("settle historical snapshot %s: %v", requestID, err)
		}
		var mode, model, amount, multiplier string
		if err := s.db.QueryRowContext(ctx, `SELECT r.billing_mode,r.pricing_model,l.amount_usd::text,l.pricing_multiplier::text
			FROM billing_reservations r JOIN billing_ledger_entries l USING(request_id) WHERE r.request_id=$1`, requestID).
			Scan(&mode, &model, &amount, &multiplier); err != nil {
			t.Fatal(err)
		}
		if mode != BillingModeOpenAIAPIEquivalent || model != oldModel || amount != "1.006400000000" || !equalAllocationDecimal(multiplier, "2") {
			t.Fatalf("historical snapshot changed: %s/%s cost=%s multiplier=%s", mode, model, amount, multiplier)
		}
	}
	if after := readHistory(); before != after {
		t.Fatalf("migration or replay rewrote settled history: before=%s after=%s", before, after)
	}
}
