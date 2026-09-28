package billing

import (
	"errors"
	"testing"
)

func TestParseMultiplier(t *testing.T) {
	for input, want := range map[string]string{
		"1.0": "1", "0.125000000000": "0.125", "2.5": "2.5",
		"0.000000000001": "0.000000000001", "999999999999999999.999999999999": "999999999999999999.999999999999",
	} {
		if got, err := ParseMultiplier(input); err != nil || got != want {
			t.Errorf("ParseMultiplier(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "0.000000000000", "-1", "+1", "01", "1.", ".5", " 1", "1 ", "1e1", "1/2", "NaN", "Infinity", "1000000000000000000", "0.0000000000001"} {
		if _, err := ParseMultiplier(input); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParseMultiplier(%q) error = %v", input, err)
		}
	}
}

func TestCalculateCostMultiplierSingleRounding(t *testing.T) {
	for _, tt := range []struct {
		name, price, multiplier, want string
	}{
		{"default", "2", "1", "0.000002000000"},
		{"discount", "2", "0.125", "0.000000250000"},
		{"markup", "2", "2.5", "0.000005000000"},
		// Rounding the base first would produce 0 for the markup and 1e-12
		// for the discount; both results would be incorrect.
		{"markup before rounding", "0.0000004", "2", "0.000000000001"},
		{"discount before rounding", "0.0000005", "0.5", "0.000000000000"},
		{"half away from zero", "0.00000025", "2", "0.000000000001"},
		{"zero price", "0", "999999999999999999.999999999999", "0.000000000000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CalculateCostWithMultiplier(1, 0, 0, tt.price, "0", "0", tt.multiplier)
			if err != nil || got != tt.want {
				t.Fatalf("v1 = %q, %v; want %q", got, err, tt.want)
			}
			for _, mode := range []string{"separate", "included_in_input"} {
				got, err := CalculateCostV2WithMultiplier(1, 0, 0, 0, mode, tt.price, "0", "0", "0", tt.multiplier)
				if err != nil || got != tt.want {
					t.Fatalf("v2 %s = %q, %v; want %q", mode, got, err, tt.want)
				}
			}
		})
	}
}

func TestCalculateCostMultiplierCategoriesAndBounds(t *testing.T) {
	got, err := CalculateCostWithMultiplier(1_000_000, 250_000, 100_000, "2", "0.5", "10", "0.4")
	if err != nil || got != "1.050000000000" {
		t.Fatalf("v1 categories = %q, %v", got, err)
	}
	for _, tt := range []struct{ mode, want string }{
		{"separate", "18.687500000000"}, {"included_in_input", "17.750000000000"},
	} {
		got, err := CalculateCostV2WithMultiplier(1_000_000, 200_000, 300_000, 100_000, tt.mode, "5", "0.5", "6.25", "30", "2.5")
		if err != nil || got != tt.want {
			t.Fatalf("v2 %s categories = %q, %v", tt.mode, got, err)
		}
	}
	for _, factor := range []string{"0", "-1", "1e1", "0.0000000000001", "2"} {
		_, err := CalculateCostWithMultiplier(1_000_000, 0, 0, "999999999999999999", "0", "0", factor)
		if !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("v1 invalid/overflow factor %s = %v", factor, err)
		}
		_, err = CalculateCostV2WithMultiplier(1_000_000, 0, 0, 0, "separate", "999999999999999999", "0", "0", "0", factor)
		if !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("v2 invalid/overflow factor %s = %v", factor, err)
		}
	}
	// An unrounded base larger than the column limit is legal when the final
	// discounted amount fits: there is no intermediate amount column/rounding.
	got, err = CalculateCostWithMultiplier(2_000_000, 0, 0, "999999999999999999", "0", "0", "0.5")
	if err != nil || got != "999999999999999999.000000000000" {
		t.Fatalf("discounted large base = %q, %v", got, err)
	}
}
