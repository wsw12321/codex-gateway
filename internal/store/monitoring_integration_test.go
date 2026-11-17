//go:build integration

package store

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// The request monitor names each attributed account by its Gateway alias and
// CPA name, joined from the durable account row like the masked email.
func TestMonitoringAccountNamesPostgresIntegration(t *testing.T) {
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
	at := time.Now().UTC().Truncate(time.Microsecond)
	named := upstreamIntegrationAccountID("monitor-named-" + suffix)
	plain := upstreamIntegrationAccountID("monitor-plain-" + suffix)
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{
		{ID: named, DisplayName: "team-a", MaskedEmail: "a***@example.com", Plan: "plus", Status: UpstreamAccountStatusAvailable},
		{ID: plain, MaskedEmail: "p***@example.com", Plan: "plus", Status: UpstreamAccountStatusAvailable},
	}, at); err != nil {
		t.Fatal(err)
	}
	actor := usageSummaryUser(t, ctx, repository, "monitor-actor-"+suffix)
	if _, err := repository.SetUpstreamAccountAlias(ctx, SetUpstreamAccountAliasParams{
		AccountID: named, Alias: "主力 Plus", ActorUserID: actor.ID, At: at,
	}); err != nil {
		t.Fatal(err)
	}

	user := usageSummaryUser(t, ctx, repository, "monitor-"+suffix)
	device := usageSummaryDevice(t, ctx, repository, user.ID, "monitor-"+suffix)
	key := usageSummaryKey(t, ctx, repository, user.ID, device.ID, suffix, at.Add(-time.Hour))
	requests := map[string]string{
		"monitor-named-" + suffix:        named,
		"monitor-plain-" + suffix:        plain,
		"monitor-unattributed-" + suffix: "",
	}
	for requestID, accountID := range requests {
		if _, err := repository.BeginUsageRequest(ctx, BeginUsageRequestParams{
			RequestID: requestID, UserID: user.ID, DeviceID: device.ID, APIKeyID: key.ID,
			Model: "monitor-model", Endpoint: "responses", RequestedAt: at, RequestBytes: 10,
		}); err != nil {
			t.Fatalf("BeginUsageRequest(%s): %v", requestID, err)
		}
		if accountID != "" {
			if _, err := repository.db.ExecContext(ctx, `UPDATE usage_requests SET upstream_account_id=$2 WHERE request_id=$1`, requestID, accountID); err != nil {
				t.Fatal(err)
			}
		}
	}

	snapshot, err := repository.ListMonitoringRequests(ctx, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	value := func(pointer *string) string {
		if pointer == nil {
			return "<nil>"
		}
		return *pointer
	}
	for _, window := range [][]MonitoringRequest{snapshot.InProgress, snapshot.Recent} {
		found := 0
		for _, row := range window {
			if _, ok := requests[row.RequestID]; !ok {
				continue
			}
			found++
			got := [4]string{value(row.UpstreamAccountID), value(row.UpstreamAlias), value(row.UpstreamDisplayName), value(row.UpstreamMaskedEmail)}
			want := map[string][4]string{
				"monitor-named-" + suffix:        {named, "主力 Plus", "team-a", "a***@example.com"},
				"monitor-plain-" + suffix:        {plain, "<nil>", "<nil>", "p***@example.com"},
				"monitor-unattributed-" + suffix: {"<nil>", "<nil>", "<nil>", "<nil>"},
			}[row.RequestID]
			if got != want {
				t.Fatalf("%s account fields=%v; want %v", row.RequestID, got, want)
			}
		}
		if found != len(requests) {
			t.Fatalf("monitoring window returned %d of %d fixture requests", found, len(requests))
		}
	}
}
