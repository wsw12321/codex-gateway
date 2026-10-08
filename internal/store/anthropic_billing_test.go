package store

import (
	"errors"
	"math"
	"testing"
)

func TestValidateCacheWriteTTL(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		total, five, hour               int64
		totalPresent, ttlPresent, valid bool
	}{
		{"legacy", 0, 0, 0, false, false, true},
		{"unknown", 500, 0, 0, true, false, true},
		{"observed zero", 0, 0, 0, true, true, true},
		{"mixed", 500, 300, 200, true, true, true},
		{"missing aggregate", 500, 300, 200, false, true, false},
		{"partial", 500, 300, 0, true, true, false},
		{"contradictory", 500, 300, 201, true, true, false},
		{"unobserved nonzero", 500, 300, 200, true, false, false},
		{"overflow", 500, math.MaxInt64, math.MaxInt64, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCacheWriteTTL(tc.total, tc.five, tc.hour, tc.totalPresent, tc.ttlPresent)
			if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrInvalid) {
				t.Fatalf("validation = %v, valid=%v", err, tc.valid)
			}
		})
	}
}
