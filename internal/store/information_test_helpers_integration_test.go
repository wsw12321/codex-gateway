//go:build integration

package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Cleanup intentionally changes the database-wide retention boundary, so its
// fixtures must not share a schema with unrelated historical usage tests.
func informationIntegrationStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	configuration, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*configuration)
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "information_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	configuration.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*configuration)
	db.SetMaxOpenConns(20)
	s := New(db)
	t.Cleanup(func() {
		_ = s.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop information test schema: %v", err)
		}
		_ = admin.Close()
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}
