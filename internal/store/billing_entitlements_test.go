package store

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestBillingCustomPeriodAndDateBounds(t *testing.T) {
	for _, tc := range []struct {
		tier       string
		days, want int
	}{{BillingTierDay, 0, 1}, {BillingTierWeek, 7, 7}, {BillingTierMonth, 0, 31}, {BillingTierCustom, 1, 1}, {BillingTierCustom, 3650, 3650}} {
		got, err := normalizeBillingPeriodDays(tc.tier, tc.days)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %d %v", tc, got, err)
		}
	}
	for _, tc := range []struct {
		tier string
		days int
	}{{BillingTierCustom, 0}, {BillingTierCustom, 3651}, {BillingTierMonth, 30}, {"year", 365}} {
		if _, err := normalizeBillingPeriodDays(tc.tier, tc.days); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid period accepted: %+v %v", tc, err)
		}
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 123, time.FixedZone("CST", 8*3600))
	end, err := billingExpiryDays(start, 3650, 99)
	if err != nil || end.Location() != time.UTC || !end.Equal(start.UTC().AddDate(0, 0, 3650*99)) {
		t.Fatalf("large exact expiry: %s %v", end, err)
	}
	for _, count := range []int{0, -1, math.MaxInt32} {
		if _, err := billingExpiryDays(start, 3650, count); !errors.Is(err, ErrInvalid) {
			t.Fatalf("count %d: %v", count, err)
		}
	}
	if _, err := billingExpiryDays(time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), 1, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsupported JSON date accepted: %v", err)
	}
}
func TestBillingModelScopeRequiresExplicitSelection(t *testing.T) {
	for _, models := range [][]string{nil, {}, {""}, {"invalid/model"}} {
		if _, err := normalizeBillingModels(models); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid scope accepted: %#v %v", models, err)
		}
	}
	got, err := normalizeBillingModels([]string{"gemini-test", "gpt-test", "gpt-test"})
	if err != nil || len(got) != 2 || got[0] != "gemini-test" {
		t.Fatalf("scope normalization: %v %v", got, err)
	}
}
