//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestGlobalUsagePostgresIntegration(t *testing.T) {
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
	owner, createdOwner := globalUsageIntegrationOwner(t, ctx, repository, suffix)
	if createdOwner {
		t.Cleanup(func() {
			_ = repository.DisableUser(context.Background(), owner.ID, time.Now().UTC())
		})
	}
	member := globalUsageIntegrationUser(t, ctx, repository, "gu-member-"+suffix, UserRoleMember)
	zeroUsageMember := globalUsageIntegrationUser(t, ctx, repository, "gu-zero-"+suffix, UserRoleMember)
	ownerDevice, ownerKey := globalUsageIntegrationKey(t, ctx, repository, owner, suffix+"o")
	memberDevice, memberKey := globalUsageIntegrationKey(t, ctx, repository, member, suffix+"m")

	now := time.Now().UTC().Truncate(time.Microsecond)
	currentMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	liveFrom := currentMonth.AddDate(0, -1, 0)
	oldAt := liveFrom.AddDate(0, -3, 1)
	previousMonthAt := liveFrom.AddDate(0, 0, 1)
	currentAt := now.Add(-time.Minute)
	if currentAt.Before(currentMonth) {
		currentAt = currentMonth
	}
	until := now.Add(time.Minute)
	model := "global-usage-" + suffix

	insertGlobalUsageSeries(t, ctx, repository, owner, ownerDevice, ownerKey, model, suffix+"-old", oldAt, 1, 10, 0, 2, 1)
	insertGlobalUsageSeries(t, ctx, repository, owner, ownerDevice, ownerKey, model, suffix+"-previous", previousMonthAt, 1, 20, 5, 4, 2)
	insertGlobalUsageSeries(t, ctx, repository, member, memberDevice, memberKey, model, suffix+"-current", currentAt, 501, 1, 0, 1, 0)

	if err := repository.AggregateUsageMonth(ctx, oldAt, "UTC"); err != nil {
		t.Fatalf("AggregateUsageMonth(old): %v", err)
	}
	// A previous-month aggregate exists, but all-history mode must ignore it
	// and use retained request rows from liveFrom onward exactly once.
	if err := repository.AggregateUsageMonth(ctx, liveFrom, "UTC"); err != nil {
		t.Fatalf("AggregateUsageMonth(previous): %v", err)
	}
	result, err := repository.db.ExecContext(ctx,
		`DELETE FROM usage_requests WHERE model = $1 AND requested_at < $2`, model, liveFrom,
	)
	if err != nil {
		t.Fatalf("delete retained detail: %v", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil || deleted != 1 {
		t.Fatalf("deleted old details = %d, err = %v", deleted, err)
	}

	allRows, err := repository.GlobalUsage(ctx, time.Time{}, until, model, true, liveFrom)
	if err != nil {
		t.Fatalf("GlobalUsage(all): %v", err)
	}
	ownerAll := findGlobalUsageIntegrationRow(t, allRows, owner.ID)
	memberAll := findGlobalUsageIntegrationRow(t, allRows, member.ID)
	zeroAll := findGlobalUsageIntegrationRow(t, allRows, zeroUsageMember.ID)
	if ownerAll.RequestCount != 2 || ownerAll.InputTokens != 30 || ownerAll.OutputTokens != 6 {
		t.Fatalf("owner all-history usage = %+v", ownerAll)
	}
	if memberAll.RequestCount != 501 || memberAll.InputTokens != 501 || memberAll.OutputTokens != 501 {
		t.Fatalf("member all-history usage = %+v", memberAll)
	}
	if zeroAll.RequestCount != 0 || zeroAll.Model != "" {
		t.Fatalf("zero-usage member = %+v", zeroAll)
	}
	assertGlobalUsageIntegrationTotals(t, allRows, 503, 531, 507)

	boundedRows, err := repository.GlobalUsage(ctx, liveFrom, until, model, false, time.Time{})
	if err != nil {
		t.Fatalf("GlobalUsage(bounded): %v", err)
	}
	ownerBounded := findGlobalUsageIntegrationRow(t, boundedRows, owner.ID)
	if ownerBounded.RequestCount != 1 || ownerBounded.InputTokens != 20 || ownerBounded.OutputTokens != 4 {
		t.Fatalf("previous month was duplicated or omitted: %+v", ownerBounded)
	}
	assertGlobalUsageIntegrationTotals(t, boundedRows, 502, 521, 505)

	filter := UsageFilter{From: &liveFrom, Until: &until, Model: model, Limit: 500}
	details, err := repository.ListUsageRequests(ctx, filter)
	if err != nil {
		t.Fatalf("ListUsageRequests: %v", err)
	}
	if len(details) != 500 {
		t.Fatalf("detail rows = %d, want capped 500", len(details))
	}
	summary, err := repository.SummarizeUsageRequests(ctx, filter)
	if err != nil {
		t.Fatalf("SummarizeUsageRequests: %v", err)
	}
	if summary.RequestCount != 502 || summary.InputTokens != 521 || summary.OutputTokens != 505 {
		t.Fatalf("summary was truncated by detail limit: %+v", summary)
	}
}

func globalUsageIntegrationOwner(t *testing.T, ctx context.Context, repository *Store, suffix string) (User, bool) {
	t.Helper()
	owner, err := scanUser(repository.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE role = 'owner' AND status = 'active' LIMIT 1`,
	))
	if err == nil {
		return owner, false
	}
	if err != sql.ErrNoRows {
		t.Fatalf("find active owner: %v", err)
	}
	return globalUsageIntegrationUser(t, ctx, repository, "gu-owner-"+suffix, UserRoleOwner), true
}

func globalUsageIntegrationUser(t *testing.T, ctx context.Context, repository *Store, username, role string) User {
	t.Helper()
	user, err := repository.CreateUser(ctx, CreateUserParams{
		Username: username, DisplayName: username, Role: role,
	})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	return user
}

func globalUsageIntegrationKey(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	user User,
	suffix string,
) (Device, APIKey) {
	t.Helper()
	device, err := repository.CreateDevice(ctx, CreateDeviceParams{
		UserID: user.ID, Name: "global-usage-" + suffix,
	})
	if err != nil {
		t.Fatalf("CreateDevice(%s): %v", user.Username, err)
	}
	hash := sha256.Sum256([]byte(user.ID + suffix))
	now := time.Now().UTC()
	key, err := repository.CreateAPIKey(ctx, CreateAPIKeyParams{
		PublicID:  "globalkey" + suffix,
		KeyPrefix: "cgk_gu_" + suffix,
		KeyHash:   hash[:], UserID: user.ID, DeviceID: device.ID,
		Name:      "global-usage-" + suffix,
		CreatedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(%s): %v", user.Username, err)
	}
	return device, key
}

func insertGlobalUsageSeries(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	user User,
	device Device,
	key APIKey,
	model string,
	requestBase string,
	requestedAt time.Time,
	count int,
	inputTokens, cachedInputTokens, outputTokens, reasoningTokens int64,
) {
	t.Helper()
	_, err := repository.db.ExecContext(ctx, `
		INSERT INTO usage_requests (
			request_id, user_id, device_id, api_key_id, key_prefix, model,
			endpoint, state, http_status, requested_at, completed_at,
			input_tokens, cached_input_tokens, output_tokens, reasoning_tokens
		)
		SELECT 'gu-' || $1 || '-' || series::text, $2, $3, $4, $5, $6,
			'responses', 'completed', 200, $7, $7::timestamptz + interval '1 second',
			$8, $9, $10, $11
		FROM generate_series(1, $12) AS series`,
		requestBase, user.ID, device.ID, key.ID, key.KeyPrefix, model, requestedAt,
		inputTokens, cachedInputTokens, outputTokens, reasoningTokens, count,
	)
	if err != nil {
		t.Fatalf("insert usage series %s: %v", requestBase, err)
	}
}

func findGlobalUsageIntegrationRow(t *testing.T, rows []GlobalUsageRow, userID string) GlobalUsageRow {
	t.Helper()
	for _, row := range rows {
		if row.UserID == userID {
			return row
		}
	}
	t.Fatalf("global usage omitted user %s", userID)
	return GlobalUsageRow{}
}

func assertGlobalUsageIntegrationTotals(t *testing.T, rows []GlobalUsageRow, requests, input, output int64) {
	t.Helper()
	var actualRequests, actualInput, actualOutput int64
	for _, row := range rows {
		actualRequests += row.RequestCount
		actualInput += row.InputTokens
		actualOutput += row.OutputTokens
	}
	if actualRequests != requests || actualInput != input || actualOutput != output {
		t.Fatalf(
			"global totals = requests %d, input %d, output %d; want %d, %d, %d",
			actualRequests, actualInput, actualOutput, requests, input, output,
		)
	}
}
