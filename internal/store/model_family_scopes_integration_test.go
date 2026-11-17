//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestModelFamilyScopesFundLaterModelsPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	if err := s.SyncModelAccessCatalog(ctx, []string{"gpt-old", "claude-old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingModelScopes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "model-families")
	billingIntegrationFundingAccount(t, ctx, s)
	write := func(reason string) BillingWriteParams { return billingIntegrationWrite(t, user.ID, reason, now) }
	reserve := func(id, model string) (BillingReservation, error) {
		return s.ReserveBilling(ctx, BillingReservationParams{
			RequestID: id, UserID: user.ID, APIKeyID: key.ID, Model: model,
			InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: now,
		})
	}

	for _, invalid := range [][]string{{"family:xai"}, {"family:"}, {"gpt-missing", "family:gpt"}} {
		if _, err := s.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: write("invalid family scope"), UserID: user.ID,
			Name: "Invalid", Tier: "day", AllowanceUSD: "1", ModelIDs: invalid,
		}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("scope %v accepted: %v", invalid, err)
		}
	}
	subscription, err := s.PutSubscription(ctx, PutSubscriptionParams{
		BillingWriteParams: write("GPT family subscription"), UserID: user.ID,
		Name: "GPT 全系", Tier: "day", AllowanceUSD: "1", ModelIDs: []string{"family:gpt"},
	})
	if err != nil || !reflect.DeepEqual(subscription.ModelIDs, []string{"family:gpt"}) {
		t.Fatalf("family subscription: %+v %v", subscription, err)
	}
	if reservation, err := reserve("family-scope-gpt-old", "gpt-old"); err != nil || len(reservation.OrderedFundingSources) != 1 ||
		reservation.OrderedFundingSources[0].SubscriptionID != subscription.ID {
		t.Fatalf("family scope did not fund a catalog model: %+v %v", reservation, err)
	}

	// A model priced after the scope was saved is covered without an edit.
	if err := s.SyncModelAccessCatalog(ctx, []string{"gpt-old", "gpt-new", "claude-old"}); err != nil {
		t.Fatal(err)
	}
	later, err := reserve("family-scope-gpt-new", "gpt-new")
	if err != nil || len(later.OrderedFundingSources) != 1 || later.OrderedFundingSources[0].SubscriptionID != subscription.ID {
		t.Fatalf("family scope did not fund a later model: %+v %v", later, err)
	}
	var insufficient *InsufficientFundsError
	if _, err := reserve("family-scope-claude-old", "claude-old"); !errors.As(err, &insufficient) || insufficient.RetryAfter != 0 {
		t.Fatalf("GPT family funded a Claude model: %v", err)
	}

	// Exhausting the period leaves only the renewal, which the retry hint finds
	// through the family token as well.
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{
		RequestID: later.RequestID, UserID: user.ID, DeviceID: device.ID, APIKeyID: key.ID,
		Model: "gpt-new", Endpoint: "responses", RequestedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
		RequestID: later.RequestID, State: "completed", HTTPStatus: 200, CompletedAt: now.Add(time.Second),
		InputTokens: 1_000_000, ActualModel: "gpt-new",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleBilling(ctx, later.RequestID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reserve("family-scope-gpt-exhausted", "gpt-new"); !errors.As(err, &insufficient) ||
		insufficient.RetryAfter <= 0 || insufficient.RetryAfter > 24*time.Hour {
		t.Fatalf("family renewal hint: %v", err)
	}

	// Group scopes select allocation groups by family too.
	group, err := s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: write("Claude family group"), Name: "Claude family", LimitUSD: "2", Period: "day",
		ModelIDs: []string{"family:claude"},
	})
	if err != nil || !reflect.DeepEqual(group.ModelIDs, []string{"family:claude"}) {
		t.Fatalf("family group: %+v %v", group, err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{
		BillingWriteParams: write("family membership"), GroupID: group.ID, Action: "add", UserIDs: []string{user.ID},
	}); err != nil {
		t.Fatal(err)
	}
	hasGroup := func(provider, model string) bool {
		t.Helper()
		groups, err := s.ListUserUpstreamAllocationGroups(ctx, user.ID, provider, model)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range groups {
			if candidate.GroupID == group.ID {
				return true
			}
		}
		return false
	}
	if !hasGroup(UpstreamProviderAnthropic, "claude-old") || !hasGroup(UpstreamProviderAnthropic, "claude-priced-later") {
		t.Fatal("Claude family group missing from Claude allocation")
	}
	if hasGroup(UpstreamProviderCodex, "gpt-old") {
		t.Fatal("Claude family group offered for a GPT model")
	}

	// An omitted group scope means every model, now and later.
	defaulted, err := s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: write("default group"), Name: "Defaulted", LimitUSD: "1", Period: "day",
	})
	if err != nil || !reflect.DeepEqual(defaulted.ModelIDs, []string{"family:claude", "family:gpt"}) {
		t.Fatalf("defaulted group scope: %+v %v", defaulted, err)
	}

	settings, err := s.SetBillingCashModels(ctx, SetBillingCashModelsParams{
		BillingWriteParams: write("cash family"), ModelIDs: []string{"family:claude", "gpt-old"},
	})
	if err != nil || !reflect.DeepEqual(settings.ModelIDs, []string{"family:claude", "gpt-old"}) {
		t.Fatalf("cash family scope: %+v %v", settings, err)
	}
	if _, err := s.SetBillingCashModels(ctx, SetBillingCashModelsParams{
		BillingWriteParams: write("cash unknown family"), ModelIDs: []string{"family:meta"},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown cash family accepted: %v", err)
	}
}

