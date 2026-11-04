package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGroupAccountUsageVisibilityMigrationDefaultsOff(t *testing.T) {
	t.Parallel()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Name == "0042_group_account_usage_visibility.sql" {
			sql = migration.SQL
		}
	}
	if !strings.Contains(sql, "ADD COLUMN account_usage_visible BOOLEAN NOT NULL DEFAULT false") {
		t.Fatalf("account usage migration must add a disabled-by-default column: %q", sql)
	}
}

func TestGroupAccountUsageRejectsInvalidInputBeforeDatabase(t *testing.T) {
	t.Parallel()
	repository := New(nil)
	for _, params := range []SetGroupAccountUsageVisibilityParams{
		{GroupID: "not-a-group", BillingWriteParams: BillingWriteParams{OperationID: "op", ActorUserID: "actor", Reason: "why"}},
		{GroupID: "00000000-0000-0000-0000-000000000000", BillingWriteParams: BillingWriteParams{OperationID: "op", ActorUserID: "actor", Reason: "why"}},
		{GroupID: "00000000-0000-4000-8000-000000000001", BillingWriteParams: BillingWriteParams{OperationID: "op", ActorUserID: "actor"}},
		{GroupID: "00000000-0000-4000-8000-000000000001", BillingWriteParams: BillingWriteParams{ActorUserID: "actor", Reason: "why"}},
	} {
		if _, err := repository.SetGroupAccountUsageVisibility(context.Background(), params); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid write %+v error = %v, want ErrInvalid", params, err)
		}
	}
	if _, err := repository.ListGroupAccountUsageTargets(context.Background(), "member"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid member error = %v, want ErrInvalid", err)
	}
}
