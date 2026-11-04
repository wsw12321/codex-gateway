package store

import (
	"strings"
	"testing"
	"time"
)

func quotaWindow(used int, reset time.Time) *UpstreamQuotaWindow {
	return &UpstreamQuotaWindow{UsedPercent: used, ResetAt: &reset}
}

func TestUpstreamQuotaGuardMigration(t *testing.T) {
	t.Parallel()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Name == "0044_upstream_quota_guard.sql" {
			sql = migration.SQL
		}
	}
	if sql == "" {
		t.Fatal("0044_upstream_quota_guard.sql is missing")
	}
	for _, required := range []string{
		"safety_percent SMALLINT NOT NULL DEFAULT 5 CHECK (safety_percent BETWEEN 0 AND 50)",
		"INSERT INTO upstream_quota_guard_settings(singleton) VALUES (true)",
		"REFERENCES upstream_accounts(id) ON DELETE CASCADE",
		"status IN ('available', 'limited')",
		"source IN ('auto', 'manual')",
		"status = 'limited' OR (limited_window IS NULL AND reset_at IS NULL AND restore_at IS NULL)",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration lacks %q", required)
		}
	}
}

func TestEvaluateUpstreamQuotaGuardTripsAtThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	fiveReset, weekReset := now.Add(2*time.Hour), now.Add(72*time.Hour)
	for _, test := range []struct {
		name        string
		five, week  int
		percent     int
		limited     bool
		window      string
		wantRestore time.Time
	}{
		{name: "remaining equals threshold", five: 95, week: 10, percent: 5, limited: true, window: UpstreamQuotaWindowFiveHour, wantRestore: fiveReset.Add(5 * time.Minute)},
		{name: "one point above threshold", five: 94, week: 10, percent: 5},
		{name: "weekly trips", five: 10, week: 96, percent: 5, limited: true, window: UpstreamQuotaWindowSevenDay, wantRestore: weekReset.Add(5 * time.Minute)},
		{name: "both trip keeps latest reset", five: 100, week: 99, percent: 5, limited: true, window: UpstreamQuotaWindowSevenDay, wantRestore: weekReset.Add(5 * time.Minute)},
		{name: "zero threshold waits for exhaustion", five: 99, week: 10, percent: 0},
		{name: "zero threshold exhausted", five: 100, week: 10, percent: 0, limited: true, window: UpstreamQuotaWindowFiveHour, wantRestore: fiveReset.Add(5 * time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{
				ObservedAt: now, FiveHour: quotaWindow(test.five, fiveReset), SevenDay: quotaWindow(test.week, weekReset),
			}, nil, test.percent, now)
			if decision.limited != test.limited || decision.window != test.window {
				t.Fatalf("decision=%+v", decision)
			}
			if !test.limited {
				if decision.restoreAt != nil || decision.resetAt != nil {
					t.Fatalf("available decision carries times: %+v", decision)
				}
				return
			}
			if decision.restoreAt == nil || !decision.restoreAt.Equal(test.wantRestore) || !decision.nextCheckAt.Equal(test.wantRestore) {
				t.Fatalf("restore=%v next=%v want %v", decision.restoreAt, decision.nextCheckAt, test.wantRestore)
			}
		})
	}
}

func TestEvaluateUpstreamQuotaGuardIgnoresWindowsThatAlreadyReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	decision := evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{
		ObservedAt: now.Add(-6 * time.Hour), FiveHour: quotaWindow(100, now.Add(-time.Minute)), SevenDay: quotaWindow(20, now.Add(48*time.Hour)),
	}, nil, 5, now)
	if decision.limited || !decision.nextCheckAt.Equal(now.Add(UpstreamQuotaGuardMaxInterval)) {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestEvaluateUpstreamQuotaGuardHoldsWindowWithoutResetTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	decision := evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{
		ObservedAt: now, FiveHour: &UpstreamQuotaWindow{UsedPercent: 97},
	}, nil, 5, now)
	if !decision.limited || decision.window != UpstreamQuotaWindowFiveHour || decision.resetAt != nil ||
		decision.restoreAt == nil || !decision.restoreAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("decision=%+v", decision)
	}
	// A known later reset outlasts the fallback hold.
	decision = evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{
		ObservedAt: now, FiveHour: &UpstreamQuotaWindow{UsedPercent: 97}, SevenDay: quotaWindow(99, now.Add(5*time.Hour)),
	}, nil, 5, now)
	if !decision.limited || decision.window != UpstreamQuotaWindowSevenDay || !decision.restoreAt.Equal(now.Add(5*time.Hour+5*time.Minute)) {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestEvaluateUpstreamQuotaGuardSchedulesByMargin(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	for _, test := range []struct {
		used int
		want time.Duration
	}{
		{used: 90, want: 10 * time.Minute}, // margin 5
		{used: 85, want: 10 * time.Minute}, // margin 10
		{used: 80, want: 15 * time.Minute}, // margin 15
		{used: 70, want: 30 * time.Minute}, // margin 25
		{used: 50, want: 60 * time.Minute}, // margin 45
		{used: 0, want: 60 * time.Minute},
	} {
		decision := evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{
			ObservedAt: now, FiveHour: quotaWindow(test.used, reset), SevenDay: quotaWindow(0, now.Add(100*time.Hour)),
		}, nil, 5, now)
		if decision.limited || !decision.nextCheckAt.Equal(now.Add(test.want)) {
			t.Fatalf("used=%d next=%v want %v", test.used, decision.nextCheckAt.Sub(now), test.want)
		}
	}
	decision := evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{ObservedAt: now}, nil, 5, now)
	if decision.limited || !decision.nextCheckAt.Equal(now.Add(UpstreamQuotaGuardMinInterval)) {
		t.Fatalf("empty observation next=%v", decision.nextCheckAt)
	}
}

