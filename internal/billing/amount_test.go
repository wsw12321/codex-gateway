package billing

import (
	"errors"
	"testing"
)

func TestParseInput(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value     string
		signed    bool
		allowZero bool
		want      string
	}{
		{"1.230000", false, false, "1.23"},
		{"+2.5", true, false, "2.5"},
		{"-0.000001", true, false, "-0.000001"},
		{"0", false, true, "0"},
		{"999999999999999999.999999", false, false, "999999999999999999.999999"},
	} {
		got, err := ParseInput(test.value, test.signed, test.allowZero)
		if err != nil || got != test.want {
			t.Errorf("ParseInput(%q) = %q, %v; want %q", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{
		"", " 1", "1 ", "01", ".1", "1.", "1e3", "1.0000001", "-1",
		"1000000000000000000", "999999999999999999.9999999",
	} {
		if _, err := ParseInput(value, false, false); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParseInput(%q) error = %v", value, err)
		}
	}
	for _, value := range []string{"0", "+0", "-0", "-0.000000"} {
		if _, err := ParseInput(value, true, false); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParseInput(%q) accepted a zero value", value)
		}
	}
}

func TestParseRateAndPriceFitNumericSnapshot(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0.000000000001", "1", "999999999999999999.999999999999"} {
		if _, err := ParseRate(value); err != nil {
			t.Errorf("ParseRate(%q) error = %v", value, err)
		}
		if _, err := ParsePrice(value); err != nil {
			t.Errorf("ParsePrice(%q) error = %v", value, err)
		}
	}
	if got, err := ParsePrice("0"); err != nil || got != "0" {
		t.Fatalf("ParsePrice(0) = %q, %v", got, err)
	}
	for _, value := range []string{
		"", " 1", "+1", "-1", "01", ".1", "1.", "1e3", "1/2",
		"0.0000000000001", "1000000000000000000",
	} {
		if _, err := ParseRate(value); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParseRate(%q) error = %v", value, err)
		}
		if _, err := ParsePrice(value); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParsePrice(%q) error = %v", value, err)
		}
	}
	if _, err := ParseRate("0"); !errors.Is(err, ErrInvalidDecimal) {
		t.Fatalf("ParseRate(0) error = %v", err)
	}
}

func TestCalculateCostAndRounding(t *testing.T) {
	t.Parallel()
	cost, err := CalculateCost(1_000_000, 250_000, 100_000, "2", "0.5", "10")
	if err != nil {
		t.Fatal(err)
	}
	if cost != "2.625000000000" {
		t.Fatalf("cost = %s, want 2.625000000000", cost)
	}
	cost, err = CalculateCost(1, 0, 0, "0.0000005", "0", "0")
	if err != nil {
		t.Fatal(err)
	}
	if cost != "0.000000000001" {
		t.Fatalf("half-up cost = %s", cost)
	}
	cost, err = CalculateCost(1_000_000, 0, 0, "999999999999999999.999999999999", "0", "0")
	if err != nil {
		t.Fatal(err)
	}
	if cost != "999999999999999999.999999999999" {
		t.Fatalf("maximum cost = %s", cost)
	}
}

func TestCalculateCostRejectsInvalidInputsAndOverflow(t *testing.T) {
	t.Parallel()
	for _, counts := range [][3]int64{
		{-1, 0, 0},
		{1, -1, 0},
		{1, 0, -1},
		{1, 2, 0},
	} {
		if _, err := CalculateCost(counts[0], counts[1], counts[2], "1", "1", "1"); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("CalculateCost(%v) error = %v", counts, err)
		}
	}
	for _, price := range []string{"1e0", "1/2", "01", "-1", "0.0000000000001", "1000000000000000000"} {
		if _, err := CalculateCost(1, 0, 0, price, "0", "0"); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("CalculateCost price %q error = %v", price, err)
		}
	}
	if _, err := CalculateCost(1_000_001, 0, 0, "999999999999999999.999999999999", "0", "0"); !errors.Is(err, ErrInvalidDecimal) {
		t.Fatalf("CalculateCost overflow error = %v", err)
	}
}

func TestCalculateCostV2CacheWriteModes(t *testing.T) {
	t.Parallel()
	separate, err := CalculateCostV2(
		1_000_000, 200_000, 300_000, 100_000, "separate",
		"5", "0.5", "6.25", "30",
	)
	if err != nil {
		t.Fatal(err)
	}
	if separate != "7.475000000000" {
		t.Fatalf("separate cost = %s", separate)
	}
	included, err := CalculateCostV2(
		1_000_000, 200_000, 300_000, 100_000, "included_in_input",
		"5", "0.5", "0", "30",
	)
	if err != nil {
		t.Fatal(err)
	}
	if included != "7.100000000000" {
		t.Fatalf("included cost = %s", included)
	}
}

func TestCalculateCostV2RejectsOverlappingSeparateCategories(t *testing.T) {
	t.Parallel()
	if _, err := CalculateCostV2(100, 80, 21, 0, "separate", "1", "1", "1", "1"); !errors.Is(err, ErrInvalidDecimal) {
		t.Fatalf("overlapping categories error = %v", err)
	}
}

func TestMultiplyToAmountValidatesAndRounds(t *testing.T) {
	t.Parallel()
	got, err := MultiplyToAmount("10.123456", "0.123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.249809371464" {
		t.Fatalf("converted amount = %s", got)
	}
	got, err = MultiplyToAmount("0.000001", "0.0000005")
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.000000000001" {
		t.Fatalf("half-up converted amount = %s", got)
	}
	got, err = MultiplyToAmount("999999999999999999.999999", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "999999999999999999.999999000000" {
		t.Fatalf("maximum converted amount = %s", got)
	}
	for _, test := range [][2]string{
		{"1.0000001", "1"},
		{"1e0", "1"},
		{"0", "1"},
		{"1", "0"},
		{"1", "1.0000000000001"},
		{"999999999999999999", "2"},
	} {
		if _, err := MultiplyToAmount(test[0], test[1]); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("MultiplyToAmount(%q, %q) error = %v", test[0], test[1], err)
		}
	}
}
