//go:build integration

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

// Exercise the plugin's quota report through the real route, authentication
// and quota guard tables.
func TestObservedUpstreamQuotaPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pgConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*pgConfig)
	defer admin.Close()
	schema := "quota_push_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	pgConfig.RuntimeParams["search_path"] = schema
	repository := store.New(stdlib.OpenDB(*pgConfig))
	defer repository.Close()
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const claudeID = "0c1a0de000000001"
	now := time.Now().UTC()
	if err := repository.SyncUpstreamAccounts(ctx, []store.UpstreamAccountSnapshot{{
		ID: quotaPushAccount, MaskedEmail: "c***@example.com", Plan: "plus", Status: "available",
	}}, now); err != nil {
		t.Fatal(err)
	}
	anthropic := repository.WithUpstreamProvider(store.UpstreamProviderAnthropic)
	if err := anthropic.SyncUpstreamAccounts(ctx, []store.UpstreamAccountSnapshot{{
		ID: claudeID, MaskedEmail: "l***@example.com", Plan: "unknown", Status: "available",
	}}, now); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		config: config.Config{SidecarToken: "sidecar-test-secret"}, store: repository, quotaGuardRepo: repository,
		mux: http.NewServeMux(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.routes()
	stateOf := func(scoped *store.Store, id string) store.UpstreamQuotaGuardState {
		t.Helper()
		_, states, err := scoped.ListUpstreamQuotaGuards(ctx, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if state.AccountID == id {
				return state
			}
		}
		t.Fatalf("no guard state for %s", id)
		return store.UpstreamQuotaGuardState{}
	}
	audits := func(id, eventType string) int {
		t.Helper()
		var count int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM `+schema+`.audit_events
			WHERE subject_id = $1 AND event_type = $2 AND metadata->>'via' = 'observation'`, id, eventType).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	push := func(path, body string) {
		t.Helper()
		if w := quotaPushServe(s, path, body, nil); w.Code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	reset, week := now.Add(2*time.Hour).Unix(), now.Add(72*time.Hour).Unix()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }

	push(quotaPushCodex, codexQuotaPushBody(30, 300, reset, week, at(30*time.Second)))
	if got := stateOf(repository, quotaPushAccount); got.Status != store.UpstreamQuotaGuardAvailable || got.FiveHour == nil ||
		got.FiveHour.UsedPercent != 30 || got.SevenDay == nil || got.SevenDay.UsedPercent != 55 || got.NextCheckAt == nil {
		t.Fatalf("first report=%+v", got)
	}
	// Crossing the safety margin limits the account at once.
	push(quotaPushCodex, codexQuotaPushBody(97, 300, reset, week, at(20*time.Second)))
	limited := stateOf(repository, quotaPushAccount)
	if limited.Status != store.UpstreamQuotaGuardLimited || limited.Source != store.UpstreamQuotaGuardSourceAuto ||
		limited.FiveHour.UsedPercent != 97 || limited.RestoreAt == nil || limited.RestoreAt.Unix() != reset+int64(store.UpstreamQuotaGuardRestoreDelay/time.Second) {
		t.Fatalf("limited=%+v", limited)
	}
	if audits(quotaPushAccount, "upstream_quota_guard.limited") != 1 {
		t.Fatal("the limit was not audited")
	}
	// A report that arrives late changes nothing.
	push(quotaPushCodex, codexQuotaPushBody(10, 300, reset, week, at(40*time.Second)))
	if got := stateOf(repository, quotaPushAccount); got.Status != store.UpstreamQuotaGuardLimited || got.FiveHour.UsedPercent != 97 ||
		!got.ObservedAt.Equal(*limited.ObservedAt) {
		t.Fatalf("a late report moved the snapshot back: %+v", got)
	}
	if audits(quotaPushAccount, "upstream_quota_guard.restored") != 0 {
		t.Fatal("a late report restored the account")
	}

	// Claude reports go to the Claude account; an account is never reached
	// through another provider's namespace.
	windows := `[{"window":"five_hour","used_percent":96,"reset_at":"` + time.Unix(reset, 0).UTC().Format(time.RFC3339) + `","status":"allowed_warning"},` +
		`{"window":"seven_day","used_percent":10,"status":"allowed"}]`
	body := strings.Replace(claudeQuotaPushBody(at(10*time.Second), windows), quotaPushAccount, claudeID, 1)
	push(quotaPushAnthropic, body)
	if got := stateOf(anthropic, claudeID); got.Status != store.UpstreamQuotaGuardLimited || got.FiveHour.UsedPercent != 96 {
		t.Fatalf("claude=%+v", got)
	}
	if w := quotaPushServe(s, quotaPushCodex, strings.Replace(codexQuotaPushBody(30, 300, reset, week, at(time.Second)), quotaPushAccount, claudeID, 1), nil); w.Code != http.StatusNotFound {
		t.Fatalf("a Claude account took a Codex report: %d %s", w.Code, w.Body.String())
	}
	if w := quotaPushServe(s, quotaPushCodex, strings.Replace(codexQuotaPushBody(30, 300, reset, week, at(time.Second)), quotaPushAccount, "fedcba9876543210", 1), nil); w.Code != http.StatusNotFound ||
		!strings.Contains(w.Body.String(), "upstream_account_not_found") {
		t.Fatalf("unknown account: %d %s", w.Code, w.Body.String())
	}
}
