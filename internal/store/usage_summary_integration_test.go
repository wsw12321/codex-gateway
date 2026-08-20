//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestUsageSummaryChargedUSDPostgresIntegration(t *testing.T) {
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
	userA := usageSummaryUser(t, ctx, repository, "usage-a-"+suffix)
	userB := usageSummaryUser(t, ctx, repository, "usage-b-"+suffix)
	deviceA1 := usageSummaryDevice(t, ctx, repository, userA.ID, "a1-"+suffix)
	deviceA2 := usageSummaryDevice(t, ctx, repository, userA.ID, "a2-"+suffix)
	deviceB := usageSummaryDevice(t, ctx, repository, userB.ID, "b-"+suffix)
	projectA1 := usageSummaryProject(t, ctx, repository, userA.ID, "a1-"+suffix)
	projectA2 := usageSummaryProject(t, ctx, repository, userA.ID, "a2-"+suffix)
	projectB := usageSummaryProject(t, ctx, repository, userB.ID, "b-"+suffix)
	keyA1 := usageSummaryKey(t, ctx, repository, userA.ID, deviceA1.ID, "a1-"+suffix, now)
	keyA2 := usageSummaryKey(t, ctx, repository, userA.ID, deviceA1.ID, "a2-"+suffix, now)
	keyA3 := usageSummaryKey(t, ctx, repository, userA.ID, deviceA2.ID, "a3-"+suffix, now)
	keyB := usageSummaryKey(t, ctx, repository, userB.ID, deviceB.ID, "b-"+suffix, now)
	modelPrimary := "summary-model-" + suffix
	modelOther := "summary-other-" + suffix
	modelPending := "pending-model-" + suffix
	modelCatalog := "catalog-model-" + suffix

	type fixture struct {
		name       string
		user       User
		device     Device
		key        APIKey
		project    Project
		model      string
		endpoint   string
		state      string
		httpStatus int
		requested  time.Time
		input      int64
		output     int64
		charged    string
	}
	fixtures := []fixture{
		{name: "base", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 200,
			requested: now, input: 100, output: 25, charged: "1.000000000001"},
		{name: "old", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 200,
			requested: now.Add(-2 * time.Hour), input: 20, output: 2, charged: "2"},
		{name: "device", user: userA, device: deviceA2, key: keyA3, project: projectA1,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 200,
			requested: now, input: 30, output: 3, charged: "3"},
		{name: "key", user: userA, device: deviceA1, key: keyA2, project: projectA1,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 200,
			requested: now, input: 40, output: 4, charged: "4"},
		{name: "project", user: userA, device: deviceA1, key: keyA1, project: projectA2,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 200,
			requested: now, input: 50, output: 5, charged: "5"},
		{name: "model", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelOther, endpoint: "responses", state: "completed", httpStatus: 200,
			requested: now, input: 60, output: 6, charged: "6"},
		{name: "state", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelPrimary, endpoint: "responses", state: "failed", httpStatus: 500,
			requested: now, input: 70, output: 7, charged: "7"},
		{name: "status", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 201,
			requested: now, input: 80, output: 8, charged: "8"},
		{name: "class", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 404,
			requested: now, input: 90, output: 9, charged: "9"},
		{name: "user", user: userB, device: deviceB, key: keyB, project: projectB,
			model: modelPrimary, endpoint: "responses", state: "completed", httpStatus: 200,
			requested: now, input: 100, output: 10, charged: "10"},
		{name: "unsettled", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelPending, endpoint: "responses", state: "in_progress",
			requested: now, input: 110, output: 11},
		{name: "models", user: userA, device: deviceA1, key: keyA1, project: projectA1,
			model: modelCatalog, endpoint: "models", state: "completed", httpStatus: 200,
			requested: now, input: 0, output: 0},
	}
	for index, value := range fixtures {
		requestID := "usage-summary-" + suffix + "-" + strconv.Itoa(index)
		completedAt := any(nil)
		if value.state != "in_progress" {
			completedAt = value.requested.Add(time.Second)
		}
		var httpStatus any
		if value.httpStatus != 0 {
			httpStatus = value.httpStatus
		}
		if _, err := repository.db.ExecContext(ctx, `
			INSERT INTO usage_requests
				(request_id,user_id,device_id,api_key_id,key_prefix,project_id,model,
				 endpoint,state,http_status,requested_at,completed_at,input_tokens,
				 cached_input_tokens,output_tokens,reasoning_tokens)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0,$14,0)`,
			requestID, value.user.ID, value.device.ID, value.key.ID, value.key.KeyPrefix,
			value.project.ID, value.model, value.endpoint, value.state, httpStatus,
			value.requested, completedAt, value.input, value.output,
		); err != nil {
			t.Fatalf("insert usage fixture %s: %v", value.name, err)
		}
		if value.charged == "" {
			continue
		}
		if _, err := repository.db.ExecContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id,entry_type,amount_usd,cash_delta_usd,request_id,model,
				 input_tokens,cached_input_tokens,output_tokens,actual_cost_usd,
				 charged_usd,uncovered_usd,reason,created_at)
			VALUES ($1,'usage_charge',$2::numeric,0,$3,$4,$5,0,$6,
				$2::numeric,$2::numeric,0,'usage summary fixture',$7)`,
			value.user.ID, value.charged, requestID, value.model,
			value.input, value.output, value.requested.Add(time.Second),
		); err != nil {
			t.Fatalf("insert usage ledger fixture %s: %v", value.name, err)
		}
	}

	from, until := now.Add(-30*time.Minute), now.Add(30*time.Minute)
	tests := []struct {
		name       string
		filter     UsageFilter
		wantCount  int64
		wantCharge string
	}{
		{name: "time", filter: UsageFilter{From: &from, Until: &until, UserID: userA.ID}, wantCount: 10, wantCharge: "43.000000000001"},
		{name: "user", filter: UsageFilter{UserID: userA.ID}, wantCount: 11, wantCharge: "45.000000000001"},
		{name: "device", filter: UsageFilter{DeviceID: deviceA2.ID}, wantCount: 1, wantCharge: "3.000000000000"},
		{name: "key", filter: UsageFilter{APIKeyID: keyA2.ID}, wantCount: 1, wantCharge: "4.000000000000"},
		{name: "project", filter: UsageFilter{ProjectID: projectA2.ID}, wantCount: 1, wantCharge: "5.000000000000"},
		{name: "model", filter: UsageFilter{Model: modelOther}, wantCount: 1, wantCharge: "6.000000000000"},
		{name: "state", filter: UsageFilter{UserID: userA.ID, State: "failed"}, wantCount: 1, wantCharge: "7.000000000000"},
		{name: "http status", filter: UsageFilter{UserID: userA.ID, HTTPStatus: 201}, wantCount: 1, wantCharge: "8.000000000000"},
		{name: "status class", filter: UsageFilter{UserID: userA.ID, StatusClass: 4}, wantCount: 1, wantCharge: "9.000000000000"},
		{name: "other user", filter: UsageFilter{UserID: userB.ID}, wantCount: 1, wantCharge: "10.000000000000"},
		{name: "unsettled", filter: UsageFilter{UserID: userA.ID, Model: modelPending, State: "in_progress"}, wantCount: 1, wantCharge: "0"},
		{name: "models zero", filter: UsageFilter{UserID: userA.ID, Model: modelCatalog}, wantCount: 1, wantCharge: "0"},
		{name: "combined", filter: UsageFilter{
			From: &from, Until: &until, UserID: userA.ID, DeviceID: deviceA1.ID,
			APIKeyID: keyA1.ID, ProjectID: projectA1.ID, Model: modelPrimary,
			State: "completed", HTTPStatus: 200,
		}, wantCount: 1, wantCharge: "1.000000000001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.filter.Limit = 500
			details, err := repository.ListUsageRequests(ctx, test.filter)
			if err != nil {
				t.Fatalf("ListUsageRequests: %v", err)
			}
			summary, err := repository.SummarizeUsageRequests(ctx, test.filter)
			if err != nil {
				t.Fatalf("SummarizeUsageRequests: %v", err)
			}
			var inputTokens, outputTokens int64
			for _, detail := range details {
				inputTokens += detail.InputTokens
				outputTokens += detail.OutputTokens
			}
			if summary.RequestCount != test.wantCount || int64(len(details)) != test.wantCount ||
				summary.InputTokens != inputTokens || summary.OutputTokens != outputTokens ||
				summary.ChargedUSD != test.wantCharge {
				t.Fatalf("summary/detail scope mismatch: summary=%+v details=%d want_count=%d want_charge=%s",
					summary, len(details), test.wantCount, test.wantCharge)
			}
		})
	}
}

func usageSummaryUser(t *testing.T, ctx context.Context, repository *Store, username string) User {
	t.Helper()
	user, err := repository.CreateUser(ctx, CreateUserParams{
		Username: username, DisplayName: username, Role: UserRoleMember,
	})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	return user
}

func usageSummaryDevice(t *testing.T, ctx context.Context, repository *Store, userID, name string) Device {
	t.Helper()
	device, err := repository.CreateDevice(ctx, CreateDeviceParams{UserID: userID, Name: name})
	if err != nil {
		t.Fatalf("CreateDevice(%s): %v", name, err)
	}
	return device
}

func usageSummaryProject(t *testing.T, ctx context.Context, repository *Store, userID, suffix string) Project {
	t.Helper()
	project, err := repository.CreateProject(ctx, CreateProjectParams{
		UserID: userID, Slug: "usage-" + suffix, Name: "usage " + suffix,
	})
	if err != nil {
		t.Fatalf("CreateProject(%s): %v", suffix, err)
	}
	return project
}

func usageSummaryKey(
	t *testing.T,
	ctx context.Context,
	repository *Store,
	userID, deviceID, suffix string,
	createdAt time.Time,
) APIKey {
	t.Helper()
	digest := sha256.Sum256([]byte("usage-key-" + suffix))
	key, err := repository.CreateAPIKey(ctx, CreateAPIKeyParams{
		PublicID: "usagekey" + suffix, KeyPrefix: "cgk_us_" + suffix,
		KeyHash: digest[:], SecretCiphertext: []byte{1}, UserID: userID,
		DeviceID: deviceID, Name: "usage key " + suffix,
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(%s): %v", suffix, err)
	}
	return key
}
