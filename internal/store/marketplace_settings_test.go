package store

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeCommissionRate(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"0": "0", "0.0": "0", "0.05": "0.05", "0.050000": "0.05", "1": "1", "1.000000": "1", "0.123456": "0.123456",
	} {
		if got, err := NormalizeCommissionRate(input); err != nil || got != want {
			t.Errorf("NormalizeCommissionRate(%q)=%q,%v want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", " 0.1", "-0", "-0.1", "+0.1", "1.000001", "2", "0.1234567", "1e-2", ".5", "abc"} {
		if _, err := NormalizeCommissionRate(input); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeCommissionRate(%q) err=%v, want invalid", input, err)
		}
	}
}

func TestPlatformAccountPredicate(t *testing.T) {
	t.Parallel()
	got := platformAccountSQL("a")
	for _, want := range []string{"a.id IS NULL", "a.owner_user_id IS NULL", "NOT a.admin_suspended", "a.credential_removed_at IS NULL", "marketplace_settings"} {
		if !strings.Contains(got, want) {
			t.Fatalf("platform predicate %q lacks %q", got, want)
		}
	}
}
