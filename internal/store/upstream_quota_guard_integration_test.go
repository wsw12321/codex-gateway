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

func TestUpstreamQuotaGuardPostgresIntegration(t *testing.T) {
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
	actor := usageSummaryUser(t, ctx, repository, "guard-actor-"+suffix)
	codexID := upstreamIntegrationAccountID("guard-codex-" + suffix)
	otherID := upstreamIntegrationAccountID("guard-other-" + suffix)
	claudeID := upstreamIntegrationAccountID("guard-claude-" + suffix)
	agyID := upstreamIntegrationAccountID("guard-agy-" + suffix)
	at := time.Now().UTC().Truncate(time.Microsecond)
	anthropic := repository.WithUpstreamProvider(UpstreamProviderAnthropic)
	antigravity := repository.WithUpstreamProvider(UpstreamProviderAntigravity)
	if err := repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{
		{ID: codexID, MaskedEmail: "c***@example.com", Plan: "plus", Status: UpstreamAccountStatusAvailable},
		{ID: otherID, MaskedEmail: "o***@example.com", Plan: "plus", Status: UpstreamAccountStatusAvailable},
	}, at); err != nil {
		t.Fatal(err)
	}
	if err := anthropic.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: claudeID, MaskedEmail: "l***@example.com", Plan: "unknown", Status: UpstreamAccountStatusAvailable}}, at); err != nil {
		t.Fatal(err)
	}
	if err := antigravity.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: agyID, MaskedEmail: "g***@example.com", Plan: "unknown", Status: UpstreamAccountStatusAvailable}}, at); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = repository.SetUpstreamQuotaGuardSettings(context.Background(), SetUpstreamQuotaGuardSettingsParams{
			SafetyPercent: DefaultUpstreamQuotaGuardPercent, ActorUserID: actor.ID,
		})
	})
	eligible := func(ids ...string) map[string]bool {
		t.Helper()
		rows, err := repository.EligibleUpstreamAccountLimits(ctx, actor.ID, ids)
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]bool{}
		for _, row := range rows {
			result[row.ID] = true
		}
		return result
	}
	stateOf := func(store *Store, id string, when time.Time) UpstreamQuotaGuardState {
		t.Helper()
		_, states, err := store.ListUpstreamQuotaGuards(ctx, when)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if state.AccountID == id {
				return state
			}
		}
		t.Fatalf("no guard state for %s", id)
		return UpstreamQuotaGuardState{}
	}
	audits := func(id, eventType string) int {
		t.Helper()
		var count int
		if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events
			WHERE subject_id = $1 AND event_type = $2`, id, eventType).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	if got := eligible(codexID, otherID); !got[codexID] || !got[otherID] {
		t.Fatalf("unchecked accounts must stay eligible: %v", got)
	}

	// Claims enforce the minimum interval and cover only Codex and Claude.
	if claim, err := repository.ClaimUpstreamQuotaCheck(ctx, codexID, at); err != nil || !claim.Claimed || claim.Provider != UpstreamProviderCodex {
		t.Fatalf("first claim=%+v err=%v", claim, err)
	}
	if claim, err := repository.ClaimUpstreamQuotaCheck(ctx, codexID, at.Add(9*time.Minute)); err != nil || claim.Claimed ||
		!claim.NextCheckAt.Equal(at.Add(UpstreamQuotaGuardMinInterval)) {
		t.Fatalf("early claim=%+v err=%v", claim, err)
	}
	if claim, err := repository.ClaimUpstreamQuotaCheck(ctx, agyID, at); err != nil || claim.Provider != "" || claim.Claimed {
		t.Fatalf("antigravity claim=%+v err=%v", claim, err)
	}
	if claim, err := repository.ClaimUpstreamQuotaCheck(ctx, claudeID, at); err != nil || claim.Provider != UpstreamProviderAnthropic || !claim.Claimed {
		t.Fatalf("claude claim=%+v err=%v", claim, err)
	}

	// An automatic reading at the threshold limits the account until the
	// window resets plus the restore delay.
	reset := at.Add(2 * time.Hour)
	week := at.Add(72 * time.Hour)
	state, err := repository.RecordUpstreamQuotaObservation(ctx, RecordUpstreamQuotaObservationParams{
		Provider: UpstreamProviderCodex, AccountID: codexID, Automatic: true, At: at,
		Observation: UpstreamQuotaObservation{FiveHour: quotaWindow(95, reset), SevenDay: quotaWindow(20, week)},
	})
	if err != nil || state.Status != UpstreamQuotaGuardLimited || state.Source != UpstreamQuotaGuardSourceAuto ||
		state.LimitedWindow != UpstreamQuotaWindowFiveHour || state.RestoreAt == nil || !state.RestoreAt.Equal(reset.Add(5*time.Minute)) ||
		!state.NextAutomaticCheck().Equal(reset.Add(5*time.Minute)) {
		t.Fatalf("limited state=%+v err=%v", state, err)
	}
	if audits(codexID, "upstream_quota_guard.limited") != 1 {
		t.Fatal("automatic limit was not audited")
	}
	if got := eligible(codexID, otherID); got[codexID] || !got[otherID] {
		t.Fatalf("limited account remained eligible: %v", got)
	}
	if _, err := repository.SelectUpstreamAccount(ctx, actor.ID, []string{codexID}, at); !errors.Is(err, ErrNoUpstreamAccount) {
		t.Fatalf("limited account selected: %v", err)
	}
	if id, err := repository.SelectUpstreamAccount(ctx, actor.ID, []string{codexID}, reset.Add(6*time.Minute)); err != nil || id != codexID {
		t.Fatalf("restored account not selected: %q %v", id, err)
	}
	allocations, err := repository.ListUpstreamAccountAllocations(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, allocation := range allocations {
		if allocation.AccountID == codexID && allocation.TargetShare != "0" {
			t.Fatalf("limited account kept target share %q", allocation.TargetShare)
		}
	}
	if restored := stateOf(repository, codexID, reset.Add(6*time.Minute)); restored.Status != UpstreamQuotaGuardAvailable || restored.RestoreAt != nil {
		t.Fatalf("state after restore time=%+v", restored)
	}

	// A manual change only sets the current state. Other readings keep it;
	// the next automatic check, no earlier than the minimum interval, does not.
	manual, err := repository.SetUpstreamQuotaGuardStatus(ctx, SetUpstreamQuotaGuardStatusParams{
		AccountID: codexID, Limited: false, ActorUserID: actor.ID, SourceIP: "192.0.2.10", At: at.Add(time.Minute),
	})
	if err != nil || manual.Status != UpstreamQuotaGuardAvailable || manual.Source != UpstreamQuotaGuardSourceManual ||
		!manual.NextAutomaticCheck().Equal(at.Add(UpstreamQuotaGuardMinInterval)) {
		t.Fatalf("manual available=%+v err=%v", manual, err)
	}
	if got := eligible(codexID); !got[codexID] {
		t.Fatal("manually reopened account is not eligible")
	}
	observed, err := repository.RecordUpstreamQuotaObservation(ctx, RecordUpstreamQuotaObservationParams{
		Provider: UpstreamProviderCodex, AccountID: codexID, At: at.Add(2 * time.Minute),
		Observation: UpstreamQuotaObservation{FiveHour: quotaWindow(99, reset)},
	})
	if err != nil || observed.Status != UpstreamQuotaGuardAvailable || observed.Source != UpstreamQuotaGuardSourceManual ||
		observed.FiveHour == nil || observed.FiveHour.UsedPercent != 99 {
		t.Fatalf("observation overrode manual state=%+v err=%v", observed, err)
	}
	automatic, err := repository.RecordUpstreamQuotaObservation(ctx, RecordUpstreamQuotaObservationParams{
		Provider: UpstreamProviderCodex, AccountID: codexID, Automatic: true, At: at.Add(11 * time.Minute),
		Observation: UpstreamQuotaObservation{FiveHour: quotaWindow(99, reset)},
	})
	if err != nil || automatic.Status != UpstreamQuotaGuardLimited || automatic.Source != UpstreamQuotaGuardSourceAuto {
		t.Fatalf("automatic check kept manual state=%+v err=%v", automatic, err)
	}

	// Without a known future reset a manual limit holds until it is lifted.
	held, err := repository.SetUpstreamQuotaGuardStatus(ctx, SetUpstreamQuotaGuardStatusParams{AccountID: otherID, Limited: true, ActorUserID: actor.ID, At: at})
	if err != nil || held.Status != UpstreamQuotaGuardLimited || held.RestoreAt != nil || held.ResetAt != nil {
		t.Fatalf("manual limit=%+v err=%v", held, err)
	}
	if far := stateOf(repository, otherID, at.Add(365*24*time.Hour)); far.Status != UpstreamQuotaGuardLimited {
		t.Fatalf("manual limit expired: %+v", far)
	}
	if got := eligible(otherID); got[otherID] {
		t.Fatal("manually limited account is eligible")
	}
	if audits(otherID, "upstream_quota_guard.manual_set") != 1 {
		t.Fatal("manual limit was not audited")
	}
	if _, err := repository.SetUpstreamQuotaGuardStatus(ctx, SetUpstreamQuotaGuardStatusParams{AccountID: otherID, Limited: false, ActorUserID: actor.ID, At: at}); err != nil {
		t.Fatal(err)
	}
	// A manual limit after a reading lasts until the soonest future reset.
	if _, err := anthropic.RecordUpstreamQuotaObservation(ctx, RecordUpstreamQuotaObservationParams{
		Provider: UpstreamProviderAnthropic, AccountID: claudeID, At: at,
		Observation: UpstreamQuotaObservation{FiveHour: quotaWindow(30, reset), SevenDay: quotaWindow(10, week)},
	}); err != nil {
		t.Fatal(err)
	}
	if limited, err := anthropic.SetUpstreamQuotaGuardStatus(ctx, SetUpstreamQuotaGuardStatusParams{AccountID: claudeID, Limited: true, ActorUserID: actor.ID, At: at}); err != nil ||
		limited.LimitedWindow != UpstreamQuotaWindowFiveHour || limited.RestoreAt == nil || !limited.RestoreAt.Equal(reset.Add(5*time.Minute)) {
		t.Fatalf("claude manual limit=%+v err=%v", limited, err)
	}

	// Provider scoping.
	if _, err := repository.SetUpstreamQuotaGuardStatus(ctx, SetUpstreamQuotaGuardStatusParams{AccountID: claudeID, Limited: true, ActorUserID: actor.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("codex store changed a Claude account: %v", err)
	}
	if _, err := antigravity.SetUpstreamQuotaGuardStatus(ctx, SetUpstreamQuotaGuardStatusParams{AccountID: agyID, Limited: true, ActorUserID: actor.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("antigravity account accepted: %v", err)
	}
	if _, err := repository.RecordUpstreamQuotaObservation(ctx, RecordUpstreamQuotaObservationParams{Provider: UpstreamProviderCodex, AccountID: claudeID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading recorded under the wrong provider: %v", err)
	}

	// The safety percentage applies at once to automatic states.
	if _, err := repository.RecordUpstreamQuotaObservation(ctx, RecordUpstreamQuotaObservationParams{
		Provider: UpstreamProviderCodex, AccountID: otherID, Automatic: true, At: at,
		Observation: UpstreamQuotaObservation{FiveHour: quotaWindow(75, reset), SevenDay: quotaWindow(10, week)},
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(repository, otherID, at); got.Status != UpstreamQuotaGuardAvailable {
		t.Fatalf("75%% used limited at 5%%: %+v", got)
	}
	settings, err := repository.SetUpstreamQuotaGuardSettings(ctx, SetUpstreamQuotaGuardSettingsParams{SafetyPercent: 30, ActorUserID: actor.ID, At: at})
	if err != nil || settings.SafetyPercent != 30 {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
	if got := stateOf(repository, otherID, at); got.Status != UpstreamQuotaGuardLimited || got.Source != UpstreamQuotaGuardSourceAuto {
		t.Fatalf("raised threshold did not limit: %+v", got)
	}
	if got := stateOf(anthropic, claudeID, at); got.Source != UpstreamQuotaGuardSourceManual || got.Status != UpstreamQuotaGuardLimited {
		t.Fatalf("settings change touched a manual state: %+v", got)
	}
	lowered, err := repository.SetUpstreamQuotaGuardSettings(ctx, SetUpstreamQuotaGuardSettingsParams{SafetyPercent: 5, ActorUserID: actor.ID, At: at})
	if err != nil || lowered.Revision != settings.Revision+1 {
		t.Fatalf("lowered=%+v err=%v", lowered, err)
	}
	if got := stateOf(repository, otherID, at); got.Status != UpstreamQuotaGuardAvailable || got.NextCheckAt != nil {
		t.Fatalf("lowered threshold did not reopen for a fresh check: %+v", got)
	}
	if _, err := repository.SetUpstreamQuotaGuardSettings(ctx, SetUpstreamQuotaGuardSettingsParams{SafetyPercent: 51, ActorUserID: actor.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("51%% accepted: %v", err)
	}
}
