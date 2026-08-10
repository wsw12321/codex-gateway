package limit

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
)

func TestMemoryDailyStoreValidationAndAtomicity(t *testing.T) {
	store := NewMemoryDailyStore()
	ctx := context.Background()
	day := Day("2026-08-10")
	if err := store.ReserveRequests(ctx, Day("2026-8-10"), nil); !errors.Is(err, ErrInvalidDay) {
		t.Fatalf("invalid day error = %v", err)
	}
	duplicate := []DailyReservation{
		{Scope: ScopeKey, ID: "key", RequestLimit: 1},
		{Scope: ScopeKey, ID: "key", RequestLimit: 1},
	}
	if err := store.ReserveRequests(ctx, day, duplicate); !errors.Is(err, ErrDuplicateDailyEntry) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := store.ReserveRequests(ctx, day, []DailyReservation{{Scope: ScopeGlobal, ID: "all"}}); !errors.Is(err, ErrInvalidDailyEntry) {
		t.Fatalf("invalid scope error = %v", err)
	}

	entries := []DailyReservation{
		{Scope: ScopeKey, ID: "key", RequestLimit: 2},
		{Scope: ScopeUser, ID: "user", RequestLimit: 1},
	}
	if err := store.ReserveRequests(ctx, day, entries); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveRequests(ctx, day, entries); err == nil {
		t.Fatal("second reservation was allowed")
	} else {
		var dailyErr *DailyLimitError
		if !errors.As(err, &dailyErr) || dailyErr.Scope != ScopeUser {
			t.Fatalf("daily error = %#v, %v", dailyErr, err)
		}
	}
	keyUsage, _ := store.Usage(ctx, day, ScopeKey, "key")
	if keyUsage.Requests != 1 {
		t.Fatalf("failed transaction partially incremented key: %#v", keyUsage)
	}
}

func TestMemoryDailyStoreTokenOverflowIsAtomic(t *testing.T) {
	store := NewMemoryDailyStore()
	day := Day("2026-08-10")
	store.usage[dailyKey{day: day, scope: ScopeUser, id: "user"}] = DailyUsage{Tokens: math.MaxInt64}
	err := store.AddTokens(context.Background(), day, []DailyTokenUsage{
		{Scope: ScopeKey, ID: "key", Tokens: 1},
		{Scope: ScopeUser, ID: "user", Tokens: 1},
	})
	if !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("overflow error = %v", err)
	}
	usage, _ := store.Usage(context.Background(), day, ScopeKey, "key")
	if usage.Tokens != 0 {
		t.Fatalf("overflow partially added tokens: %#v", usage)
	}
}

func TestMemoryDailyStoreIsRaceSafe(t *testing.T) {
	store := NewMemoryDailyStore()
	day := Day("2026-08-10")
	const workers = 32
	const increments = 100
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < increments; j++ {
				if err := store.AddTokens(context.Background(), day, []DailyTokenUsage{{Scope: ScopeKey, ID: "key", Tokens: 1}}); err != nil {
					t.Errorf("AddTokens: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	usage, err := store.Usage(context.Background(), day, ScopeKey, "key")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Tokens != workers*increments {
		t.Fatalf("tokens = %d", usage.Tokens)
	}
}