func TestEvaluateUpstreamQuotaGuardShortensIntervalForFastUsage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset := now.Add(4 * time.Hour)
	previous := &UpstreamQuotaObservation{ObservedAt: now.Add(-time.Hour), FiveHour: quotaWindow(10, reset.Add(time.Minute))}
	// 50 points remain above the threshold; 20 points per hour reaches it in
	// 2.5 hours, so the check comes after half of that, capped at the maximum.
	decision := evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{ObservedAt: now, FiveHour: quotaWindow(30, reset)}, previous, 20, now)
	if !decision.nextCheckAt.Equal(now.Add(UpstreamQuotaGuardMaxInterval)) {
		t.Fatalf("slow usage next=%v", decision.nextCheckAt.Sub(now))
	}
	// 30 points per hour leaves 40 points: 80 minutes to the threshold.
	decision = evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{ObservedAt: now, FiveHour: quotaWindow(40, reset)}, previous, 20, now)
	if !decision.nextCheckAt.Equal(now.Add(40 * time.Minute)) {
		t.Fatalf("fast usage next=%v", decision.nextCheckAt.Sub(now))
	}
	// Never closer than the minimum interval, however fast usage climbs.
	decision = evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{ObservedAt: now, FiveHour: quotaWindow(70, reset)}, previous, 20, now)
	if !decision.nextCheckAt.Equal(now.Add(UpstreamQuotaGuardMinInterval)) {
		t.Fatalf("burst usage next=%v", decision.nextCheckAt.Sub(now))
	}
	// A reading from another period says nothing about the current burn rate.
	other := &UpstreamQuotaObservation{ObservedAt: now.Add(-time.Hour), FiveHour: quotaWindow(10, reset.Add(-5*time.Hour))}
	decision = evaluateUpstreamQuotaGuard(UpstreamQuotaObservation{ObservedAt: now, FiveHour: quotaWindow(40, reset)}, other, 20, now)
	if !decision.nextCheckAt.Equal(now.Add(UpstreamQuotaGuardMaxInterval)) {
		t.Fatalf("new period next=%v", decision.nextCheckAt.Sub(now))
	}
}

func TestUpstreamQuotaGuardStateReadsExpiredLimitAsAvailable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset, restore := now.Add(-10*time.Minute), now.Add(-5*time.Minute)
	row := upstreamQuotaGuardRow{accountID: "0123456789abcdef", status: UpstreamQuotaGuardLimited, source: UpstreamQuotaGuardSourceAuto, resetAt: &reset, restoreAt: &restore}
	row.limitedWindow.String, row.limitedWindow.Valid = UpstreamQuotaWindowFiveHour, true
	if state := row.state(now); state.Status != UpstreamQuotaGuardAvailable || state.RestoreAt != nil || state.LimitedWindow != "" {
		t.Fatalf("expired state=%+v", state)
	}
	if state := row.state(now.Add(-6 * time.Minute)); state.Status != UpstreamQuotaGuardLimited || state.LimitedWindow != UpstreamQuotaWindowFiveHour {
		t.Fatalf("active state=%+v", state)
	}
	manual := upstreamQuotaGuardRow{status: UpstreamQuotaGuardLimited, source: UpstreamQuotaGuardSourceManual}
	if !manual.limitedAt(now.Add(365 * 24 * time.Hour)) {
		t.Fatal("manual limit without a reset must hold until lifted")
	}
}

func TestUpstreamQuotaGuardNextAutomaticCheckHonorsFloor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	checked, next := now.Add(-2*time.Minute), now.Add(-time.Minute)
	state := UpstreamQuotaGuardState{AutoCheckedAt: &checked, NextCheckAt: &next}
	if got := state.NextAutomaticCheck(); !got.Equal(checked.Add(10 * time.Minute)) {
		t.Fatalf("next=%v", got)
	}
	later := now.Add(30 * time.Minute)
	state.NextCheckAt = &later
	if got := state.NextAutomaticCheck(); !got.Equal(later) {
		t.Fatalf("next=%v", got)
	}
}

func TestSoonestUpstreamQuotaResetSkipsPastWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	row := upstreamQuotaGuardRow{fiveHour: quotaWindow(40, now.Add(-time.Minute)), sevenDay: quotaWindow(40, now.Add(30*time.Hour))}
	if name, reset := soonestUpstreamQuotaReset(row, now); name != UpstreamQuotaWindowSevenDay || reset == nil || !reset.Equal(now.Add(30*time.Hour)) {
		t.Fatalf("name=%q reset=%v", name, reset)
	}
	if name, reset := soonestUpstreamQuotaReset(upstreamQuotaGuardRow{}, now); name != "" || reset != nil {
		t.Fatalf("empty snapshot name=%q reset=%v", name, reset)
	}
}
