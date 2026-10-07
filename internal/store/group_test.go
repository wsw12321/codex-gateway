package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPutGroupRejectsInvalidMemberLimit(t *testing.T) {
	s := New(nil)
	for _, value := range []string{"", " ", "-1", "+1", "1e3", "01", "0.1234567", "1000000000000000000", " 1", "1 "} {
		t.Run(value, func(t *testing.T) {
			_, err := s.PutGroup(context.Background(), PutGroupParams{Name: "Test", LimitUSD: "10", Period: "day", MemberLimitUSD: &value})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("member limit %q: %v", value, err)
			}
		})
	}
}

func TestGroupPeriodDuration(t *testing.T) {
	for _, tc := range []struct {
		period string
		days   int
		want   time.Duration
	}{
		{"day", 0, 24 * time.Hour}, {"week", 0, 7 * 24 * time.Hour}, {"month", 0, 31 * 24 * time.Hour}, {"custom", 11, 11 * 24 * time.Hour},
	} {
		got, err := groupPeriodDuration(tc.period, tc.days)
		if err != nil || got != tc.want {
			t.Fatalf("duration(%s,%d) = %v,%v", tc.period, tc.days, got, err)
		}
	}
	for _, tc := range []struct {
		period string
		days   int
	}{{"custom", 0}, {"custom", -1}, {"custom", 106752}, {"day", 1}, {"year", 0}} {
		if _, err := groupPeriodDuration(tc.period, tc.days); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid duration %+v: %v", tc, err)
		}
	}
}

func TestRequireGroupQuota(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	g := &GroupSummary{ID: "test", Period: "day", RemainingUSD: "0.000000000000", PeriodStartsAt: now, PeriodEndsAt: now.Add(24 * time.Hour)}
	var exceeded *GroupQuotaExceededError
	if err := requireGroupQuota(g, now); !errors.As(err, &exceeded) || exceeded.RetryAfter != 24*time.Hour || !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("exhausted quota: %v", err)
	}
	g.RemainingUSD = "1.000000000000"
	if err := requireGroupQuota(g, now); err != nil {
		t.Fatal(err)
	}
	if err := requireGroupQuota(g, now.Add(-time.Hour)); !errors.As(err, &exceeded) || !exceeded.NotStarted || exceeded.RetryAfter != time.Hour {
		t.Fatalf("future quota: %v", err)
	}
	if err := requireGroupQuota(nil, now); err != nil {
		t.Fatal(err)
	}
}

func TestGroupPeriodStartPreservesAncientAnchor(t *testing.T) {
	anchor := time.Date(1500, 1, 1, 0, 0, 0, 123000, time.UTC)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, duration := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 31 * 24 * time.Hour, 106751 * 24 * time.Hour} {
		start := groupPeriodStart(anchor, at, duration)
		if start.After(at) || !start.Add(duration).After(at) || start.Nanosecond() != anchor.Nanosecond() {
			t.Fatalf("incorrect long elapsed cycle: start=%v at=%v duration=%v", start, at, duration)
		}
		if next := groupPeriodStart(anchor, start.Add(duration), duration); !next.Equal(start.Add(duration)) {
			t.Fatalf("end boundary %v", next)
		}
	}
}

func TestPutGroupRejectsInvalidPeriodCountAndDate(t *testing.T) {
	s := New(nil)
	for _, count := range []int{-1, 100, 1000000} {
		_, err := s.PutGroup(context.Background(), PutGroupParams{Name: "Test", LimitUSD: "10", Period: "day", PeriodCount: &count})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("count %d: %v", count, err)
		}
	}
	for _, year := range []int{0, 10000} {
		start := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		_, err := s.PutGroup(context.Background(), PutGroupParams{Name: "Test", LimitUSD: "10", Period: "day", StartsAt: &start})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("start %v: %v", start, err)
		}
	}
}

