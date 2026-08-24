//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestUpstreamAccountsPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	listedID := upstreamIntegrationAccountID("listed-" + suffix)
	traceID := upstreamIntegrationAccountID("trace-" + suffix)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{
		ID: listedID, MaskedEmail: "l***@example.com", Plan: "plus",
		Status: UpstreamAccountStatusAvailable,
	}}, now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("initial SyncUpstreamAccounts: %v", err)
	}
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{
		ID: upstreamIntegrationAccountID("unsafe-" + suffix), MaskedEmail: "full@example.com", Plan: "plus",
		Status: UpstreamAccountStatusAvailable,
	}}, now.Add(-time.Minute)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("full email synchronization error = %v, want ErrInvalid", err)
	}

	oldAt := now.Add(-110 * 24 * time.Hour).Truncate(time.Second)
	user := usageSummaryUser(t, ctx, repository, "upstream-"+suffix)
	device := usageSummaryDevice(t, ctx, repository, user.ID, "upstream-"+suffix)
	key := usageSummaryKey(t, ctx, repository, user.ID, device.ID, suffix, oldAt.Add(-time.Hour))
	accountRequestID := "upstream-account-" + suffix
	unattributedRequestID := "upstream-unknown-" + suffix

	for _, fixture := range []struct {
		requestID string
		state     string
		status    int
		accountID string
		input     int64
		output    int64
	}{
		{accountRequestID, "completed", 200, traceID, 120, 30},
		{unattributedRequestID, "failed", 500, "full@example.com", 40, 10},
	} {
		if _, err := repository.BeginUsageRequest(ctx, BeginUsageRequestParams{
			RequestID: fixture.requestID, UserID: user.ID, DeviceID: device.ID,
			APIKeyID: key.ID, Model: "upstream-summary-model", Endpoint: "responses",
			RequestedAt: oldAt, RequestBytes: 10,
		}); err != nil {
			t.Fatalf("BeginUsageRequest(%s): %v", fixture.requestID, err)
		}
		completed, err := repository.CompleteUsageRequest(ctx, CompleteUsageRequestParams{
			RequestID: fixture.requestID, State: fixture.state, HTTPStatus: fixture.status,
			CompletedAt: oldAt.Add(time.Second), InputTokens: fixture.input,
			OutputTokens: fixture.output, RequestBytes: 10, ResponseBytes: 20,
			UpstreamAccountID: fixture.accountID,
		})
		if err != nil {
			t.Fatalf("CompleteUsageRequest(%s): %v", fixture.requestID, err)
		}
		if fixture.accountID == traceID && (completed.UpstreamAccountID == nil || *completed.UpstreamAccountID != traceID) {
			t.Fatalf("recognized trace attribution = %v, want %q", completed.UpstreamAccountID, traceID)
		}
		if fixture.accountID != traceID && completed.UpstreamAccountID != nil {
			t.Fatalf("malformed trace attribution = %v, want nil", completed.UpstreamAccountID)
		}
	}

	accounts, err := repository.ListUpstreamAccounts(ctx)
	if err != nil {
		t.Fatalf("ListUpstreamAccounts(placeholders): %v", err)
	}
	placeholder := findUpstreamAccount(accounts, traceID)
	if placeholder == nil || placeholder.MaskedEmail != "" || placeholder.Plan != "unknown" ||
		placeholder.Status != UpstreamAccountStatusUnavailable || placeholder.LastSyncedAt != nil {
		t.Fatalf("trace placeholder = %+v", placeholder)
	}

	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{
		{ID: listedID, MaskedEmail: "l***@example.com", Plan: "plus", Status: "available", LastSyncedAt: now.Add(-2 * time.Hour)},
		{ID: traceID, MaskedEmail: "t***@example.com", Plan: "pro", Status: "available", LastSyncedAt: now.Add(-3 * time.Hour)},
	}, now); err != nil {
		t.Fatalf("synchronize placeholder metadata: %v", err)
	}
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{
		ID: listedID, MaskedEmail: "l***@example.com", Plan: "plus", Status: "available",
	}}, now.Add(time.Second)); err != nil {
		t.Fatalf("mark missing account unavailable: %v", err)
	}
	accounts, err = repository.ListUpstreamAccounts(ctx)
	if err != nil {
		t.Fatalf("ListUpstreamAccounts(after missing snapshot): %v", err)
	}
	missing := findUpstreamAccount(accounts, traceID)
	if missing == nil || missing.Status != UpstreamAccountStatusUnavailable ||
		missing.LastSyncedAt == nil || !missing.LastSyncedAt.Equal(now.Add(-3*time.Hour)) {
		t.Fatalf("missing account auth sync metadata was overwritten: %+v", missing)
	}

	for _, fixture := range []struct {
		requestID string
		accountID any
		cost      string
	}{
		{accountRequestID, traceID, "1.25"},
		{unattributedRequestID, nil, "0.75"},
	} {
		if _, err := repository.db.ExecContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id,entry_type,amount_usd,cash_delta_usd,request_id,
				 upstream_account_id,model,input_tokens,cached_input_tokens,output_tokens,
				 actual_cost_usd,charged_usd,uncovered_usd,reason,created_at,usage_requested_at)
			SELECT user_id,'usage_charge',$3::numeric,0,request_id,$2,model,
				input_tokens,cached_input_tokens,output_tokens,$3::numeric,0,$3::numeric,
				'upstream account fixture',completed_at,requested_at
			FROM usage_requests WHERE request_id = $1`,
			fixture.requestID, fixture.accountID, fixture.cost); err != nil {
			t.Fatalf("insert usage ledger fixture %s: %v", fixture.requestID, err)
		}
	}
	if err := repository.AttributeUsageRequest(ctx, accountRequestID, traceID); err != nil {
		t.Fatalf("idempotent settled attribution: %v", err)
	}
	if err := repository.AttributeUsageRequest(ctx, accountRequestID, listedID); !errors.Is(err, ErrConflict) {
		t.Fatalf("change settled attribution error = %v, want ErrConflict", err)
	}

	if err := repository.AggregateUsageDay(ctx, oldAt, "UTC"); err != nil {
		t.Fatalf("AggregateUsageDay: %v", err)
	}
	if err := repository.AggregateUsageMonth(ctx, oldAt, "UTC"); err != nil {
		t.Fatalf("AggregateUsageMonth: %v", err)
	}
	var dailyAttributed, dailyUnattributed, monthlyAttributed, monthlyUnattributed int64
	if err := repository.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT sum(request_count) FROM usage_daily
			WHERE usage_day=$1::date AND user_id=$2 AND upstream_account_id=$3),0),
		COALESCE((SELECT sum(request_count) FROM usage_daily
			WHERE usage_day=$1::date AND user_id=$2 AND upstream_account_id IS NULL),0),
		COALESCE((SELECT sum(request_count) FROM usage_monthly
			WHERE usage_month=date_trunc('month',$1::date)::date
			  AND user_id=$2 AND upstream_account_id=$3),0),
		COALESCE((SELECT sum(request_count) FROM usage_monthly
			WHERE usage_month=date_trunc('month',$1::date)::date
			  AND user_id=$2 AND upstream_account_id IS NULL),0)`,
		oldAt.Format("2006-01-02"), user.ID, traceID,
	).Scan(&dailyAttributed, &dailyUnattributed, &monthlyAttributed, &monthlyUnattributed); err != nil {
		t.Fatalf("read upstream account aggregates: %v", err)
	}
	if dailyAttributed != 1 || dailyUnattributed != 1 ||
		monthlyAttributed != 1 || monthlyUnattributed != 1 {
		t.Fatalf("daily/monthly upstream groups = %d/%d %d/%d, want 1/1 1/1",
			dailyAttributed, dailyUnattributed, monthlyAttributed, monthlyUnattributed)
	}
	deleted, err := repository.DeleteUsageRequestsBefore(ctx, now.Add(-90*24*time.Hour), 100_000)
	if err != nil {
		t.Fatalf("DeleteUsageRequestsBefore: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("deleted usage details = %d, want at least 2", deleted)
	}

	summaries, err := repository.SummarizeUpstreamAccounts(ctx, UpstreamAccountSummaryFilter{
		All: true, LiveFrom: time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC),
		Until: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("SummarizeUpstreamAccounts(all): %v", err)
	}
	trace := findUpstreamAccountSummary(summaries, &traceID)
	if trace == nil || trace.MaskedEmail != "t***@example.com" || trace.Plan != "pro" ||
		trace.Status != UpstreamAccountStatusUnavailable || trace.RequestCount != 1 ||
		trace.ErrorCount != 0 || trace.InputTokens != 120 || trace.OutputTokens != 30 ||
		trace.EquivalentCostUSD != "1.250000000000" {
		t.Fatalf("retained trace summary = %+v", trace)
	}
	unattributed := findUpstreamAccountSummary(summaries, nil)
	if unattributed == nil || unattributed.RequestCount < 1 || unattributed.ErrorCount < 1 ||
		unattributed.InputTokens < 40 || unattributed.OutputTokens < 10 {
		t.Fatalf("unattributed summary = %+v", unattributed)
	}
	listed := findUpstreamAccountSummary(summaries, &listedID)
	if listed == nil || listed.RequestCount != 0 || listed.EquivalentCostUSD != "0" {
		t.Fatalf("zero-use listed account summary = %+v", listed)
	}
}

func upstreamIntegrationAccountID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

func findUpstreamAccount(values []UpstreamAccount, id string) *UpstreamAccount {
	for index := range values {
		if values[index].ID == id {
			return &values[index]
		}
	}
	return nil
}

func findUpstreamAccountSummary(values []UpstreamAccountSummary, id *string) *UpstreamAccountSummary {
	for index := range values {
		if id == nil && values[index].AccountID == nil {
			return &values[index]
		}
		if id != nil && values[index].AccountID != nil && *values[index].AccountID == *id {
			return &values[index]
		}
	}
	return nil
}
