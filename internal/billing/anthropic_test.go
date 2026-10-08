package billing

import (
	"math"
	"testing"
)

func TestCalculateCostV2TTLWithMultiplier(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		fiveMinute, oneHour                    int64
		observed                               bool
		fivePrice, hourPrice, multiplier, want string
	}{
		{"mixed TTLs", 300, 200, true, "3.75", "6", "1", "0.004755000000"},
		{"only five minute", 500, 0, true, "3.75", "6", "1", "0.004305000000"},
		{"only one hour", 0, 500, true, "3.75", "6", "1", "0.005430000000"},
		{"missing breakdown", 0, 0, false, "3.75", "6", "1", "0.005430000000"},
		{"five minute price higher", 0, 0, false, "9", "6", "1", "0.006930000000"},
		{"multiplier", 300, 200, true, "3.75", "6", "1.5", "0.007132500000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 1,000 total input = 400 ordinary + 100 reads + 500 writes.
			got, err := CalculateCostV2TTLWithMultiplier(1000, 100, 500, tc.fiveMinute, tc.oneHour, 80,
				tc.observed, "3", "0.3", tc.fivePrice, tc.hourPrice, "15", tc.multiplier)
			if err != nil || got != tc.want {
				t.Fatalf("cost = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestCalculateCostV2TTLRejectsUntrustedBreakdown(t *testing.T) {
	for _, tc := range []struct {
		total, five, hour int64
		present           bool
	}{
		{500, 300, 201, true}, {500, 300, 199, true}, {500, 1, 0, false},
		{500, -1, 501, true}, {500, math.MaxInt64, math.MaxInt64, true},
	} {
		if got, err := CalculateCostV2TTLWithMultiplier(1000, 100, tc.total, tc.five, tc.hour, 80,
			tc.present, "3", "0.3", "3.75", "6", "15", "1"); err == nil {
			t.Fatalf("accepted invalid breakdown %+v: %s", tc, got)
		}
	}
}

func TestCalculateCostV2TTLMultipliesBeforeRounding(t *testing.T) {
	got, err := CalculateCostV2TTLWithMultiplier(1, 0, 1, 1, 0, 0, true,
		"0", "0", "0.0000003", "0.0000003", "0", "2")
	if err != nil || got != "0.000000000001" {
		t.Fatalf("cost = %q, %v", got, err)
	}
}