func TestGroupExpirationLongPeriodsAndDateBounds(t *testing.T) {
	end := time.Date(2026, 9, 21, 12, 0, 0, 123000, time.UTC)
	duration := 106751 * 24 * time.Hour
	expires, err := groupExpiration(end, duration, 20, 1)
	want := end.AddDate(0, 0, 19*106751)
	if err != nil || expires == nil || !expires.Equal(want) {
		t.Fatalf("long expiry = %v %v; want %v", expires, err, want)
	}
	if _, err := groupExpiration(end, duration, 99, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsupported final date: %v", err)
	}
	if expires, err := groupExpiration(end, duration, 0, 100); err != nil || expires != nil {
		t.Fatalf("unlimited = %v %v", expires, err)
	}
	if _, err := groupExpiration(end, 24*time.Hour, 2, 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("decreasing count below current: %v", err)
	}
	if expires, err := groupExpiration(end, duration, 3, 3); err != nil || expires == nil || !expires.Equal(end) {
		t.Fatalf("last cycle expires at saved end: %v %v", expires, err)
	}
	boundary := time.Date(9999, 12, 30, 23, 59, 59, 123000, time.UTC)
	if got, err := addGroupPeriods(boundary, 24*time.Hour, 1); err != nil || got.Year() != 9999 || got.Day() != 31 || got.Nanosecond() != 123000 {
		t.Fatalf("valid final date = %v %v", got, err)
	}
	if _, err := addGroupPeriods(boundary, 24*time.Hour, 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("date overflow: %v", err)
	}
}

func TestGroupPeriodAvailabilityAndFinalRetry(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	end := now.Add(24 * time.Hour)
	g := &GroupSummary{ID: "test", Period: "day", PeriodCount: 1, CurrentPeriodNumber: 1,
		RemainingUSD: "1.000000000000", PeriodStartsAt: now, PeriodEndsAt: end, ExpiresAt: &end}
	for _, at := range []time.Time{now.Add(-time.Nanosecond), end, end.Add(time.Hour)} {
		if groupPeriodActive(g, at) {
			t.Fatalf("group active outside its period at %v", at)
		}
	}
	if !groupPeriodActive(g, now) || groupHasNextPeriod(g) {
		t.Fatal("final cycle availability is incorrect")
	}
	g.RemainingUSD = "0.000000000000"
	var unavailable *GroupQuotaExceededError
	if err := requireGroupQuota(g, now); !errors.As(err, &unavailable) || unavailable.RetryAfter != 0 {
		t.Fatalf("final cycle must not promise renewal: %v", err)
	}
	g.RemainingUSD = "1.000000000000"
	if err := requireGroupQuota(g, end); !errors.As(err, &unavailable) || unavailable.RetryAfter != 0 {
		t.Fatalf("expired cycle with money remaining: %v", err)
	}
	later := end.Add(24 * time.Hour)
	g.PeriodCount, g.ExpiresAt = 2, &later
	if !groupHasNextPeriod(g) {
		t.Fatal("second configured cycle missing")
	}
	g.PeriodCount, g.ExpiresAt = 0, nil
	if !groupHasNextPeriod(g) {
		t.Fatal("unlimited renewal missing")
	}
	g.PeriodStartsAt = time.Date(9999, 12, 30, 0, 0, 0, 0, time.UTC)
	g.PeriodEndsAt = g.PeriodStartsAt.Add(24 * time.Hour)
	if groupHasNextPeriod(g) {
		t.Fatal("unlimited group must not advertise a cycle outside supported dates")
	}
	g.RemainingUSD = "0.000000000000"
	if err := requireGroupQuota(g, g.PeriodStartsAt); !errors.As(err, &unavailable) || unavailable.RetryAfter != 0 {
		t.Fatalf("unrepresentable next cycle must not promise renewal: %v", err)
	}
}

func TestDecodeLegacyGroupResponseUsesItsOwnSnapshot(t *testing.T) {
	end := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
	legacy := map[string]any{"id": "legacy", "period_id": "old-period", "period_ends_at": end, "used_usd": "3.000000000000", "members": []any{}}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var result Group
	if err := decodeGroupOperationResponse(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if result.PeriodCount != 1 || result.CurrentPeriodNumber != 1 || result.ExpiresAt == nil || !result.ExpiresAt.Equal(end) || result.PeriodID != "old-period" {
		t.Fatalf("legacy snapshot = %+v", result)
	}
	legacy["period_count"], legacy["current_period_number"], legacy["expires_at"] = 0, 4, nil
	encoded, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeGroupOperationResponse(encoded, &result); err != nil || result.PeriodCount != 0 || result.CurrentPeriodNumber != 4 || result.ExpiresAt != nil {
		t.Fatalf("explicit unlimited snapshot = %+v %v", result, err)
	}
}
