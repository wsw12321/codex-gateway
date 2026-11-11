package gatewayplugin

import (
	"sync"
	"time"
)

// attempt is one account allocation of a request. A request has at most one
// live attempt; a failover replaces it.
type attempt struct {
	provider       string
	account        string
	rootKey        string
	bindKey        bindingKey
	bound          bool
	selectionID    string
	controlVersion uint64
	release        func(SlotOutcome)
	// accountFailure records that the upstream blamed this account, so the
	// conversation must not keep holding or reusing it.
	accountFailure bool
}

// requestState tracks one Gateway request from interception to completion.
type requestState struct {
	token    string
	generate bool
	created  time.Time
	current  *attempt
}

// finishedAttempt keeps what a late usage report needs after the request
// itself has completed.
type finishedAttempt struct {
	token    string
	attempt  *attempt
	finished time.Time
	released bool
}

// requestTable correlates the hooks of one request. The Gateway's request
// token arrives in headers, which the scheduler and the before-auth
// interceptor see; completion is keyed by the host request id and usage by the
// selection id.
type requestTable struct {
	mu          sync.Mutex
	byToken     map[string]*requestState
	byRequestID map[string]string
	bySelection map[string]*finishedAttempt
	now         func() time.Time
	maxAge      time.Duration
	lateWindow  time.Duration
	lastSweep   time.Time
}

func newRequestTable() *requestTable {
	return &requestTable{
		byToken: map[string]*requestState{}, byRequestID: map[string]string{}, bySelection: map[string]*finishedAttempt{},
		now: time.Now, maxAge: 6 * time.Hour, lateWindow: 10 * time.Minute,
	}
}

// begin registers a request at before-auth interception.
func (t *requestTable) begin(token, requestID string, generate bool) {
	if token == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.byToken[token]
	if state == nil {
		state = &requestState{token: token, created: t.now()}
		t.byToken[token] = state
	}
	state.generate = state.generate || generate
	if requestID != "" {
		t.byRequestID[requestID] = token
	}
}

// replace installs next as the request's live attempt and returns the one it
// supersedes. A second pick for the same request is a failover: the previous
// account failed in a way that let CPA move on.
func (t *requestTable) replace(token string, next *attempt) (previous *attempt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.byToken[token]
	if state == nil {
		state = &requestState{token: token, generate: true, created: t.now()}
		t.byToken[token] = state
	}
	previous = state.current
	state.current = next
	if previous != nil && previous.selectionID != "" {
		t.bySelection[previous.selectionID] = &finishedAttempt{token: token, attempt: previous, finished: t.now(), released: true}
	}
	if next.selectionID != "" {
		t.bySelection[next.selectionID] = &finishedAttempt{token: token, attempt: next}
	}
	return previous
}

// complete removes a request by host request id and returns its state.
func (t *requestTable) complete(requestID string) *requestState {
	t.mu.Lock()
	defer t.mu.Unlock()
	token, ok := t.byRequestID[requestID]
	if !ok {
		return nil
	}
	delete(t.byRequestID, requestID)
	state := t.byToken[token]
	delete(t.byToken, token)
	if state != nil && state.current != nil && state.current.selectionID != "" {
		if finished := t.bySelection[state.current.selectionID]; finished != nil {
			finished.finished, finished.released = t.now(), true
		}
	}
	return state
}

// selection returns the attempt a usage record refers to. released reports
// whether its slot has already been given back.
func (t *requestTable) selection(selectionID string) (found *attempt, released bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	finished := t.bySelection[selectionID]
	if finished == nil {
		return nil, false
	}
	return finished.attempt, finished.released
}

// markAccountFailure flags a live attempt under the table lock.
func (t *requestTable) markAccountFailure(target *attempt) {
	t.mu.Lock()
	target.accountFailure = true
	t.mu.Unlock()
}

func (t *requestTable) accountFailed(target *attempt) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return target.accountFailure
}

// sweep drops state whose terminal event never arrived and returns the
// attempts whose slots must be given back. It is cheap and runs at most once
// a minute, from the request path.
func (t *requestTable) sweep() []*attempt {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if now.Sub(t.lastSweep) < time.Minute {
		return nil
	}
	t.lastSweep = now
	var stale []*attempt
	for token, state := range t.byToken {
		if now.Sub(state.created) > t.maxAge {
			if state.current != nil {
				stale = append(stale, state.current)
			}
			delete(t.byToken, token)
		}
	}
	for requestID, token := range t.byRequestID {
		if t.byToken[token] == nil {
			delete(t.byRequestID, requestID)
		}
	}
	for selectionID, finished := range t.bySelection {
		if finished.released && now.Sub(finished.finished) > t.lateWindow {
			delete(t.bySelection, selectionID)
		} else if !finished.released && t.byToken[finished.token] == nil {
			delete(t.bySelection, selectionID)
		}
	}
	return stale
}
