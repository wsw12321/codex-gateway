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

func TestCPAV830ModelMigrationPostgresIntegration(t *testing.T) {
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
	schema := "cpa_v830_" + strings.ReplaceAll(id, "-", "")
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
		if migration.Name >= "0035" {
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply predecessor %s: %v", migration.Name, err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)`, migration.Name, migration.Checksum[:]); err != nil {
			t.Fatal(err)
		}
	}

	removed := []string{"gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3.6-flash-high", "gemini-3.7-flash-high", "gemini-3.7-flash"}
	added := []string{"claude-haiku-5-5", "gemini-4-argon-high"}
	const unchanged = "gpt-6.1-sol"
	if err := s.SyncModelAccessCatalog(ctx, append(slices.Clone(removed), unchanged)); err != nil {
		t.Fatal(err)
	}
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "cpa-v830")
	disabled := globalUsageIntegrationUser(t, ctx, s, "cpa-v830-disabled", UserRoleMember)
	if err := s.DisableUser(ctx, disabled.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET model_allowlist=ARRAY[$2,$3,$2] WHERE id=$1`, key.ID, removed[0], unchanged); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, model := range []string{unchanged, added[1]} {
		if _, err := s.SetModelMultiplier(ctx, SetModelMultiplierParams{
			BillingWriteParams: billingIntegrationWrite(t, user.ID, "seed v8.0.20 multiplier", now),
			Model:              model, Multiplier: "1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A v8.0.30 rollback must never also restore the old 0025 cutover. Seed
	// exact matches for all three old settings to make accidental stacking
	// observable even though the current rollout does not touch this model.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO cpa_v8_model_config_backup(setting,model,before_value,applied_value)
		SELECT 'default',model,to_jsonb(d)||'{"enabled":false}'::jsonb,to_jsonb(d)
		FROM model_access_defaults d WHERE model=$1
		ON CONFLICT(setting,model,subject_id) DO UPDATE
		SET before_value=EXCLUDED.before_value,applied_value=EXCLUDED.applied_value;
	`, unchanged); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO cpa_v8_model_config_backup(setting,model,subject_id,before_value,applied_value)
		SELECT 'user',model,user_id::text,to_jsonb(a)||'{"enabled":false}'::jsonb,to_jsonb(a)
		FROM user_model_access a WHERE model=$1
		ON CONFLICT(setting,model,subject_id) DO UPDATE
		SET before_value=EXCLUDED.before_value,applied_value=EXCLUDED.applied_value`, unchanged); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO cpa_v8_model_config_backup(setting,model,before_value,applied_value)
		SELECT 'multiplier',model,to_jsonb(m)||'{"multiplier":2.5}'::jsonb,to_jsonb(m)
		FROM billing_model_multipliers m WHERE model=$1
		ON CONFLICT(setting,model,subject_id) DO UPDATE
		SET before_value=EXCLUDED.before_value,applied_value=EXCLUDED.applied_value`, unchanged); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSubscription(ctx, PutSubscriptionParams{
		Name: "Retired model history", ModelIDs: []string{removed[0]},
		BillingWriteParams: billingIntegrationWrite(t, user.ID, "fund retired model history", now),
		UserID:             user.ID, Tier: BillingTierDay, AllowanceUSD: "10",
	}); err != nil {
		t.Fatal(err)
	}
	requestID := "cpa-v830-settled-before-cutover"
	rule := config.CPAV8PricingModels()["gemini-3.8-flash-high"]
	params := billingIntegrationAdmission(user, device, key, requestID, now)
	params.Usage.Model = removed[0]
	params.Usage.PricingRuleVersion = config.PricingSchemaV2
	params.Usage.RequestedServiceTier = "default"
	params.Billing = &BillingReservationParams{
		RequestID: requestID, UserID: user.ID, APIKeyID: key.ID, Model: removed[0],
		PricingRuleVersion: config.PricingSchemaV2, BillingMode: BillingModeGeminiAPIEquivalent,
		PricingCatalogAsOf: "2026-10-08", PricingModel: removed[0],
		PricingSnapshot: billingIntegrationV2Snapshot(t, removed[0], rule),
		CacheWriteMode:  config.CacheWriteIncludedInInput, RequestedServiceTier: "default", Now: now,
	}
	if _, err := s.AdmitRequest(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: requestID, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second),
		ActualModel: removed[0], ActualServiceTier: "default", InputTokens: 100, CachedInputTokens: 20,
		OutputTokens: 37, ReasoningTokens: 7,
	}); err != nil {
		t.Fatal(err)
	}
	readProtected := func() string {
		t.Helper()
		var value string
		if err := s.db.QueryRowContext(ctx, `SELECT jsonb_build_object(
			'key',(SELECT to_jsonb(k) FROM api_keys k WHERE id=$1),
			'account',(SELECT to_jsonb(a) FROM billing_accounts a WHERE user_id=$2),
			'usage',(SELECT to_jsonb(u) FROM usage_requests u WHERE request_id=$3),
			'reservation',(SELECT to_jsonb(r) FROM billing_reservations r WHERE request_id=$3),
			'ledger',(SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM billing_ledger_entries l),
			'subscriptions',(SELECT jsonb_agg(to_jsonb(b) ORDER BY b.id) FROM billing_subscriptions b),
			'entitlements',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM billing_subscription_entitlements e),
			'unchanged_default',(SELECT to_jsonb(d) FROM model_access_defaults d WHERE model=$4),
			'unchanged_users',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.user_id) FROM user_model_access a WHERE model=$4 AND user_id IN ($2,$5)),
			'unchanged_multiplier',(SELECT to_jsonb(m) FROM billing_model_multipliers m WHERE model=$4))::text`,
			key.ID, user.ID, requestID, unchanged, disabled.ID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	protectedBefore := readProtected()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readProtected(); got != protectedBefore {
		t.Fatal("v8.0.30 migration changed keys, usage, settled billing, subscription scope, or unrelated configuration")
	}
	for _, model := range removed {
		if err := s.RequireModelAccess(ctx, user.ID, model); !errors.Is(err, ErrModelAccessUnavailable) {
			t.Fatalf("removed model %s remains available: %v", model, err)
		}
		var enabled bool
		if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM user_model_access WHERE user_id=$1 AND model=$2`, user.ID, model).Scan(&enabled); err != nil || !enabled {
			t.Fatalf("retired permission history changed for %s: enabled=%t err=%v", model, enabled, err)
		}
	}
	for _, model := range added {
		for _, principal := range []User{user, disabled} {
			if err := s.RequireModelAccess(ctx, principal.ID, model); err != nil {
				t.Fatalf("new model %s not initialized for %s: %v", model, principal.ID, err)
			}
		}
	}
	catalog, err := s.ListModelAccessModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, model := range catalog {
		models = append(models, model.Model)
	}
	want := append(slices.Clone(added), unchanged)
	slices.Sort(want)
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("v8.0.30 active catalog = %v, want %v", models, want)
	}
	history, err := s.ListUsageModels(ctx, user.ID)
	if err != nil || !slices.Contains(history, removed[0]) {
		t.Fatalf("retired history no longer readable: %v %v", history, err)
	}
	var snapshotMultiplier string
	if err := s.db.QueryRowContext(ctx, `SELECT applied_value->>'multiplier' FROM cpa_v830_model_config_backup
		WHERE setting='multiplier' AND model=$1`, added[0]).Scan(&snapshotMultiplier); err != nil || snapshotMultiplier != "1" {
		t.Fatalf("missing default multiplier snapshot: %q %v", snapshotMultiplier, err)
	}
	future := globalUsageIntegrationUser(t, ctx, s, "cpa-v830-future", UserRoleMember)
	for _, model := range added {
		if err := s.RequireModelAccess(ctx, future.ID, model); err != nil {
			t.Fatalf("new user did not inherit %s: %v", model, err)
		}
	}
	// Later owner changes survive both migration replay and conditional rollback.
	if _, err := s.db.ExecContext(ctx, `UPDATE model_access_defaults SET enabled=false,updated_at=now()+interval '1 second' WHERE model=$1`, added[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE user_model_access SET enabled=false,updated_at=now()+interval '1 second' WHERE model=$1 AND user_id=$2`, added[1], user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_model_multipliers SET multiplier=3,updated_at=now()+interval '1 second' WHERE model=$1`, added[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireModelAccess(ctx, user.ID, added[1]); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("migration replay overwrote later user choice: %v", err)
	}
	rollback, err := os.ReadFile("../../scripts/rollback-cpa-model-config.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(rollback)); err != nil {
		t.Fatalf("conditional v8.0.30 rollback: %v", err)
	}
	if got := readProtected(); got != protectedBefore {
		t.Fatal("v8.0.30 rollback changed protected history or also applied the old 0025 rollback")
	}
	for _, model := range removed {
		if err := s.RequireModelAccess(ctx, user.ID, model); err != nil {
			t.Fatalf("rollback did not restore retired catalog entry %s: %v", model, err)
		}
	}
	if err := s.RequireModelAccess(ctx, user.ID, added[0]); !errors.Is(err, ErrModelAccessUnavailable) {
		t.Fatalf("rollback retained unchanged new catalog entry: %v", err)
	}
	if err := s.RequireModelAccess(ctx, user.ID, added[1]); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("rollback overwrote later permission choice: %v", err)
	}
	var futureEnabled bool
	if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM user_model_access WHERE user_id=$1 AND model=$2`, future.ID, added[0]).Scan(&futureEnabled); err != nil || !futureEnabled {
		t.Fatalf("rollback changed a post-cutover user: enabled=%t err=%v", futureEnabled, err)
	}
	multipliers, err := s.ListModelMultipliers(ctx, []string{unchanged, added[0], added[1]})
	if err != nil {
		t.Fatal(err)
	}
	for _, multiplier := range multipliers {
		want := "1"
		if multiplier.Model == added[1] {
			want = "3"
		}
		if !equalAllocationDecimal(multiplier.Multiplier, want) {
			t.Fatalf("rollback changed multiplier %s: got %s want %s", multiplier.Model, multiplier.Multiplier, want)
		}
	}
}
