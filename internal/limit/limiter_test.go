package limit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(value time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(value)
	c.mu.Unlock()
}

func testTime() time.Time {
	return time.Date(2026, time.August, 10, 12, 0, 30, 250_000_000, time.UTC)
}

func mustLimiter(t *testing.T, config Config) *Limiter {
	t.Helper()
	limiter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func requireLimitError(t *testing.T, err error, scope Scope, reason Reason) *LimitError {
	t.Helper()
	var limitErr *LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error %v is not a LimitError", err)
	}
	if limitErr.Scope != scope || limitErr.Reason != reason {
		t.Fatalf("limit error = %#v, want %s/%s", limitErr, scope, reason)
	}
	if limitErr.RetryAfter <= 0 || limitErr.RetryAfterSeconds() <= 0 {
		t.Fatalf("invalid Retry-After: %#v", limitErr)
	}
	return limitErr
}

func TestFixedWindowRPMIsJointAcrossKeyAndUser(t *testing.T) {
	clock := &fakeClock{now: testTime()}
	limiter := mustLimiter(t, Config{
		Key: Limits{RequestsPerMinute: 2}, User: Limits{RequestsPerMinute: 3}, Clock: clock,
	})
	keyA := Identity{KeyID: "key-a", UserID: "user-a"}
	for i := 0; i < 2; i++ {
		release, err := limiter.Acquire(context.Background(), keyA)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	_, err := limiter.Acquire(context.Background(), keyA)
	limitErr := requireLimitError(t, err, ScopeKey, ReasonRPM)
	if limitErr.RetryAfter != 29_750*time.Millisecond || limitErr.RetryAfterSeconds() != 30 {
		t.Fatalf("Retry-After = %v / %d", limitErr.RetryAfter, limitErr.RetryAfterSeconds())
	}

	// A different key has capacity, but shares the user's third and final slot.
	keyB := Identity{KeyID: "key-b", UserID: "user-a"}
	release, err := limiter.Acquire(context.Background(), keyB)
	if err != nil {
		t.Fatal(err)
	}
	release()
	keyC := Identity{KeyID: "key-c", UserID: "user-a"}
	_, err = limiter.Acquire(context.Background(), keyC)
	requireLimitError(t, err, ScopeUser, ReasonRPM)
	// The failed joint admission must not consume key-c's own RPM counter.
	if got := limiter.Snapshot(keyC).KeyRPM; got != 0 {
		t.Fatalf("partially consumed key RPM = %d", got)
	}

	clock.Advance(30 * time.Second)
	release, err = limiter.Acquire(context.Background(), keyA)
	if err != nil {
		t.Fatalf("new fixed window remained blocked: %v", err)
	}
	release()
}

func TestConcurrencyLimitsAndIdempotentRelease(t *testing.T) {
	clock := &fakeClock{now: testTime()}
	limiter := mustLimiter(t, Config{
		Key: Limits{Concurrent: 1}, User: Limits{Concurrent: 2}, GlobalConcurrent: 2, Clock: clock,
	})
	a := Identity{KeyID: "a", UserID: "user-1"}
	b := Identity{KeyID: "b", UserID: "user-1"}
	c := Identity{KeyID: "c", UserID: "user-2"}
	releaseA, err := limiter.Acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Acquire(context.Background(), a); err == nil {
		t.Fatal("per-key concurrency was not enforced")
	} else {
		requireLimitError(t, err, ScopeKey, ReasonConcurrency)
	}
	releaseB, err := limiter.Acquire(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Acquire(context.Background(), c); err == nil {
		t.Fatal("global concurrency was not enforced")
	} else {
		requireLimitError(t, err, ScopeGlobal, ReasonConcurrency)
	}

	releaseA()
	releaseA()
	releaseA()
	releaseC, err := limiter.Acquire(context.Background(), c)
	if err != nil {
		t.Fatalf("idempotent release did not free exactly one slot: %v", err)
	}
	releaseB()
	releaseC()
	got := limiter.Snapshot(a)
	if got.KeyConcurrent != 0 || got.UserConcurrent != 0 || got.GlobalConcurrent != 0 {
		t.Fatalf("concurrency leaked: %#v", got)
	}
}

func TestContextCancellationAutomaticallyReleases(t *testing.T) {
	limiter := mustLimiter(t, Config{Key: Limits{Concurrent: 1}})
	identity := Identity{KeyID: "key", UserID: "user"}
	ctx, cancel := context.WithCancel(context.Background())
	release, err := limiter.Acquire(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if got := limiter.Snapshot(identity).KeyConcurrent; got != 1 {
		t.Fatalf("concurrency = %d", got)
	}
	cancel()
	eventually(t, func() bool { return limiter.Snapshot(identity).KeyConcurrent == 0 })
	// A release racing with cancellation is still safe and idempotent.
	release()
	release()

	canceled, cancelImmediately := context.WithCancel(context.Background())
	cancelImmediately()
	if _, err := limiter.Acquire(canceled, identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled acquire error = %v", err)
	}
	if got := limiter.Snapshot(identity); got.KeyRPM != 0 || got.KeyConcurrent != 0 {
		t.Fatalf("pre-canceled acquire mutated counters: %#v", got)
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDailyRequestLimitsAreDurableAndAtomic(t *testing.T) {
	clock := &fakeClock{now: testTime()}
	store := NewMemoryDailyStore()
	config := Config{
		Key: Limits{RequestsPerDay: 2}, User: Limits{RequestsPerDay: 3},
		DailyStore: store, Clock: clock,
	}
	identity := Identity{KeyID: "key-a", UserID: "user"}
	limiter := mustLimiter(t, config)
	for i := 0; i < 2; i++ {
		release, err := limiter.Acquire(context.Background(), identity)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	_, err := limiter.Acquire(context.Background(), identity)
	limitErr := requireLimitError(t, err, ScopeKey, ReasonDailyRequests)
	if limitErr.RetryAfter != 11*time.Hour+59*time.Minute+29*time.Second+750*time.Millisecond {
		t.Fatalf("daily Retry-After = %v", limitErr.RetryAfter)
	}
	if got := limiter.Snapshot(identity); got.KeyConcurrent != 0 || got.GlobalConcurrent != 0 {
		t.Fatalf("daily rejection leaked local capacity: %#v", got)
	}

	// A new process-local limiter sees the same durable counters.
	restarted := mustLimiter(t, config)
	if _, err := restarted.Acquire(context.Background(), identity); err == nil {
		t.Fatal("daily quota reset after limiter restart")
	} else {
		requireLimitError(t, err, ScopeKey, ReasonDailyRequests)
	}

	// Exhaust the user's final slot with another key. A subsequent failed joint
	// reservation must not increment its key side.
	other := Identity{KeyID: "key-b", UserID: "user"}
	release, err := restarted.Acquire(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	release()
	blocked := Identity{KeyID: "key-c", UserID: "user"}
	if _, err := restarted.Acquire(context.Background(), blocked); err == nil {
		t.Fatal("aggregate user quota was not enforced")
	} else {
		requireLimitError(t, err, ScopeUser, ReasonDailyRequests)
	}
	usage, err := store.Usage(context.Background(), Day("2026-08-10"), ScopeKey, blocked.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (DailyUsage{}) {
		t.Fatalf("failed reservation partially mutated key: %#v", usage)
	}
}

type failingDailyStore struct {
	calls atomic.Int64
}

func (s *failingDailyStore) ReserveRequests(context.Context, Day, []DailyReservation) error {
	s.calls.Add(1)
	return errors.New("database unavailable")
}
func (*failingDailyStore) Usage(context.Context, Day, Scope, string) (DailyUsage, error) {
	return DailyUsage{}, nil
}

func TestDailyStoreFailureRollsBackLocalAdmission(t *testing.T) {
	store := &failingDailyStore{}
	limiter := mustLimiter(t, Config{
		Key: Limits{RequestsPerMinute: 1, Concurrent: 1, RequestsPerDay: 1}, DailyStore: store,
	})
	identity := Identity{KeyID: "key", UserID: "user"}
	for i := 0; i < 2; i++ {
		if _, err := limiter.Acquire(context.Background(), identity); err == nil || errors.As(err, new(*LimitError)) {
			t.Fatalf("Acquire error = %v", err)
		}
		if got := limiter.Snapshot(identity); got.KeyRPM != 0 || got.KeyConcurrent != 0 || got.GlobalConcurrent != 0 {
			t.Fatalf("failed store left local reservation: %#v", got)
		}
	}
	if store.calls.Load() != 2 {
		t.Fatalf("store calls = %d", store.calls.Load())
	}
}

type cancelStore struct{}

func (*cancelStore) ReserveRequests(ctx context.Context, _ Day, _ []DailyReservation) error {
	<-ctx.Done()
	return ctx.Err()
}
func (*cancelStore) Usage(context.Context, Day, Scope, string) (DailyUsage, error) {
	return DailyUsage{}, nil
}

func TestCancellationDuringDailyReservationRollsBack(t *testing.T) {
	limiter := mustLimiter(t, Config{
		Key: Limits{RequestsPerMinute: 1, Concurrent: 1, RequestsPerDay: 1}, DailyStore: &cancelStore{},
	})
	identity := Identity{KeyID: "key", UserID: "user"}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := limiter.Acquire(ctx, identity)
		result <- err
	}()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error = %v", err)
	}
	if got := limiter.Snapshot(identity); got.KeyRPM != 0 || got.KeyConcurrent != 0 || got.GlobalConcurrent != 0 {
		t.Fatalf("canceled reservation left counters: %#v", got)
	}
}

func TestConcurrentAcquireNeverExceedsConfiguredCapacity(t *testing.T) {
	limiter := mustLimiter(t, Config{
		Key: Limits{Concurrent: 4}, User: Limits{Concurrent: 8}, GlobalConcurrent: 12,
	})
	var maximum atomic.Int64
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 200; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			identity := Identity{KeyID: "key-" + string(rune('a'+i%10)), UserID: "user"}
			release, err := limiter.Acquire(context.Background(), identity)
			if err != nil {
				return
			}
			current := int64(limiter.Snapshot(identity).GlobalConcurrent)
			for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
			}
			time.Sleep(time.Microsecond)
			release()
		}(i)
	}
	close(start)
	wait.Wait()
	if maximum.Load() > 12 {
		t.Fatalf("global concurrency reached %d", maximum.Load())
	}
	if got := limiter.Snapshot(Identity{KeyID: "key-a", UserID: "user"}); got.GlobalConcurrent != 0 {
		t.Fatalf("concurrency leaked: %#v", got)
	}
}

func TestConfigValidationAndRetryRounding(t *testing.T) {
	if _, err := New(DefaultConfig(nil)); !errors.Is(err, ErrDailyStoreRequired) {
		t.Fatalf("nil daily store error = %v", err)
	}
	if _, err := New(Config{Key: Limits{Concurrent: -1}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative limit error = %v", err)
	}
	if got := retryAfterSeconds(time.Nanosecond); got != 1 {
		t.Fatalf("nanosecond retry = %d", got)
	}
	if got := retryAfterSeconds(1500 * time.Millisecond); got != 2 {
		t.Fatalf("fractional retry = %d", got)
	}
}
