//go:build integration

package store

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestSummarizeUsageByModelPostgresIntegration(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	user := usageSummaryUser(t, ctx, repository, "models-a-"+suffix)
	other := usageSummaryUser(t, ctx, repository, "models-b-"+suffix)
	device := usageSummaryDevice(t, ctx, repository, user.ID, "ma-"+suffix)
	otherDevice := usageSummaryDevice(t, ctx, repository, other.ID, "mb-"+suffix)
	key := usageSummaryKey(t, ctx, repository, user.ID, device.ID, "ma-"+suffix, now)
	otherKey := usageSummaryKey(t, ctx, repository, other.ID, otherDevice.ID, "mb-"+suffix, now)
	cheap, costly, idle := "models-cheap-"+suffix, "models-costly-"+suffix, "models-idle-"+suffix

	type fixture struct {
		user      User
		key       APIKey
		model     string
		state     string
		status    int
		requested time.Time
		input     int64
		cached    int64
		output    int64
		charged   string
	}
	fixtures := []fixture{
		{user: user, key: key, model: cheap, state: "completed", status: 200, requested: now.Add(-time.Hour), input: 100, cached: 60, output: 10, charged: "0.100000000001"},
		{user: user, key: key, model: cheap, state: "failed", status: 500, requested: now.Add(-2 * time.Hour), input: 5, output: 0, charged: "0.2"},
		{user: user, key: key, model: costly, state: "completed", status: 200, requested: now.Add(-3 * time.Hour), input: 200, output: 50, charged: "2.5"},
		{user: user, key: key, model: idle, state: "in_progress", requested: now.Add(-time.Minute), input: 7, output: 1},
		// Outside the window and another user's request never count.
		{user: user, key: key, model: costly, state: "completed", status: 200, requested: now.Add(-30 * time.Hour), input: 999, output: 999, charged: "99"},
		{user: other, key: otherKey, model: cheap, state: "completed", status: 200, requested: now.Add(-time.Hour), input: 888, output: 888, charged: "88"},
	}
	for index, value := range fixtures {
		requestID := "usage-models-" + suffix + "-" + strconv.Itoa(index)
		var completedAt, status any
		if value.state != "in_progress" {
			completedAt = value.requested.Add(time.Second)
		}
		if value.status != 0 {
			status = value.status
		}
		if _, err := repository.db.ExecContext(ctx, `
			INSERT INTO usage_requests
				(request_id,user_id,device_id,api_key_id,key_prefix,model,endpoint,state,http_status,
				 requested_at,completed_at,input_tokens,cached_input_tokens,output_tokens,reasoning_tokens)
			VALUES ($1,$2,(SELECT device_id FROM api_keys WHERE id=$3),$3,$4,$5,'responses',$6,$7,$8,$9,$10,$11,$12,0)`,
			requestID, value.user.ID, value.key.ID, value.key.KeyPrefix, value.model, value.state, status,
			value.requested, completedAt, value.input, value.cached, value.output); err != nil {
			t.Fatalf("insert usage fixture %d: %v", index, err)
		}
		if value.charged == "" {
			continue
		}
		if _, err := repository.db.ExecContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id,entry_type,amount_usd,cash_delta_usd,request_id,model,
				 input_tokens,cached_input_tokens,output_tokens,actual_cost_usd,
				 charged_usd,group_charged_usd,personal_charged_usd,uncovered_usd,reason,created_at)
			VALUES ($1,'usage_charge',$2::numeric,0,$3,$4,$5,$6,$7,
				$2::numeric,$2::numeric,0,$2::numeric,0,'usage models fixture',$8)`,
			value.user.ID, value.charged, requestID, value.model, value.input, value.cached, value.output,
			value.requested.Add(time.Second)); err != nil {
			t.Fatalf("insert ledger fixture %d: %v", index, err)
		}
	}

	from, until := now.Add(-24*time.Hour), now.Add(time.Second)
	filter := UsageFilter{From: &from, Until: &until, UserID: user.ID}
	breakdown, err := repository.SummarizeUsageByModel(ctx, filter)
	if err != nil {
		t.Fatalf("SummarizeUsageByModel: %v", err)
	}
	want := []UsageModelSummary{
		{Model: costly, RequestCount: 1, InputTokens: 200, OutputTokens: 50, ChargedUSD: "2.500000000000"},
		{Model: cheap, RequestCount: 2, ErrorCount: 1, InputTokens: 105, CachedInputTokens: 60, OutputTokens: 10, ChargedUSD: "0.300000000001"},
		{Model: idle, RequestCount: 1, InProgressCount: 1, InputTokens: 7, OutputTokens: 1, ChargedUSD: "0"},
	}
	if len(breakdown.Models) != len(want) || breakdown.Truncated {
		t.Fatalf("models = %+v", breakdown)
	}
	for index := range want {
		if breakdown.Models[index] != want[index] {
			t.Fatalf("model %d = %+v, want %+v", index, breakdown.Models[index], want[index])
		}
	}
	summary, err := repository.SummarizeUsageRequests(ctx, filter)
	if err != nil {
		t.Fatalf("SummarizeUsageRequests: %v", err)
	}
	if breakdown.TotalRequests != summary.RequestCount || breakdown.TotalTokens != summary.InputTokens+summary.OutputTokens ||
		breakdown.TotalChargedUSD != "2.800000000001" || summary.ChargedUSD != "2.800000000001" {
		t.Fatalf("totals do not match the summary: breakdown=%+v summary=%+v", breakdown, summary)
	}

	modelOnly := filter
	modelOnly.Model = cheap
	single, err := repository.SummarizeUsageByModel(ctx, modelOnly)
	if err != nil {
		t.Fatalf("SummarizeUsageByModel(model): %v", err)
	}
	if len(single.Models) != 1 || single.Models[0] != want[1] || single.TotalChargedUSD != "0.300000000001" {
		t.Fatalf("model filter = %+v", single)
	}

	empty, err := repository.SummarizeUsageByModel(ctx, UsageFilter{From: &from, Until: &until, UserID: user.ID, Model: "missing-" + suffix})
	if err != nil {
		t.Fatalf("SummarizeUsageByModel(empty): %v", err)
	}
	if len(empty.Models) != 0 || empty.TotalRequests != 0 || empty.TotalChargedUSD != "0.000000000000" {
		t.Fatalf("empty = %+v", empty)
	}
}
