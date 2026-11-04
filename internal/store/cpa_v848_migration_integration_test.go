//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
)

func TestCPAV848ModelMigrationPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connection, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*connection)
	defer admin.Close()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "cpa_v848_" + strings.ReplaceAll(id, "-", "")
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
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		name text PRIMARY KEY, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Name >= "0038" {
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply predecessor %s: %v", migration.Name, err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)`, migration.Name, migration.Checksum[:]); err != nil {
			t.Fatal(err)
		}
	}

	const retired = "gpt-5.5"
	const unchanged = "gpt-6.1-sol"
	if !slices.Contains(config.RetiredCodexModels(), retired) {
		t.Fatalf("%s is not in the retired Codex model list", retired)
	}
	if err := s.SyncModelAccessCatalog(ctx, []string{retired, unchanged}); err != nil {
		t.Fatal(err)
	}
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "cpa-v848")
	disabled := globalUsageIntegrationUser(t, ctx, s, "cpa-v848-disabled", UserRoleMember)
	if err := s.DisableUser(ctx, disabled.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET model_allowlist=ARRAY[$2,$3] WHERE id=$1`, key.ID, retired, unchanged); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "seed v8.0.30 multiplier", now),
		Model:              retired, Multiplier: "2",
	}); err != nil {
		t.Fatal(err)
	}
	// A v8.0.48 rollback must never also restore the 0035 cutover. Seed an exact
	// match for an unrelated model so accidental stacking becomes observable.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO cpa_v830_model_config_backup(setting,model,before_value,applied_value)
		SELECT 'default',model,to_jsonb(d)||'{"enabled":false}'::jsonb,to_jsonb(d)
		FROM model_access_defaults d WHERE model=$1
		ON CONFLICT(setting,model,subject_id) DO UPDATE
		SET before_value=EXCLUDED.before_value,applied_value=EXCLUDED.applied_value`, unchanged); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSubscription(ctx, PutSubscriptionParams{
		Name: "Retired Codex model history", ModelIDs: []string{retired},
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "fund retired Codex history", now),
		UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "10",
	}); err != nil {
		t.Fatal(err)
	}
	pricingRaw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(string(pricingRaw))
	if err != nil {
		t.Fatal(err)
	}
	rule := pricing.Models["gpt-5.4"]
	requestID := "cpa-v848-settled-before-cutover"
	billingIntegrationFundingLayerSchema(t, ctx, s)
	params := billingIntegrationAdmission(user, device, key, requestID, now)
	params.Usage.Model, params.Billing.Model = retired, retired
	params.Usage.PricingRuleVersion, params.Billing.PricingRuleVersion = config.PricingSchemaV2, config.PricingSchemaV2
	params.Usage.RequestedServiceTier, params.Billing.RequestedServiceTier = "default", "default"
	params.Billing.InputUSDPerMillion, params.Billing.CachedInputUSDPerMillion, params.Billing.OutputUSDPerMillion = "", "", ""
	params.Billing.PricingModel, params.Billing.PricingCatalogAsOf = retired, "2026-10-17"
	params.Billing.CacheWriteMode, params.Billing.BillingMode = rule.CacheWriteMode, BillingModeOpenAIAPIEquivalent
	params.Billing.PricingSnapshot = billingIntegrationV2Snapshot(t, retired, rule)
	if _, err := s.AdmitRequest(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: requestID, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second),
		ActualModel: retired, ActualServiceTier: "default", InputTokens: 100, CachedInputTokens: 20,
		OutputTokens: 37, ReasoningTokens: 7,
	}); err != nil {
		t.Fatal(err)
	}
	readProtected := func() map[string]string {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, `SELECT key, value::text FROM jsonb_each(jsonb_build_object(
			'key',(SELECT to_jsonb(k) FROM api_keys k WHERE id=$1),
			'account',(SELECT to_jsonb(a) FROM billing_accounts a WHERE user_id=$2),
			'usage',(SELECT to_jsonb(u) FROM usage_requests u WHERE request_id=$3),
			'reservation',(SELECT to_jsonb(r) FROM billing_reservations r WHERE request_id=$3),
			'ledger',(SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM billing_ledger_entries l),
			'subscriptions',(SELECT jsonb_agg(to_jsonb(b) ORDER BY b.id) FROM billing_subscriptions b),
			'entitlements',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM billing_subscription_entitlements e),
			'retired_multiplier',(SELECT to_jsonb(m) FROM billing_model_multipliers m WHERE model=$4),
			'unchanged_default',(SELECT to_jsonb(d) FROM model_access_defaults d WHERE model=$5),
			'unchanged_users',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.user_id) FROM user_model_access a WHERE model=$5 AND user_id IN ($2,$6))))`,
			key.ID, user.ID, requestID, retired, unchanged, disabled.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		values := map[string]string{}
		for rows.Next() {
			var name, value string
			if err := rows.Scan(&name, &value); err != nil {
				t.Fatal(err)
			}
			values[name] = value
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return values
	}
	assertProtected := func(stage string, want, got map[string]string, ignored ...string) {
		t.Helper()
		for name, value := range want {
			if !slices.Contains(ignored, name) && got[name] != value {
				t.Fatalf("%s changed protected %s", stage, name)
			}
		}
	}
	protectedBefore := readProtected()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The cutover itself must not touch keys, usage, billing, scopes,
	// multipliers or the unrelated model's configuration.
	assertProtected("v8.0.48 migration", protectedBefore, readProtected())
	if err := s.RequireModelAccess(ctx, user.ID, retired); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("retired model remains available: %v", err)
	}
	for _, principal := range []User{user, disabled} {
		var enabled bool
		if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM user_model_access WHERE user_id=$1 AND model=$2`, principal.ID, retired).Scan(&enabled); err != nil || !enabled {
			t.Fatalf("retired permission history changed for %s: enabled=%t err=%v", principal.ID, enabled, err)
		}
	}
	if err := s.RequireModelAccess(ctx, user.ID, unchanged); err != nil {
		t.Fatalf("unrelated model changed: %v", err)
	}
	catalog, err := s.ListModelAccessModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, model := range catalog {
		models = append(models, model.Model)
	}
	if !reflect.DeepEqual(models, []string{unchanged}) {
		t.Fatalf("v8.0.48 active catalog = %v, want [%s]", models, unchanged)
	}
	history, err := s.ListUsageModels(ctx, user.ID)
	if err != nil || !slices.Contains(history, retired) {
		t.Fatalf("retired history no longer readable: %v %v", history, err)
	}
	var beforeActive, appliedActive bool
	if err := s.db.QueryRowContext(ctx, `SELECT (before_value->>'catalog_active')::boolean,(applied_value->>'catalog_active')::boolean
		FROM cpa_v848_model_config_backup WHERE setting='default' AND model=$1`, retired).Scan(&beforeActive, &appliedActive); err != nil || !beforeActive || appliedActive {
		t.Fatalf("default snapshot = before %t applied %t, err %v", beforeActive, appliedActive, err)
	}
	var snapshotMultiplier string
	if err := s.db.QueryRowContext(ctx, `SELECT applied_value->>'multiplier' FROM cpa_v848_model_config_backup
		WHERE setting='multiplier' AND model=$1`, retired).Scan(&snapshotMultiplier); err != nil || !equalAllocationDecimal(snapshotMultiplier, "2") {
		t.Fatalf("missing multiplier snapshot: %q %v", snapshotMultiplier, err)
	}
	// A later owner choice survives migration replay and conditional rollback.
	if _, err := s.db.ExecContext(ctx, `UPDATE user_model_access SET enabled=false,updated_at=now()+interval '1 second' WHERE model=$1 AND user_id=$2`, retired, disabled.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rollback, err := os.ReadFile("../../scripts/rollback-cpa-model-config.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(rollback)); err != nil {
		t.Fatalf("conditional v8.0.48 rollback: %v", err)
	}
	// The conditional rollback rewrites unchanged multiplier rows with their
	// before value (refreshing updated_at), exactly like the 0035 rollback.
	assertProtected("v8.0.48 rollback (or a stacked 0035 rollback)", protectedBefore, readProtected(), "retired_multiplier")
	multipliers, err := s.ListModelMultipliers(ctx, []string{retired})
	if err != nil || len(multipliers) != 1 || !equalAllocationDecimal(multipliers[0].Multiplier, "2") {
		t.Fatalf("rollback changed the retired multiplier: %+v %v", multipliers, err)
	}
	if err := s.RequireModelAccess(ctx, user.ID, retired); err != nil {
		t.Fatalf("rollback did not restore the retired catalog entry: %v", err)
	}
	var laterChoice bool
	if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM user_model_access WHERE user_id=$1 AND model=$2`, disabled.ID, retired).Scan(&laterChoice); err != nil || laterChoice {
		t.Fatalf("rollback overwrote a post-cutover permission choice: enabled=%t err=%v", laterChoice, err)
	}
}
