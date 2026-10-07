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

func TestGroupPeriodLimitsMigrationPreservesSnapshotsPostgresIntegration(t *testing.T) {
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
	schema := "group_term_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop group term migration schema: %v", err)
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
		if migration.Name == "0031_group_period_limits.sql" {
			upgrade = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply legacy %s: %v", migration.Name, err)
		}
	}
	if upgrade.Name == "" {
		t.Fatal("group period limits migration missing")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	type snapshot struct {
		name, groupID, periodID, userID string
		anchor, start, end, created     time.Time
		archived                        *time.Time
	}
	var snapshots []snapshot
	for _, state := range []string{"active", "future", "expired", "archived"} {
		user := globalUsageIntegrationUser(t, ctx, s, "term-upgrade-"+state+"-"+id, UserRoleMember)
		groupID, _ := newUUID()
		periodID, _ := newUUID()
		start := now.Add(-time.Hour)
		if state == "future" {
			start = now.Add(24 * time.Hour)
		} else if state == "expired" {
			start = now.Add(-72 * time.Hour)
		}
		value := snapshot{name: state, groupID: groupID, periodID: periodID, userID: user.ID,
			anchor: start.Add(-365 * 24 * time.Hour), start: start, end: start.Add(24 * time.Hour), created: now.Add(-400 * 24 * time.Hour)}
		if state == "archived" {
			archived := now.Add(-time.Minute)
			value.archived = &archived
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO user_groups(id,name,limit_usd,member_limit_usd,period,starts_at,archived_at,created_at,updated_at)
			VALUES($1,$2,100,20,'day',$3,$4,$5,$5)`, groupID, state, value.anchor, value.archived, value.created); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO group_usage_periods(id,group_id,starts_at,ends_at,limit_usd,member_limit_usd,used_usd,created_at)
			VALUES($1,$2,$3,$4,100,20,12,$5)`, periodID, groupID, value.start, value.end, value.created); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE user_groups SET current_period_id=$2 WHERE id=$1`, groupID, periodID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO group_member_usage(period_id,user_id,used_usd) VALUES($1,$2,7)`, periodID, user.ID); err != nil {
			t.Fatal(err)
		}
		if state != "archived" {
			if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts SET group_id=$2 WHERE user_id=$1`, user.ID, groupID); err != nil {
				t.Fatal(err)
			}
		}
		snapshots = append(snapshots, value)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, upgrade.SQL); err != nil {
		_ = tx.Rollback()
		t.Fatalf("apply group period limits upgrade: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, before := range snapshots {
		t.Run(before.name, func(t *testing.T) {
			g, err := s.GetGroup(ctx, before.groupID)
			if err != nil {
				t.Fatal(err)
			}
			if g.PeriodCount != 1 || g.CurrentPeriodNumber != 1 || g.ExpiresAt == nil || !g.ExpiresAt.Equal(before.end) ||
				g.PeriodID != before.periodID || !g.StartsAt.Equal(before.anchor) || !g.PeriodStartsAt.Equal(before.start) || !g.PeriodEndsAt.Equal(before.end) ||
				g.LimitUSD != "100.000000000000" || g.MemberLimitUSD == nil || *g.MemberLimitUSD != "20.000000000000" || g.UsedUSD != "12.000000000000" ||
				!g.CreatedAt.Equal(before.created) || !g.UpdatedAt.Equal(before.created) || (g.ArchivedAt == nil) != (before.archived == nil) {
				t.Fatalf("migration changed saved group: %+v", g)
			}
			var memberUsed, periodLimit, memberLimit string
			var periods int
			if err := s.db.QueryRowContext(ctx, `SELECT m.used_usd::text,p.limit_usd::text,p.member_limit_usd::text,
				(SELECT count(*) FROM group_usage_periods WHERE group_id=$2)
				FROM group_member_usage m JOIN group_usage_periods p ON p.id=m.period_id WHERE m.period_id=$1 AND m.user_id=$3`,
				before.periodID, before.groupID, before.userID).Scan(&memberUsed, &periodLimit, &memberLimit, &periods); err != nil ||
				memberUsed != "7.000000000000" || periodLimit != "100.000000000000" || memberLimit != "20.000000000000" || periods != 1 {
				t.Fatalf("migration changed period or member accounting: used=%s limits=%s/%s rows=%d %v", memberUsed, periodLimit, memberLimit, periods, err)
			}
			if before.archived == nil && (g.MemberCount != 1 || len(g.Members) != 1 || g.Members[0].UserID != before.userID || g.Members[0].UsedUSD != memberUsed) {
				t.Fatalf("migration changed membership: %+v", g)
			}
			if before.archived != nil && !g.ArchivedAt.Equal(*before.archived) {
				t.Fatalf("migration changed archive time: %+v", g)
			}
		})
	}
}
