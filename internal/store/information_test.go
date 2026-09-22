package store

import (
	"errors"
	"testing"
	"time"
)

func TestInformationCutoffUTC(t *testing.T) {
	now := time.Date(2024, 3, 1, 1, 30, 0, 0, time.FixedZone("east", 8*60*60))
	got, err := InformationCutoff(now, 1)
	want := time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)
	if err != nil || !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("cutoff = %v, %v; want %v", got, err, want)
	}
	for _, days := range []int{0, -1, 2_147_483_647} {
		if _, err := InformationCutoff(now, days); !errors.Is(err, ErrInvalid) {
			t.Errorf("days %d: error = %v, want ErrInvalid", days, err)
		}
	}
}
