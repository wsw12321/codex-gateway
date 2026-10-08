//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
)

func TestCPAV8ModelMigrationPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connection, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*connection)
	defer admin.Close()
	id, _ := newUUID()
	schema := "cpa_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop migration schema: %v", err)
		}
	}()
	connection.RuntimeParams["search_path"] = schema
	s := New(stdlib.OpenDB(*connection))
	defer s.Close()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		name text PRIMARY KEY, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Name >= "0025" {
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply predecessor %s: %v", migration.Name, err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)`, migration.Name, migration.Checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	// Install the independent funding and model-price schemas needed by current helpers without
	// applying the CPA catalog migration being tested until after seeding.
	for _, migration := range migrations {
		if migration.Name != "0027_group_priority_billing.sql" && migration.Name != "0029_model_prices.sql" && migration.Name != "0033_anthropic_billing.sql" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply billing helper schema: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)`, migration.Name, migration.Checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	const unchangedModel = "gpt-5.4"
	const retiredModel = "gemini-3.1-pro-high"
	models := append(config.AntigravityModels(), "gpt-6.1-sol", unchangedModel, retiredModel)
	if err := s.SyncModelAccessCatalog(ctx, models); err != nil {
		t.Fatal(err)
	}
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "cpa-migration")
	disabled := globalUsageIntegrationUser(t, ctx, s, "cpa-migration-disabled", UserRoleMember)
	if err := s.DisableUser(ctx, disabled.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE model_access_defaults SET enabled=false;
		UPDATE user_model_access SET enabled=false`); err != nil {
		t.Fatal(err)
	}
	// Keep the exact restricted list, including an old-only model and duplicates.
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET model_allowlist=ARRAY[$2,$3,$2] WHERE id=$1`, key.ID, retiredModel, unchangedModel); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, model := range []string{unchangedModel, "gemini-3.8-flash-high", "gpt-6.1-sol"} {
		if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{
			BillingWriteParams: billingIntegrationWrite(t, user.ID, "seed previous multiplier", now),
			Model:              model, Multiplier: "2.5",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutSubscription(ctx, PutSubscriptionParams{
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "fund migration history", now),
		UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "10",
	}); err != nil {
		t.Fatal(err)
	}
	requestID := "cpa-migration-historical-reservation"
	params := billingIntegrationAdmission(user, device, key, requestID, now)
	params.Usage.Model = retiredModel
	params.Usage.PricingRuleVersion = config.PricingSchemaV2
	params.Usage.RequestedServiceTier = "default"
	rule := config.CPAV8PricingModels()["gemini-pro-agent"]
	params.Billing = &BillingReservationParams{
		RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: retiredModel,
		PricingRuleVersion: config.PricingSchemaV2, BillingMode: BillingModeGeminiAPIEquivalent,
		PricingCatalogAsOf: "2026-09-28", PricingModel: retiredModel,
		PricingSnapshot: billingIntegrationV2Snapshot(t, retiredModel, rule),
		CacheWriteMode:  config.CacheWriteIncludedInInput, RequestedServiceTier: "default", Now: now,
	}
	if _, err := s.AdmitRequest(ctx, params); err != nil {
		t.Fatal(err)
	}
	readProtected := func() string {
		t.Helper()
		var data string
		if err := s.db.QueryRowContext(ctx, `SELECT jsonb_build_object(
			'key', (SELECT to_jsonb(k) FROM api_keys k WHERE id=$1),
			'balance', (SELECT to_jsonb(b) FROM billing_accounts b WHERE user_id=$2),
			'reservation', (SELECT to_jsonb(r) FROM billing_reservations r WHERE request_id=$3),
			'ledger', (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM billing_ledger_entries l),
			'other_default', (SELECT to_jsonb(d) FROM model_access_defaults d WHERE model=$4),
			'other_users', (SELECT jsonb_agg(to_jsonb(a) ORDER BY a.user_id) FROM user_model_access a WHERE model=$4),
			'other_multiplier', (SELECT to_jsonb(m) FROM billing_model_multipliers m WHERE model=$4))::text`,
			key.ID, user.ID, requestID, unchangedModel).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	protectedBefore := readProtected()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if after := readProtected(); after != protectedBefore {
		t.Fatal("migration changed restricted key, other Codex settings, balance, ledger or admission snapshot")
	}
	// Catalog synchronization must receive the complete pricing catalog, not
	// just Gemini. Retired names stay in history but leave current availability.
	fullCatalog := append(config.AntigravityModels(), "gpt-6.1-sol", unchangedModel)
	if err := s.SyncModelAccessCatalog(ctx, fullCatalog); err != nil {
		t.Fatal(err)
	}
	for model := range config.CPAV8PricingModels() {
		for _, principal := range []User{user, disabled} {
			if err := s.RequireModelAccess(ctx, principal.ID, model); err != nil {
				t.Fatalf("target model %s user %s not granted: %v", model, principal.ID, err)
			}
		}
	}
	if err := s.RequireModelAccess(ctx, user.ID, retiredModel); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("retired model remains active: %v", err)
	}
	if err := s.RequireModelAccess(ctx, user.ID, unchangedModel); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("other Codex permission changed: %v", err)
	}
	future := globalUsageIntegrationUser(t, ctx, s, "cpa-migration-future", UserRoleMember)
	for model := range config.CPAV8PricingModels() {
		if err := s.RequireModelAccess(ctx, future.ID, model); err != nil {
			t.Fatalf("future user did not inherit target default %s: %v", model, err)
		}
	}
	multipliers, err := s.ListModelMultipliers(ctx, []string{"gpt-6.1-sol", "gemini-3.8-flash-high", unchangedModel})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range multipliers {
		want := "1"
		if value.Model == unchangedModel {
			want = "2.5"
		}
		if !equalAllocationDecimal(value.Multiplier, want) {
			t.Fatalf("wrong multiplier after migration: %+v", value)
		}
	}
	// Simulate later Owner changes, then restart. Migration must not run twice.
	if _, err := s.db.ExecContext(ctx, `UPDATE model_access_defaults SET enabled=false,updated_at=now()+interval '1 second' WHERE model='gpt-6.1-sol';
		UPDATE user_model_access SET enabled=false,updated_at=now()+interval '1 second' WHERE model='gpt-6.1-sol';
		UPDATE billing_model_multipliers SET multiplier=3,updated_at=now()+interval '1 second' WHERE model='gpt-6.1-sol'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncModelAccessCatalog(ctx, fullCatalog); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireModelAccess(ctx, user.ID, "gpt-6.1-sol"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("restart overwrote a later permission change: %v", err)
	}
	rollback, err := os.ReadFile("../../scripts/rollback-cpa-model-config.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(rollback)); err != nil {
		t.Fatalf("conditional configuration rollback: %v", err)
	}
	if err := s.RequireModelAccess(ctx, user.ID, "gemini-3.8-flash-high"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("rollback did not restore previous target permission: %v", err)
	}
	multipliers, err = s.ListModelMultipliers(ctx, []string{"gpt-6.1-sol", "gemini-3.8-flash-high"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range multipliers {
		want := "2.5"
		if value.Model == "gpt-6.1-sol" {
			want = "3"
		}
		if !equalAllocationDecimal(value.Multiplier, want) {
			t.Fatalf("rollback overwrote changed setting or lost unchanged previous value: %+v", value)
		}
	}
	// Verify a preserved admission snapshot can still be decoded after rollback.
	var snapshot []byte
	if err := s.db.QueryRowContext(ctx, `SELECT pricing_snapshot FROM billing_reservations WHERE request_id=$1`, requestID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var saved config.PricingSnapshot
	if err := json.Unmarshal(snapshot, &saved); err != nil || saved.Model != retiredModel {
		t.Fatalf("historical pricing snapshot changed: %v", err)
	}
}
