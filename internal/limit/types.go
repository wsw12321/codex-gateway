// Package limit implements the gateway's in-process RPM and concurrency
// admission controls. Daily counters are delegated to a transactional store so
// they survive process restarts.
package limit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	DefaultWindow                = time.Minute
	DefaultConcurrencyRetryAfter = time.Second
)

var (
	ErrInvalidConfig      = errors.New("invalid limiter configuration")
	ErrInvalidIdentity    = errors.New("key and user IDs are required")
	ErrDailyStoreRequired = errors.New("daily limits require a persistent DailyStore")
	ErrNegativeTokenCount = errors.New("token count cannot be negative")
	ErrNilContext         = errors.New("nil context")
)

// Scope identifies the quota owner.
type Scope string

const (
	ScopeKey    Scope = "key"
	ScopeUser   Scope = "user"
	ScopeGlobal Scope = "global"
)

// Reason is stable and can be mapped directly to an API error code or alert.
type Reason string

const (
	ReasonRPM           Reason = "requests_per_minute"
	ReasonConcurrency   Reason = "concurrency"
	ReasonDailyRequests Reason = "daily_requests"
	ReasonDailyTokens   Reason = "daily_tokens"
)

// Identity is the authenticated key/user pair being admitted.
type Identity struct {
	KeyID  string
	UserID string
}

// Limits configures one key or user scope. Zero disables a limit.
type Limits struct {
	RequestsPerMinute int
	Concurrent        int
	RequestsPerDay    int64
	TokensPerDay      int64
}

// Clock makes boundary behavior deterministic in tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Config configures one Limiter. DailyStore must provide durable transactional
// counters whenever a daily limit is non-zero.
type Config struct {
	Key              Limits
	User             Limits
	GlobalConcurrent int

	// Window defaults to one minute. A different value is useful for tests;
	// production RPM limits should retain the default.
	Window time.Duration
	// ConcurrencyRetryAfter defaults to one second because the actual release
	// time of another request is unknowable.
	ConcurrencyRetryAfter time.Duration
	// DayLocation selects quota day boundaries and defaults to UTC.
	DayLocation *time.Location
	DailyStore  DailyStore
	Clock       Clock
}

// DefaultConfig returns the limits from the deployment plan. Passing nil is
// allowed here for configuration assembly, but New will reject it because the
// returned configuration contains daily limits.
func DefaultConfig(store DailyStore) Config {
	return Config{
		Key: Limits{
			RequestsPerMinute: 30,
			Concurrent:        4,
			RequestsPerDay:    1_000,
			TokensPerDay:      20_000_000,
		},
		User: Limits{
			RequestsPerMinute: 60,
			Concurrent:        8,
			RequestsPerDay:    2_000,
			TokensPerDay:      40_000_000,
		},
		GlobalConcurrent: 12,
		DailyStore:       store,
	}
}

// LimitError is a normal admission rejection. RetryAfter is always positive.
type LimitError struct {
	Scope      Scope
	Reason     Reason
	RetryAfter time.Duration
	Limit      int64
}

func (e *LimitError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s %s limit exceeded", e.Scope, e.Reason)
}

// RetryAfterSeconds returns a positive, ceiling-rounded value suitable for the
// HTTP Retry-After header.
func (e *LimitError) RetryAfterSeconds() int {
	if e == nil {
		return 0
	}
	return retryAfterSeconds(e.RetryAfter)
}

func retryAfterSeconds(value time.Duration) int {
	if value <= 0 {
		return 1
	}
	seconds := value / time.Second
	if value%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return int(seconds)
}

// ReleaseFunc releases concurrency exactly once. It is safe to call from
// multiple goroutines and safe to call after the request context is canceled.
type ReleaseFunc func()

// Snapshot is an instantaneous diagnostic view. RPM counts are for the active
// fixed window only.
type Snapshot struct {
	KeyRPM           int
	UserRPM          int
	KeyConcurrent    int
	UserConcurrent   int
	GlobalConcurrent int
	WindowEnds       time.Time
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	return ctx.Err()
}
