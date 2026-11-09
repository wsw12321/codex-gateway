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

func TestProtocolConversionSettingsPostgresIntegration(t *testing.T) {
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
	actor := usageSummaryUser(t, ctx, repository, "conversion-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	// The test database is shared by store tests; start from the stored state.
	initial, err := repository.ProtocolConversionSettings(ctx)
	if err != nil || initial.Revision < 1 {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	var defaultEnabled bool
	if err := repository.db.QueryRowContext(ctx, `SELECT column_default = 'false' FROM information_schema.columns
		WHERE table_name = 'protocol_conversion_settings' AND column_name = 'claude_responses_enabled'`).Scan(&defaultEnabled); err != nil || !defaultEnabled {
		t.Fatalf("claude_responses_enabled default is not false: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Microsecond)
	enabled, err := repository.SetProtocolConversionSettings(ctx, SetProtocolConversionSettingsParams{
		ClaudeResponsesEnabled: !initial.ClaudeResponsesEnabled, ExpectedRevision: initial.Revision,
		ActorUserID: actor.ID, SourceIP: "192.0.2.30", At: at,
	})
	if err != nil || enabled.ClaudeResponsesEnabled == initial.ClaudeResponsesEnabled || enabled.Revision != initial.Revision+1 || !enabled.UpdatedAt.Equal(at) {
		t.Fatalf("toggle=%+v err=%v", enabled, err)
	}
	if read, err := repository.ProtocolConversionSettings(ctx); err != nil || read != enabled {
		t.Fatalf("read back=%+v err=%v", read, err)
	}
	// A page that still holds the old revision cannot overwrite the change.
	if _, err := repository.SetProtocolConversionSettings(ctx, SetProtocolConversionSettingsParams{
		ClaudeResponsesEnabled: initial.ClaudeResponsesEnabled, ExpectedRevision: initial.Revision, ActorUserID: actor.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision err=%v", err)
	}
	if _, err := repository.SetProtocolConversionSettings(ctx, SetProtocolConversionSettingsParams{ExpectedRevision: enabled.Revision}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing actor err=%v", err)
	}
	var audits int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events
		WHERE event_type='gateway.protocol_conversion_changed' AND actor_user_id=$1::uuid
		AND (metadata->>'claude_responses_enabled')::boolean = $2`, actor.ID, enabled.ClaudeResponsesEnabled).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit events=%d err=%v", audits, err)
	}
	restored, err := repository.SetProtocolConversionSettings(ctx, SetProtocolConversionSettingsParams{
		ClaudeResponsesEnabled: initial.ClaudeResponsesEnabled, ExpectedRevision: enabled.Revision, ActorUserID: actor.ID,
	})
	if err != nil || restored.ClaudeResponsesEnabled != initial.ClaudeResponsesEnabled {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
}
