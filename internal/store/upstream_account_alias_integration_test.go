//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUpstreamAccountAliasPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	actor := usageSummaryUser(t, ctx, repository, "alias-actor-"+suffix)
	id := upstreamIntegrationAccountID("alias-account-" + suffix)
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: id, MaskedEmail: "a***@example.com", Plan: "plus", Status: UpstreamAccountStatusAvailable}}, at); err != nil {
		t.Fatal(err)
	}
	alias := func() string {
		t.Helper()
		var value string
		if err := repository.db.QueryRowContext(ctx, `SELECT alias FROM upstream_accounts WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := alias(); got != "" {
		t.Fatalf("default alias=%q; want empty", got)
	}

	account, err := repository.SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{
		AccountID: id, Alias: "  主力 Plus  ", ActorUserID: actor.ID, SourceIP: "192.0.2.10", At: at,
	})
	if err != nil || account.Alias != "主力 Plus" || account.MaskedEmail != "a***@example.com" {
		t.Fatalf("set alias result=%+v err=%v", account, err)
	}
	// A later metadata snapshot, including a model-identification sync that
	// omits display names, must not replace the Gateway-owned alias.
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: id, MaskedEmail: "b***@example.com", Plan: "pro", Status: UpstreamAccountStatusAvailable}}, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := alias(); got != "主力 Plus" {
		t.Fatalf("sync overwrote alias=%q", got)
	}
	listed, err := repository.ListUpstreamAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range listed {
		if item.ID == id {
			found = item.Alias == "主力 Plus" && item.MaskedEmail == "b***@example.com"
		}
	}
	if !found {
		t.Fatalf("listed accounts missing alias for %s: %+v", id, listed)
	}
	summaries, err := repository.SummarizeUpstreamAccounts(ctx, UpstreamAccountSummaryFilter{From: at.Add(-time.Hour), Until: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, item := range summaries {
		if item.AccountID != nil && *item.AccountID == id {
			found = item.Alias == "主力 Plus"
		}
	}
	if !found {
		t.Fatalf("summary missing alias for %s", id)
	}

	// The account belongs to Codex; another provider's namespace cannot rename it.
	if _, err := repository.WithUpstreamProvider(UpstreamProviderAnthropic).SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{
		AccountID: id, Alias: "other", ActorUserID: actor.ID, At: at,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-provider alias error=%v; want ErrNotFound", err)
	}
	if _, err := repository.SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{
		AccountID: id, Alias: "rolled back", ActorUserID: actor.ID, SourceIP: "not-an-ip", At: at,
	}); err == nil {
		t.Fatal("invalid audit source IP succeeded")
	}
	if got := alias(); got != "主力 Plus" {
		t.Fatalf("failed audit did not roll back alias=%q", got)
	}
	if _, err := repository.SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{
		AccountID: upstreamIntegrationAccountID("missing-" + suffix), Alias: "x", ActorUserID: actor.ID, At: at,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account error=%v; want ErrNotFound", err)
	}
	if _, err := repository.SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{
		AccountID: id, Alias: "", ActorUserID: actor.ID, At: at,
	}); err != nil || alias() != "" {
		t.Fatalf("clear alias=%q err=%v", alias(), err)
	}

	// The constraint is the last line of defense for writes outside the store API.
	for _, unsafe := range []string{"user@example.com", " padded", "line\nbreak", strings.Repeat("号", 41)} {
		if _, err := repository.db.ExecContext(ctx, `UPDATE upstream_accounts SET alias=$2 WHERE id=$1`, id, unsafe); err == nil {
			t.Fatalf("constraint accepted alias %q", unsafe)
		}
	}

	var previous, next string
	var audits int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND subject_id=$2 AND event_type='upstream_account.alias_changed'`, actor.ID, id).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit count=%d err=%v; want 2", audits, err)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT metadata->>'previous_alias', metadata->>'alias' FROM audit_events
		WHERE actor_user_id=$1 AND subject_id=$2 AND event_type='upstream_account.alias_changed' AND metadata->>'alias'<>''`, actor.ID, id).Scan(&previous, &next); err != nil ||
		previous != "" || next != "主力 Plus" {
		t.Fatalf("set audit previous=%q alias=%q err=%v", previous, next, err)
	}
}
