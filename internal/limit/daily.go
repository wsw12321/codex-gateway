package limit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

var (
	ErrInvalidDay          = errors.New("day must use YYYY-MM-DD format")
	ErrInvalidDailyEntry   = errors.New("invalid daily counter entry")
	ErrDuplicateDailyEntry = errors.New("duplicate daily counter entry")
	ErrCounterOverflow     = errors.New("daily counter overflow")
)

// Day is the quota bucket selected by Limiter's configured time zone.
type Day string

// DailyReservation asks a store to atomically admit one request. RequestLimit
// and TokenLimit are checked against current durable usage; zero disables that
// check. The request count is incremented only if every reservation is allowed.
type DailyReservation struct {
	Scope        Scope
	ID           string
	RequestLimit int64
	TokenLimit   int64
}

// DailyTokenUsage is actual post-response token usage. AddTokens must always
// record it, including the bounded overage created by already-running requests.
type DailyTokenUsage struct {
	Scope  Scope
	ID     string
	Tokens int64
}

// DailyUsage is durable usage for one day and scope.
type DailyUsage struct {
	Requests int64
	Tokens   int64
}

// DailyLimitError is returned by DailyStore.ReserveRequests when no mutation
// was made because one reservation was over quota.
type DailyLimitError struct {
	Scope   Scope
	ID      string
	Reason  Reason
	Current int64
	Limit   int64
}

func (e *DailyLimitError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s %s daily limit exceeded", e.Scope, e.ID)
}

// DailyStore is the persistence seam for restart-safe quotas. Implementations
// must make each ReserveRequests call transactional: either all scope counters
// increment once, or none do. AddTokens must likewise apply all increments or
// none. A PostgreSQL implementation should use a transaction and row locks (or
// equivalent atomic UPSERT logic).
type DailyStore interface {
	ReserveRequests(ctx context.Context, day Day, reservations []DailyReservation) error
	AddTokens(ctx context.Context, day Day, usage []DailyTokenUsage) error
	Usage(ctx context.Context, day Day, scope Scope, id string) (DailyUsage, error)
}

type dailyKey struct {
	day   Day
	scope Scope
	id    string
}

// MemoryDailyStore is a race-safe reference implementation for tests and
// single-process development. Production must use a durable implementation.
type MemoryDailyStore struct {
	mu    sync.Mutex
	usage map[dailyKey]DailyUsage
}

func NewMemoryDailyStore() *MemoryDailyStore {
	return &MemoryDailyStore{usage: make(map[dailyKey]DailyUsage)}
}

func (s *MemoryDailyStore) ReserveRequests(ctx context.Context, day Day, reservations []DailyReservation) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if err := validateDay(day); err != nil {
		return err
	}
	if err := validateReservations(reservations); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.usage == nil {
		s.usage = make(map[dailyKey]DailyUsage)
	}

	for _, reservation := range reservations {
		key := dailyKey{day: day, scope: reservation.Scope, id: reservation.ID}
		current := s.usage[key]
		if reservation.RequestLimit > 0 && current.Requests >= reservation.RequestLimit {
			return &DailyLimitError{
				Scope: reservation.Scope, ID: reservation.ID, Reason: ReasonDailyRequests,
				Current: current.Requests, Limit: reservation.RequestLimit,
			}
		}
		if reservation.TokenLimit > 0 && current.Tokens >= reservation.TokenLimit {
			return &DailyLimitError{
				Scope: reservation.Scope, ID: reservation.ID, Reason: ReasonDailyTokens,
				Current: current.Tokens, Limit: reservation.TokenLimit,
			}
		}
		if current.Requests == math.MaxInt64 {
			return ErrCounterOverflow
		}
	}
	for _, reservation := range reservations {
		key := dailyKey{day: day, scope: reservation.Scope, id: reservation.ID}
		current := s.usage[key]
		current.Requests++
		s.usage[key] = current
	}
	return nil
}

func (s *MemoryDailyStore) AddTokens(ctx context.Context, day Day, usage []DailyTokenUsage) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if err := validateDay(day); err != nil {
		return err
	}
	if err := validateTokenUsage(usage); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.usage == nil {
		s.usage = make(map[dailyKey]DailyUsage)
	}
	for _, increment := range usage {
		key := dailyKey{day: day, scope: increment.Scope, id: increment.ID}
		current := s.usage[key]
		if increment.Tokens > math.MaxInt64-current.Tokens {
			return ErrCounterOverflow
		}
	}
	for _, increment := range usage {
		key := dailyKey{day: day, scope: increment.Scope, id: increment.ID}
		current := s.usage[key]
		current.Tokens += increment.Tokens
		s.usage[key] = current
	}
	return nil
}

func (s *MemoryDailyStore) Usage(ctx context.Context, day Day, scope Scope, id string) (DailyUsage, error) {
	if err := validateContext(ctx); err != nil {
		return DailyUsage{}, err
	}
	if err := validateDay(day); err != nil {
		return DailyUsage{}, err
	}
	if !validDailyScope(scope) || id == "" {
		return DailyUsage{}, ErrInvalidDailyEntry
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return DailyUsage{}, err
	}
	return s.usage[dailyKey{day: day, scope: scope, id: id}], nil
}

// DeleteBefore removes old in-memory buckets. It is intentionally not part of
// DailyStore because durable retention is owned by the database cleanup job.
func (s *MemoryDailyStore) DeleteBefore(day Day) error {
	if err := validateDay(day); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.usage {
		if key.day < day {
			delete(s.usage, key)
		}
	}
	return nil
}

func validateDay(day Day) error {
	parsed, err := time.Parse("2006-01-02", string(day))
	if err != nil || Day(parsed.Format("2006-01-02")) != day {
		return ErrInvalidDay
	}
	return nil
}

func validateReservations(entries []DailyReservation) error {
	type identity struct {
		scope Scope
		id    string
	}
	seen := make(map[identity]struct{}, len(entries))
	for _, entry := range entries {
		if !validDailyScope(entry.Scope) || entry.ID == "" || entry.RequestLimit < 0 || entry.TokenLimit < 0 {
			return ErrInvalidDailyEntry
		}
		key := identity{scope: entry.Scope, id: entry.ID}
		if _, exists := seen[key]; exists {
			return ErrDuplicateDailyEntry
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateTokenUsage(entries []DailyTokenUsage) error {
	type identity struct {
		scope Scope
		id    string
	}
	seen := make(map[identity]struct{}, len(entries))
	for _, entry := range entries {
		if !validDailyScope(entry.Scope) || entry.ID == "" || entry.Tokens < 0 {
			return ErrInvalidDailyEntry
		}
		key := identity{scope: entry.Scope, id: entry.ID}
		if _, exists := seen[key]; exists {
			return ErrDuplicateDailyEntry
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validDailyScope(scope Scope) bool {
	return scope == ScopeKey || scope == ScopeUser
}
