package limit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type windowCounter struct {
	start time.Time
	count int
}

type localReservation struct {
	identity   Identity
	window     time.Time
	keyRPM     bool
	userRPM    bool
	concurrent bool
}

// Limiter jointly admits per-key and per-user RPM/concurrency plus global
// concurrency. One mutex makes local checks and increments indivisible.
type Limiter struct {
	config Config
	clock  Clock

	mu             sync.Mutex
	keyRPM         map[string]*windowCounter
	userRPM        map[string]*windowCounter
	keyConcurrent  map[string]int
	userConcurrent map[string]int
	globalCurrent  int
}

// New validates and copies a limiter configuration.
func New(config Config) (*Limiter, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if config.Window == 0 {
		config.Window = DefaultWindow
	}
	if config.ConcurrencyRetryAfter == 0 {
		config.ConcurrencyRetryAfter = DefaultConcurrencyRetryAfter
	}
	if config.DayLocation == nil {
		config.DayLocation = time.UTC
	}
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Limiter{
		config:         config,
		clock:          clock,
		keyRPM:         make(map[string]*windowCounter),
		userRPM:        make(map[string]*windowCounter),
		keyConcurrent:  make(map[string]int),
		userConcurrent: make(map[string]int),
	}, nil
}

func validateConfig(config Config) error {
	if config.Window < 0 || config.ConcurrencyRetryAfter < 0 || config.GlobalConcurrent < 0 ||
		invalidLimits(config.Key) || invalidLimits(config.User) {
		return ErrInvalidConfig
	}
	if hasDailyLimits(config.Key) || hasDailyLimits(config.User) {
		if config.DailyStore == nil {
			return ErrDailyStoreRequired
		}
	}
	return nil
}

func invalidLimits(value Limits) bool {
	return value.RequestsPerMinute < 0 || value.Concurrent < 0 ||
		value.RequestsPerDay < 0 || value.TokensPerDay < 0
}

func hasDailyLimits(value Limits) bool {
	return value.RequestsPerDay > 0 || value.TokensPerDay > 0
}

// Acquire performs a non-blocking admission attempt. On success, the returned
// release function is idempotent. It is also invoked automatically when ctx is
// canceled, preventing leaked concurrency slots on client disconnects.
//
// A *LimitError is a normal 429-style rejection. Other errors are context or
// DailyStore failures and should not be presented as quota errors.
func (l *Limiter) Acquire(ctx context.Context, identity Identity) (ReleaseFunc, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if identity.KeyID == "" || identity.UserID == "" {
		return nil, ErrInvalidIdentity
	}

	now := l.clock.Now()
	reservation, limitErr := l.reserveLocal(identity, now)
	if limitErr != nil {
		return nil, limitErr
	}

	dailyReservations := l.dailyReservations(identity)
	if len(dailyReservations) > 0 {
		day := dayAt(now, l.config.DayLocation)
		err := l.config.DailyStore.ReserveRequests(ctx, day, dailyReservations)
		if err != nil {
			l.rollbackLocal(reservation)
			var dailyErr *DailyLimitError
			if errors.As(err, &dailyErr) {
				return nil, &LimitError{
					Scope: dailyErr.Scope, Reason: dailyErr.Reason,
					RetryAfter: untilNextDay(now, l.config.DayLocation), Limit: dailyErr.Limit,
				}
			}
			return nil, fmt.Errorf("reserve daily request quota: %w", err)
		}
	}

	release, released := l.releaseFor(identity)
	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
				release()
			case <-released:
			}
		}()
	}
	return release, nil
}

func (l *Limiter) reserveLocal(identity Identity, now time.Time) (localReservation, *LimitError) {
	window := now.Truncate(l.config.Window)
	l.mu.Lock()
	defer l.mu.Unlock()

	keyCounter := activeCounter(l.keyRPM, identity.KeyID, window)
	userCounter := activeCounter(l.userRPM, identity.UserID, window)
	windowRetry := window.Add(l.config.Window).Sub(now)
	if l.config.Key.RequestsPerMinute > 0 && keyCounter.count >= l.config.Key.RequestsPerMinute {
		return localReservation{}, &LimitError{Scope: ScopeKey, Reason: ReasonRPM, RetryAfter: windowRetry, Limit: int64(l.config.Key.RequestsPerMinute)}
	}
	if l.config.User.RequestsPerMinute > 0 && userCounter.count >= l.config.User.RequestsPerMinute {
		return localReservation{}, &LimitError{Scope: ScopeUser, Reason: ReasonRPM, RetryAfter: windowRetry, Limit: int64(l.config.User.RequestsPerMinute)}
	}
	if l.config.Key.Concurrent > 0 && l.keyConcurrent[identity.KeyID] >= l.config.Key.Concurrent {
		return localReservation{}, &LimitError{Scope: ScopeKey, Reason: ReasonConcurrency, RetryAfter: l.config.ConcurrencyRetryAfter, Limit: int64(l.config.Key.Concurrent)}
	}
	if l.config.User.Concurrent > 0 && l.userConcurrent[identity.UserID] >= l.config.User.Concurrent {
		return localReservation{}, &LimitError{Scope: ScopeUser, Reason: ReasonConcurrency, RetryAfter: l.config.ConcurrencyRetryAfter, Limit: int64(l.config.User.Concurrent)}
	}
	if l.config.GlobalConcurrent > 0 && l.globalCurrent >= l.config.GlobalConcurrent {
		return localReservation{}, &LimitError{Scope: ScopeGlobal, Reason: ReasonConcurrency, RetryAfter: l.config.ConcurrencyRetryAfter, Limit: int64(l.config.GlobalConcurrent)}
	}

	reservation := localReservation{identity: identity, window: window, concurrent: true}
	if l.config.Key.RequestsPerMinute > 0 {
		keyCounter.count++
		reservation.keyRPM = true
	}
	if l.config.User.RequestsPerMinute > 0 {
		userCounter.count++
		reservation.userRPM = true
	}
	l.keyConcurrent[identity.KeyID]++
	l.userConcurrent[identity.UserID]++
	l.globalCurrent++
	return reservation, nil
}

