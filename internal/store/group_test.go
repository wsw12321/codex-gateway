package store

import (
	"errors"
	"testing"
	"time"
)

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
	g := &GroupSummary{ID: "test", RemainingUSD: "0.000000000000", PeriodStartsAt: now, PeriodEndsAt: now.Add(24 * time.Hour)}
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
