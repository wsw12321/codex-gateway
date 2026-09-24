package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUpstreamConcurrencyLimitRejectsInvalidWritesBeforeDatabase(t *testing.T) {
	t.Parallel()
	repository := New(nil)
	for _, params := range []SetUpstreamAccountConcurrentLimitParams{
		{AccountID: "0123456789abcdef", Limit: 0, ActorUserID: "actor"},
		{AccountID: "0123456789abcdef", Limit: -1, ActorUserID: "actor"},
		{AccountID: "0123456789abcdef", Limit: MaxUpstreamConcurrentLimit + 1, ActorUserID: "actor"},
		{AccountID: "0123456789abcdeF", Limit: 1, ActorUserID: "actor"},
		{AccountID: "0123456789abcdef", Limit: 1},
	} {
		if _, err := repository.SetUpstreamAccountConcurrentLimit(context.Background(), params); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid write %+v error = %v, want ErrInvalid", params, err)
		}
	}
}

func TestUpstreamConcurrencyLimitMigration(t *testing.T) {
	t.Parallel()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Name == "0017_upstream_concurrency.sql" {
			sql = migration.SQL
		}
	}
	if sql == "" {
		t.Fatal("0017_upstream_concurrency.sql is missing")
	}
	for _, required := range []string{
		"concurrent_limit INTEGER NOT NULL DEFAULT 1",
		"CHECK (concurrent_limit > 0)",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("concurrency migration missing %q", required)
		}
	}
}