func activeCounter(counters map[string]*windowCounter, id string, window time.Time) *windowCounter {
	counter := counters[id]
	if counter == nil {
		counter = &windowCounter{start: window}
		counters[id] = counter
	} else if !counter.start.Equal(window) {
		counter.start = window
		counter.count = 0
	}
	return counter
}

// rollbackLocal is used only when durable admission failed. RPM increments are
// rolled back if their original window is still active; concurrency always is.
func (l *Limiter) rollbackLocal(reservation localReservation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reservation.keyRPM {
		if counter := l.keyRPM[reservation.identity.KeyID]; counter != nil && counter.start.Equal(reservation.window) && counter.count > 0 {
			counter.count--
		}
	}
	if reservation.userRPM {
		if counter := l.userRPM[reservation.identity.UserID]; counter != nil && counter.start.Equal(reservation.window) && counter.count > 0 {
			counter.count--
		}
	}
	if reservation.concurrent {
		l.releaseConcurrentLocked(reservation.identity)
	}
}

func (l *Limiter) releaseFor(identity Identity) (ReleaseFunc, <-chan struct{}) {
	var once sync.Once
	released := make(chan struct{})
	return func() {
		once.Do(func() {
			close(released)
			l.mu.Lock()
			defer l.mu.Unlock()
			l.releaseConcurrentLocked(identity)
		})
	}, released
}

func (l *Limiter) releaseConcurrentLocked(identity Identity) {
	if current := l.keyConcurrent[identity.KeyID]; current > 1 {
		l.keyConcurrent[identity.KeyID] = current - 1
	} else {
		delete(l.keyConcurrent, identity.KeyID)
	}
	if current := l.userConcurrent[identity.UserID]; current > 1 {
		l.userConcurrent[identity.UserID] = current - 1
	} else {
		delete(l.userConcurrent, identity.UserID)
	}
	if l.globalCurrent > 0 {
		l.globalCurrent--
	}
}

func (l *Limiter) dailyReservations(identity Identity) []DailyReservation {
	reservations := make([]DailyReservation, 0, 2)
	if hasDailyLimits(l.config.Key) {
		reservations = append(reservations, DailyReservation{
			Scope: ScopeKey, ID: identity.KeyID,
			RequestLimit: l.config.Key.RequestsPerDay, TokenLimit: l.config.Key.TokensPerDay,
		})
	}
	if hasDailyLimits(l.config.User) {
		reservations = append(reservations, DailyReservation{
			Scope: ScopeUser, ID: identity.UserID,
			RequestLimit: l.config.User.RequestsPerDay, TokenLimit: l.config.User.TokensPerDay,
		})
	}
	return reservations
}

// RecordTokens durably applies actual input+output token usage to both active
// daily scopes. Callers must not add cached-input or reasoning tokens a second
// time. Token overage is recorded rather than rejected; the next Acquire call
// is rejected, bounding overage by already-running requests.
func (l *Limiter) RecordTokens(ctx context.Context, identity Identity, tokens int64) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if identity.KeyID == "" || identity.UserID == "" {
		return ErrInvalidIdentity
	}
	if tokens < 0 {
		return ErrNegativeTokenCount
	}
	if tokens == 0 || l.config.DailyStore == nil {
		return nil
	}
	usage := make([]DailyTokenUsage, 0, 2)
	if hasDailyLimits(l.config.Key) {
		usage = append(usage, DailyTokenUsage{Scope: ScopeKey, ID: identity.KeyID, Tokens: tokens})
	}
	if hasDailyLimits(l.config.User) {
		usage = append(usage, DailyTokenUsage{Scope: ScopeUser, ID: identity.UserID, Tokens: tokens})
	}
	if len(usage) == 0 {
		return nil
	}
	day := dayAt(l.clock.Now(), l.config.DayLocation)
	if err := l.config.DailyStore.AddTokens(ctx, day, usage); err != nil {
		return fmt.Errorf("record daily token usage: %w", err)
	}
	return nil
}

// Snapshot returns current in-process counters without mutating admission
// state. It is intended for diagnostics and tests, not authorization.
func (l *Limiter) Snapshot(identity Identity) Snapshot {
	now := l.clock.Now()
	window := now.Truncate(l.config.Window)
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := Snapshot{
		KeyConcurrent: l.keyConcurrent[identity.KeyID], UserConcurrent: l.userConcurrent[identity.UserID],
		GlobalConcurrent: l.globalCurrent, WindowEnds: window.Add(l.config.Window),
	}
	if counter := l.keyRPM[identity.KeyID]; counter != nil && counter.start.Equal(window) {
		snapshot.KeyRPM = counter.count
	}
	if counter := l.userRPM[identity.UserID]; counter != nil && counter.start.Equal(window) {
		snapshot.UserRPM = counter.count
	}
	return snapshot
}

func dayAt(now time.Time, location *time.Location) Day {
	return Day(now.In(location).Format("2006-01-02"))
}

func untilNextDay(now time.Time, location *time.Location) time.Duration {
	local := now.In(location)
	next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
	retry := next.Sub(now)
	if retry <= 0 {
		return time.Second
	}
	return retry
}
