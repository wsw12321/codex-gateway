//go:build integration

package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestGroupPriorityMigrationPreservesLegacyPostgresIntegration(t *testing.T) {
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
	schema := "group_priority_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop group billing migration schema: %v", err)
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
		if migration.Name == "0027_group_priority_billing.sql" {
			upgrade = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply legacy %s: %v", migration.Name, err)
		}
	}
	if upgrade.Name == "" {
		t.Fatal("group billing migration missing")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	u, d, k := billingIntegrationPrincipal(t, ctx, s, "priority-upgrade-"+id)
	groupID, _ := newUUID()
	groupPeriodID, _ := newUUID()
	subscriptionID, _ := newUUID()
	subscriptionPeriodID, _ := newUUID()
	start, end := now.Add(-time.Hour), now.Add(23*time.Hour)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_groups(id,name,limit_usd,period,starts_at,created_at,updated_at)
		VALUES($1,'Legacy group',100,'day',$2,$2,$2)`, groupID, start); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO group_usage_periods(id,group_id,starts_at,ends_at,limit_usd,used_usd,created_at)
		VALUES($1,$2,$3,$4,100,10,$3)`, groupPeriodID, groupID, start, end); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE user_groups SET current_period_id=$2 WHERE id=$1`, groupID, groupPeriodID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts SET group_id=$2 WHERE user_id=$1`, u.ID, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscriptions
		(id,user_id,tier,enabled,allowance_usd,period_count,current_period_number,expires_at,created_at,updated_at)
		VALUES($1,$2,'day',true,100,1,1,$3,$4,$4)`, subscriptionID, u.ID, end, start); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscription_periods
		(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,remaining_usd,period_number,period_count,created_at)
		VALUES($1,$2,$3,'day',$4,$5,100,99,1,1,$4)`, subscriptionPeriodID, subscriptionID, u.ID, start, end); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET current_period_id=$2 WHERE id=$1`, subscriptionID, subscriptionPeriodID); err != nil {
		t.Fatal(err)
	}
	historicalID, inflightID := "priority-upgrade-history-"+id, "priority-upgrade-inflight-"+id
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_reservations
		(request_id,user_id,api_key_id,requested_model,input_usd_per_million,cached_input_usd_per_million,output_usd_per_million,
		 day_period_id,group_id,group_period_id,created_at,state,actual_input_tokens,actual_cached_input_tokens,actual_output_tokens,
		 actual_cost_usd,charged_usd,uncovered_usd,settled_at)
		VALUES($1,$2,$3,'billing-priced-model',1,0,0,$4,$5,$6,$7,'settled',1000000,0,0,1,1,0,$7)`,
		historicalID, u.ID, k.ID, subscriptionPeriodID, groupID, groupPeriodID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries
		(user_id,entry_type,request_id,actual_cost_usd,charged_usd,uncovered_usd,group_id,group_period_id,created_at)
		VALUES($1,'usage_charge',$2,1,1,0,$3,$4,$5)`, u.ID, historicalID, groupID, groupPeriodID, now); err != nil {
		t.Fatal(err)
	}
	// Old schemas allowed sparse administrative/imported usage ledger records.
	// Preserve them without assigning a fictional covered payment on upgrade.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries(user_id,entry_type,request_id)
		VALUES($1,'usage_charge',$2)`, u.ID, "priority-upgrade-sparse-"+id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_reservations
		(request_id,user_id,api_key_id,requested_model,input_usd_per_million,cached_input_usd_per_million,
		 output_usd_per_million,day_period_id,group_id,group_period_id,created_at)
		VALUES($1,$2,$3,'billing-priced-model',1,0,0,$4,$5,$6,$7)`, inflightID, u.ID, k.ID, subscriptionPeriodID, groupID, groupPeriodID, now); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, upgrade.SQL); err != nil {
		_ = tx.Rollback()
		t.Fatalf("apply group priority upgrade: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetGroup(ctx, groupID)
	if err != nil || g.PeriodID != groupPeriodID || g.UsedUSD != "10.000000000000" || !g.PeriodStartsAt.Equal(start) || !g.PeriodEndsAt.Equal(end) ||
		g.MemberLimitUSD != nil || len(g.Members) != 1 || g.Members[0].UsedUSD != "0.000000000000" {
		t.Fatalf("upgrade changed current period or backfilled members = %+v %v", g, err)
	}
	state, err := s.GetBillingState(ctx, u.ID, 20, 0)
	if err != nil || state.Subscriptions[0].RemainingUSD != "99.000000000000" {
		t.Fatalf("upgrade refunded historic personal payments = %+v %v", state, err)
	}
	historical, err := scanBillingReservation(s.db.QueryRowContext(ctx, `SELECT `+billingReservationColumns+` FROM billing_reservations WHERE request_id=$1`, historicalID))
	if err != nil || historical.FundingRuleVersion != 1 {
		t.Fatalf("historical version = %+v %v", historical, err)
	}
	groupBillingAssertSplit(t, historical, "0", "1", "0")
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_ledger_entries SET reason='change' WHERE request_id=$1`, historicalID); err == nil {
		t.Fatal("upgrade left ledger mutable")
	}
	billingIntegrationComplete(t, ctx, s, u, d, k, inflightID, now, 20000000, "billing-priced-model")
	settled, err := s.SettleBilling(ctx, inflightID, now.Add(time.Second))
	if err != nil || settled.FundingRuleVersion != 1 {
		t.Fatalf("legacy in-flight settlement = %+v %v", settled, err)
	}
	groupBillingAssertSplit(t, settled, "0", "20", "0")
	billingIntegrationAssertAllocations(t, ctx, s, inflightID, []string{"day"}, []string{"20.000000000000"})
	g, err = s.GetGroup(ctx, groupID)
	if err != nil || g.UsedUSD != "30.000000000000" || g.Members[0].UsedUSD != "0.000000000000" {
		t.Fatalf("legacy in-flight must add full group cost without member accounting = %+v %v", g, err)
	}
	// Legacy preservation and recovery were verified against the upgraded
	// funding schema above. Current admissions also require the independent
	// durable model-price schema, even when no override has been configured.
	for _, migration := range migrations {
		if migration.Name == "0029_model_prices.sql" {
			if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
				t.Fatalf("apply model-price helper schema: %v", err)
			}
		}
	}
	newID := "priority-upgrade-new-" + id
	newReservation := billingIntegrationReserveAndComplete(t, ctx, s, u, d, k, newID, now, 5000000, "1", "billing-priced-model")
	if newReservation.FundingRuleVersion != 2 {
		t.Fatal("new admission did not adopt group priority")
	}
	settled, err = s.SettleBilling(ctx, newID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	groupBillingAssertSplit(t, settled, "5", "0", "0")
	g, err = s.GetGroup(ctx, groupID)
	if err != nil || g.UsedUSD != "35.000000000000" || g.Members[0].UsedUSD != "5.000000000000" {
		t.Fatalf("new-rule counters = %+v %v", g, err)
	}
	state, err = s.GetBillingState(ctx, u.ID, 20, 0)
	if err != nil || state.Subscriptions[0].RemainingUSD != "79.000000000000" {
		t.Fatalf("group-first new request debited personal subscription = %+v %v", state, err)
	}
}
