package store

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestBillingPlanExpiryFixedDurationAndBounds(t *testing.T) {
	start := time.Date(2026, 9, 29, 12, 0, 0, 123456000, time.FixedZone("UTC+8", 8*3600))
	for _, tc := range []struct {
		tier        string
		count, days int
	}{
		{BillingTierDay, 101, 101}, {BillingTierWeek, 99, 693}, {BillingTierMonth, 10000, 310000},
	} {
		got, err := billingPlanExpiry(start, tc.tier, tc.count)
		if err != nil || !got.Equal(start.UTC().AddDate(0, 0, tc.days)) || got.Location() != time.UTC {
			t.Errorf("expiry %s/%d = %s, %v", tc.tier, tc.count, got, err)
		}
	}
	for _, tc := range []struct {
		start time.Time
		count int
	}{
		{start, 0}, {start, -1}, {start, math.MaxInt32},
		{time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), 1},
	} {
		if _, err := billingPlanExpiry(tc.start, BillingTierDay, tc.count); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid expiry accepted: %+v, %v", tc, err)
		}
	}
}

func TestAdvanceBillingPeriodBeyondDurationRange(t *testing.T) {
	end := time.Date(2026, 9, 29, 12, 0, 0, 500, time.UTC)
	// This span exceeds time.Duration's range; nanoseconds just before a full
	// period boundary must retain the preceding period.
	at := end.AddDate(400, 0, 0).Add(-time.Nanosecond)
	start, next, number, err := advanceBillingPeriod(end, at, BillingTierDay, 1)
	if err != nil || start.After(at) || !next.After(at) || number <= 100000 || next.Sub(start) != 24*time.Hour {
		t.Fatalf("long idle advance = %s, %s, %d, %v", start, next, number, err)
	}
	if !next.Equal(end.AddDate(400, 0, 0)) {
		t.Errorf("nanosecond boundary skipped: %s", next)
	}
	if _, _, _, err := advanceBillingPeriod(end, end, BillingTierDay, math.MaxInt32); !errors.Is(err, ErrInvalid) {
		t.Fatalf("period integer overflow accepted: %v", err)
	}
}

func TestBillingPlanTotalExactAndOverflow(t *testing.T) {
	total, err := billingPlanTotal("0.123456789012", 99)
	if err != nil || total != "12.222222112188" {
		t.Fatalf("total=%q err=%v", total, err)
	}
	if _, err := billingPlanTotal("999999999999999999", 99); !errors.Is(err, ErrInvalid) {
		t.Fatalf("amount overflow accepted: %v", err)
	}
}
