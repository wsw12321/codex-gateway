package limit

import (
	"context"
	"testing"
	"time"
)

func FuzzLimiterInvariants(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 1_000 {
			operations = operations[:1_000]
		}
		clock := &fakeClock{now: testTime()}
		limiter, err := New(Config{
			Key:              Limits{RequestsPerMinute: 3, Concurrent: 2},
			User:             Limits{RequestsPerMinute: 5, Concurrent: 3},
			GlobalConcurrent: 4, Clock: clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		identity := Identity{KeyID: "key", UserID: "user"}
		releases := make([]ReleaseFunc, 0, 2)
		for _, operation := range operations {
			switch operation % 3 {
			case 0:
				release, err := limiter.Acquire(context.Background(), identity)
				if err == nil {
					releases = append(releases, release)
				}
			case 1:
				if len(releases) > 0 {
					releases[0]()
					releases = releases[1:]
				}
			case 2:
				clock.Advance(time.Minute)
			}
			snapshot := limiter.Snapshot(identity)
			if snapshot.KeyConcurrent < 0 || snapshot.KeyConcurrent > 2 ||
				snapshot.UserConcurrent < 0 || snapshot.UserConcurrent > 3 ||
				snapshot.GlobalConcurrent < 0 || snapshot.GlobalConcurrent > 4 ||
				snapshot.KeyRPM < 0 || snapshot.KeyRPM > 3 || snapshot.UserRPM < 0 || snapshot.UserRPM > 5 {
				t.Fatalf("invariant failed: %#v", snapshot)
			}
		}
		for _, release := range releases {
			release()
		}
		if got := limiter.Snapshot(identity).GlobalConcurrent; got != 0 {
			t.Fatalf("global concurrency leaked: %d", got)
		}
	})
}

func FuzzMemoryDailyStore(f *testing.F) {
	f.Add(uint8(2), uint8(3))
	f.Fuzz(func(t *testing.T, requestLimit uint8, attempts uint8) {
		limit := int64(requestLimit%32 + 1)
		count := int(attempts % 64)
		store := NewMemoryDailyStore()
		entry := []DailyReservation{{Scope: ScopeKey, ID: "key", RequestLimit: limit}}
		allowed := int64(0)
		for i := 0; i < count; i++ {
			if err := store.ReserveRequests(context.Background(), Day("2026-08-10"), entry); err == nil {
				allowed++
			}
		}
		usage, err := store.Usage(context.Background(), Day("2026-08-10"), ScopeKey, "key")
		if err != nil {
			t.Fatal(err)
		}
		if usage.Requests != allowed || usage.Requests > limit {
			t.Fatalf("usage=%d allowed=%d limit=%d", usage.Requests, allowed, limit)
		}
	})
}