func TestModelFamilyMigrationConvertsCoveredScopesPostgresIntegration(t *testing.T) {
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
	schema := "model_family_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop model family migration schema: %v", err)
		}
	}()
	configuration.RuntimeParams["search_path"] = schema
	s := New(stdlib.OpenDB(*configuration))
	defer s.Close()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var upgrade Migration
	for _, migration := range migrations {
		if migration.Name == "0049_model_family_scopes.sql" {
			upgrade = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	if upgrade.Name == "" {
		t.Fatal("model family migration missing")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	if err := s.SyncModelAccessCatalog(ctx, []string{"claude-a", "gpt-a", "gpt-b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBillingModelScopes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	user, _, _ := billingIntegrationPrincipal(t, ctx, s, "family-migration-"+id)
	write := func(reason string) BillingWriteParams { return billingIntegrationWrite(t, user.ID, reason, now) }
	subscription := func(name string, models []string) BillingSubscriptionState {
		t.Helper()
		value, err := s.PutSubscription(ctx, PutSubscriptionParams{
			BillingWriteParams: write(name), UserID: user.ID, Name: name, Tier: "day", AllowanceUSD: "1", ModelIDs: models,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	everything := subscription("everything", []string{"claude-a", "gpt-a", "gpt-b"})
	partial := subscription("partial", []string{"gpt-a"})
	fullPlan, err := s.CreateBillingPlan(ctx, CreateBillingPlanParams{
		BillingWriteParams: write("full GPT plan"), Name: "GPT", PriceUSD: "1", Tier: "day", AllowanceUSD: "1",
		MinPeriodCount: 1, Active: true, ModelIDs: []string{"gpt-a", "gpt-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	partialPlan, err := s.CreateBillingPlan(ctx, CreateBillingPlanParams{
		BillingWriteParams: write("partial plan"), Name: "Partial", PriceUSD: "1", Tier: "day", AllowanceUSD: "1",
		MinPeriodCount: 1, Active: true, ModelIDs: []string{"gpt-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: write("claude group"), Name: "Claude", LimitUSD: "1", Period: "day", ModelIDs: []string{"claude-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBillingCashModels(ctx, SetBillingCashModelsParams{
		BillingWriteParams: write("partial cash"), ModelIDs: []string{"claude-a", "gpt-b"},
	}); err != nil {
		t.Fatal(err)
	}
	configVersion := func(id string) int64 {
		var value int64
		if err := s.db.QueryRowContext(ctx, `SELECT config_version FROM billing_subscriptions WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	everythingVersion, partialVersion := configVersion(everything.ID), configVersion(partial.ID)

	if _, err := s.db.ExecContext(ctx, upgrade.SQL); err != nil {
		t.Fatalf("apply %s: %v", upgrade.Name, err)
	}

	scope := func(query, id string) []string {
		t.Helper()
		var values []string
		var raw []byte
		if err := s.db.QueryRowContext(ctx, query, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &values); err != nil {
			t.Fatal(err)
		}
		return values
	}
	const subscriptionScope = `SELECT model_ids FROM billing_subscriptions WHERE id=$1`
	const entitlementScope = `SELECT model_ids FROM billing_subscription_entitlements WHERE subscription_id=$1 AND superseded_at IS NULL`
	const planScope = `SELECT model_ids FROM billing_plans WHERE id=$1`
	const groupScope = `SELECT model_ids FROM user_groups WHERE id=$1`
	converted := []string{"claude-a", "family:claude", "family:gpt", "gpt-a", "gpt-b"}
	if got := scope(subscriptionScope, everything.ID); !reflect.DeepEqual(got, converted) {
		t.Fatalf("covered subscription = %v", got)
	}
	if got := scope(entitlementScope, everything.ID); !reflect.DeepEqual(got, converted) {
		t.Fatalf("covered entitlement = %v", got)
	}
	if configVersion(everything.ID) == everythingVersion {
		t.Fatal("converted subscription kept its renewal configuration version")
	}
	if got := scope(subscriptionScope, partial.ID); !reflect.DeepEqual(got, []string{"gpt-a"}) || configVersion(partial.ID) != partialVersion {
		t.Fatalf("partial subscription changed: %v", got)
	}
	if got := scope(entitlementScope, partial.ID); !reflect.DeepEqual(got, []string{"gpt-a"}) {
		t.Fatalf("partial entitlement changed: %v", got)
	}
	plans, err := s.ListBillingPlans(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range plans {
		switch plan.ID {
		case fullPlan.ID:
			if !reflect.DeepEqual(plan.ModelIDs, []string{"family:gpt", "gpt-a", "gpt-b"}) || plan.Version != fullPlan.Version+1 {
				t.Fatalf("covered plan = %+v", plan)
			}
		case partialPlan.ID:
			if !reflect.DeepEqual(plan.ModelIDs, []string{"gpt-b"}) || plan.Version != partialPlan.Version {
				t.Fatalf("partial plan = %+v", plan)
			}
		}
	}
	if got := scope(groupScope, group.ID); !reflect.DeepEqual(got, []string{"claude-a", "family:claude"}) {
		t.Fatalf("covered group = %v", got)
	}
	settings, err := s.GetBillingSettings(ctx)
	if err != nil || !reflect.DeepEqual(settings.ModelIDs, []string{"claude-a", "family:claude", "gpt-b"}) {
		t.Fatalf("cash scope = %+v %v", settings, err)
	}
	var metadata string
	if err := s.db.QueryRowContext(ctx, `SELECT metadata::text FROM audit_events WHERE event_type='billing.model_families_converted'`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rows": 7`, `"plans": {"gpt": 1}`, `"groups": {"claude": 1}`, `"cash": {"claude": 1}`, `"subscriptions": {"gpt": 1, "claude": 1}`} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("audit metadata %s lacks %s", metadata, want)
		}
	}
}
