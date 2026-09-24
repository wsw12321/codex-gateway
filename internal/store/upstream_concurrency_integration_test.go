//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestUpstreamConcurrencyLimitPostgresIntegration(t *testing.T) {
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
	actor := usageSummaryUser(t, ctx, repository, "concurrency-actor-"+suffix)
	id := upstreamIntegrationAccountID("concurrency-account-" + suffix)
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := repository.EnsureUpstreamAccount(ctx, id, at); err != nil {
		t.Fatal(err)
	}
	var limit int
	if err := repository.db.QueryRowContext(ctx, `SELECT concurrent_limit FROM upstream_accounts WHERE id=$1`, id).Scan(&limit); err != nil || limit != 1 {
		t.Fatalf("default concurrent limit=%d err=%v; want 1", limit, err)
	}
	account, err := repository.SetUpstreamAccountConcurrentLimit(ctx, SetUpstreamAccountConcurrentLimitParams{
		AccountID: id, Limit: 3, ActorUserID: actor.ID, At: at,
	})
	if err != nil || account.ConcurrentLimit != 3 {
		t.Fatalf("set limit result=%+v err=%v", account, err)
	}
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: id, MaskedEmail: "c***@example.com", Plan: "plus", Status: UpstreamAccountStatusAvailable}}, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT concurrent_limit FROM upstream_accounts WHERE id=$1`, id).Scan(&limit); err != nil || limit != 3 {
		t.Fatalf("sync overwrote configured limit=%d err=%v", limit, err)
	}
	eligible, err := repository.EligibleUpstreamAccountLimits(ctx, actor.ID, []string{id})
	if err != nil || len(eligible) != 1 || eligible[0].ID != id || eligible[0].ConcurrentLimit != 3 {
		t.Fatalf("eligibility=%+v err=%v", eligible, err)
	}
	if _, err := repository.SetUpstreamAccountConcurrentLimit(ctx, SetUpstreamAccountConcurrentLimitParams{
		AccountID: id, Limit: 4, ActorUserID: actor.ID, SourceIP: "not-an-ip", At: at,
	}); err == nil {
		t.Fatal("invalid audit source IP succeeded")
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT concurrent_limit FROM upstream_accounts WHERE id=$1`, id).Scan(&limit); err != nil || limit != 3 {
		t.Fatalf("failed audit did not roll back limit=%d err=%v", limit, err)
	}
	if _, err := repository.SetUpstreamAccountConcurrentLimit(ctx, SetUpstreamAccountConcurrentLimitParams{
		AccountID: upstreamIntegrationAccountID("missing-" + suffix), Limit: 1, ActorUserID: actor.ID, At: at,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account error=%v; want ErrNotFound", err)
	}
	var audits int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND subject_id=$2 AND event_type='upstream_account.concurrent_limit_changed'`, actor.ID, id).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count=%d err=%v; want 1", audits, err)
	}
}
