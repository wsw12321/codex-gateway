package gatewayplugin

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// SlotOutcome tells a release what the finished execution means for the
// conversation's hold on that account.
type SlotOutcome uint8

const (
	// SlotKeep leaves any existing hold unchanged: request-scoped failures,
	// cancellations and token counting.
	SlotKeep SlotOutcome = iota
	// SlotHold extends the hold so another conversation cannot take the slot
	// while the client composes its next turn.
	SlotHold
	// SlotDrop clears the hold after an account-attributed failure.
	SlotDrop
)

// rootSlot is one root conversation's occupancy of an account slot: refs
// counts its in-flight executions and heldUntil keeps the slot afterwards.
type rootSlot struct {
	refs      int64
	heldUntil time.Time
}

func (s *rootSlot) occupied(now time.Time) bool {
	return s.refs > 0 || s.heldUntil.After(now)
}

// Concurrency counts active root conversations per account. Overlapping
// executions of one root share a slot; a root that moves to another account
// never occupies two.
type Concurrency struct {
	mu      sync.Mutex
	slots   map[string]map[string]*rootSlot
	limits  map[string]int64
	holdTTL time.Duration
	now     func() time.Time
	seq     uint64
}

// NewConcurrency creates an empty tracker.
func NewConcurrency(holdTTL time.Duration) *Concurrency {
	return &Concurrency{slots: map[string]map[string]*rootSlot{}, limits: map[string]int64{}, holdTTL: holdTTL, now: time.Now}
}

// SetLimit records the Owner-configured limit the Gateway returned.
func (c *Concurrency) SetLimit(account string, limit int64) {
	c.mu.Lock()
	c.limits[account] = limit
	c.mu.Unlock()
}

// limitLocked is fail-closed: an account without a known limit admits one
// conversation, which is the database default.
func (c *Concurrency) limitLocked(account string) int64 {
	if limit := c.limits[account]; limit > 0 {
		return limit
	}
	return 1
}

func (c *Concurrency) pruneLocked(account string, now time.Time) map[string]*rootSlot {
	roots := c.slots[account]
	for key, slot := range roots {
		if !slot.occupied(now) {
			delete(roots, key)
		}
	}
	if len(roots) == 0 {
		delete(c.slots, account)
		return nil
	}
	return roots
}

// Admissible drops accounts with no free slot for this conversation. Accounts
// the conversation already occupies, in flight or held, always stay.
func (c *Concurrency) Admissible(rootKey string, accounts []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	result := make([]string, 0, len(accounts))
	for _, account := range accounts {
		roots := c.pruneLocked(account, now)
		holds := rootKey != "" && roots[rootKey] != nil
		if int64(len(roots)) < c.limitLocked(account) || holds {
			result = append(result, account)
		}
	}
	return result
}

// Acquire takes a slot for one execution. rootKey is empty for requests with
// no conversation identity: they get a private key and never hold a slot.
// The returned release must be called exactly once per successful acquire;
// extra calls are ignored.
func (c *Concurrency) Acquire(account, rootKey string) (release func(SlotOutcome), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	conversation := rootKey != ""
	if !conversation {
		c.seq++
		rootKey = "execution:" + itoa(int64(c.seq)) // #nosec G115 -- a process-local counter
	}
	roots := c.pruneLocked(account, now)
	if slot := roots[rootKey]; slot != nil {
		slot.refs++
	} else {
		if int64(len(roots)) >= c.limitLocked(account) {
			return nil, false
		}
		if roots == nil {
			roots = map[string]*rootSlot{}
			c.slots[account] = roots
		}
		roots[rootKey] = &rootSlot{refs: 1}
	}
	if conversation {
		// Running here frees this conversation's idle holds elsewhere.
		for other, otherRoots := range c.slots {
			if other == account {
				continue
			}
			if slot := otherRoots[rootKey]; slot != nil && slot.refs == 0 {
				delete(otherRoots, rootKey)
				if len(otherRoots) == 0 {
					delete(c.slots, other)
				}
			}
		}
	}
	var once sync.Once
	return func(outcome SlotOutcome) {
		once.Do(func() { c.release(account, rootKey, conversation, outcome) })
	}, true
}

func (c *Concurrency) release(account, rootKey string, conversation bool, outcome SlotOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	roots := c.slots[account]
	slot := roots[rootKey]
	if slot == nil {
		return
	}
	now := c.now()
	if slot.refs > 0 {
		slot.refs--
	}
	switch outcome {
	case SlotHold:
		if until := now.Add(c.holdTTL); conversation && until.After(slot.heldUntil) {
			slot.heldUntil = until
		}
	case SlotDrop:
		slot.heldUntil = time.Time{}
	}
	if !slot.occupied(now) {
		delete(roots, rootKey)
		if len(roots) == 0 {
			delete(c.slots, account)
		}
	}
}

// DropHold clears a conversation's idle hold on an account. It covers a
// failure report that arrives after the execution was already released.
func (c *Concurrency) DropHold(account, rootKey string) {
	if !strings.HasPrefix(rootKey, "root:") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	roots := c.slots[account]
	if slot := roots[rootKey]; slot != nil {
		slot.heldUntil = time.Time{}
		if slot.refs == 0 {
			delete(roots, rootKey)
			if len(roots) == 0 {
				delete(c.slots, account)
			}
		}
	}
}

// DropIdleHolds frees every idle hold on a disabled account, so its drain
// completes as soon as in-flight executions finish.
func (c *Concurrency) DropIdleHolds(account string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	roots := c.slots[account]
	for key, slot := range roots {
		if slot.refs == 0 {
			delete(roots, key)
		}
	}
	if len(roots) == 0 {
		delete(c.slots, account)
	}
}

// AccountConcurrency is one row of the concurrency snapshot. ActiveRequests
// keeps the Gateway's field name; it counts active root conversations.
type AccountConcurrency struct {
	ID              string `json:"id"`
	ActiveRequests  int64  `json:"active_requests"`
	ConcurrentLimit int64  `json:"concurrent_limit"`
}

// Snapshot reports every known account plus any account that still has
// occupied slots, sorted by id. A disabled account keeps showing its
// in-flight executions.
func (c *Concurrency) Snapshot(known []string) []AccountConcurrency {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	seen := map[string]bool{}
	rows := make([]AccountConcurrency, 0, len(known))
	add := func(account string) {
		if seen[account] {
			return
		}
		seen[account] = true
		rows = append(rows, AccountConcurrency{ID: account, ActiveRequests: int64(len(c.pruneLocked(account, now))), ConcurrentLimit: c.limitLocked(account)})
	}
	for _, account := range known {
		add(account)
	}
	for account := range c.slots {
		add(account)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}
