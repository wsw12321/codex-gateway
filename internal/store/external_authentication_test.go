package store

import (
	"errors"
	"testing"
	"time"
)

func TestExternalVerificationTime(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		proof time.Time
		valid bool
	}{
		{"fresh", at, true},
		{"delayed creation keeps original proof", at.Add(-4 * time.Minute), true},
		{"last microsecond", at.Add(-5*time.Minute + time.Microsecond), true},
		{"boundary expired", at.Add(-5 * time.Minute), false},
		{"expired", at.Add(-6 * time.Minute), false},
		{"future", at.Add(time.Second), false},
		{"missing", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := externalVerificationTime(tc.proof, at)
			if tc.valid {
				if err != nil || !got.Equal(tc.proof) {
					t.Fatalf("proof=%v err=%v", got, err)
				}
			} else if !errors.Is(err, ErrNotFound) || !got.IsZero() {
				t.Fatalf("invalid proof=%v err=%v", got, err)
			}
		})
	}
}

func TestExternalLoginVerificationTime(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{0, 5 * time.Minute, 6 * time.Minute, 10*time.Minute - time.Microsecond} {
		proof := at.Add(-age)
		got, err := externalLoginVerificationTime(proof, at)
		if err != nil || !got.Equal(proof) {
			t.Fatalf("age %v: original proof changed or rejected: %v %v", age, got, err)
		}
	}
	for _, proof := range []time.Time{time.Time{}, at.Add(time.Second), at.Add(-10 * time.Minute), at.Add(-11 * time.Minute)} {
		if got, err := externalLoginVerificationTime(proof, at); !errors.Is(err, ErrNotFound) || !got.IsZero() {
			t.Fatalf("invalid login proof accepted: %v %v", got, err)
		}
	}
}
