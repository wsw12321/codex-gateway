package proxy

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"sync"
)

// requestTokenHeader carries the one-time dispatch token to CPA. The Gateway
// plugin echoes it on the allocation callbacks, which is how a dispatch learns
// its upstream account without trusting anything CPA renders in the response.
const requestTokenHeader = "X-Codex-Gateway-Request"

var requestTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// Allocation outcomes the plugin may report instead of an account.
const (
	allocationRejectedConcurrency = "upstream_concurrency_exceeded"
	allocationRejectedUnavailable = "gateway_allocation_unavailable"
)

var (
	// ErrUnknownDispatch means a report named no dispatch in flight for that
	// user and provider. Either the request already finished or the report
	// did not come from the dispatch it claims.
	ErrUnknownDispatch = errors.New("proxy: unknown dispatch")
	// ErrAccountOutsideScope means the reported account is not in the account
	// pool this dispatch was admitted with.
	ErrAccountOutsideScope = errors.New("proxy: account outside admitted scope")
	// ErrInvalidAllocationReport means the report is malformed.
	ErrInvalidAllocationReport = errors.New("proxy: invalid allocation report")
)

// AllocationReport is one allocation decision reported by the CPA plugin.
type AllocationReport struct {
	Token            string
	UserID           string
	AccountID        string
	ConversationHash string
	Rejected         string
}

// Valid checks the report's shape. Exactly one of AccountID and Rejected is set.
func (r AllocationReport) Valid() bool {
	if !requestTokenPattern.MatchString(r.Token) || !gatewayUserPattern.MatchString(r.UserID) {
		return false
	}
	if r.ConversationHash != "" && !conversationHashPattern.MatchString(r.ConversationHash) {
		return false
	}
	if r.Rejected != "" {
		return r.AccountID == "" && (r.Rejected == allocationRejectedConcurrency || r.Rejected == allocationRejectedUnavailable)
	}
	return upstreamAccountPattern.MatchString(r.AccountID)
}

type ledgerEntry struct {
	provider string
	userID   string
	// scope is the union of the admitted group pools, or nil when a personal
	// layer leaves the pool to the user's own entitlement.
	scope            map[string]bool
	accountID        string
	conversationHash string
	rejected         string
}

// AllocationLedger records, per in-flight dispatch, the account CPA's plugin
// allocated. It is process-local: the plugin's callbacks always reach the same
// Gateway process that is waiting on the dispatch.
type AllocationLedger struct {
	mu      sync.Mutex
	entries map[string]*ledgerEntry
}

// NewAllocationLedger creates an empty ledger.
func NewAllocationLedger() *AllocationLedger {
	return &AllocationLedger{entries: make(map[string]*ledgerEntry)}
}

// dispatch is one open ledger entry. A nil dispatch is valid and inert, which
// is what every forward path gets when the ledger is not in use.
type dispatch struct {
	ledger *AllocationLedger
	token  string
}

func (l *AllocationLedger) open(provider, userID string, groups []AllocationGroup) (*dispatch, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	entry := &ledgerEntry{provider: provider, userID: userID}
	restricted := len(groups) > 0
	for _, group := range groups {
		if group.AllAccounts {
			restricted = false
		}
	}
	if restricted {
		entry.scope = make(map[string]bool)
		for _, group := range groups {
			for _, id := range group.AccountIDs {
				entry.scope[id] = true
			}
		}
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	l.mu.Lock()
	l.entries[token] = entry
	l.mu.Unlock()
	return &dispatch{ledger: l, token: token}, nil
}

// Record stores a report on its dispatch. The last report before the response
// starts is the account that served it: an earlier one was a failed attempt.
func (l *AllocationLedger) Record(provider string, report AllocationReport) error {
	if l == nil || !report.Valid() {
		return ErrInvalidAllocationReport
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[report.Token]
	if entry == nil || entry.provider != provider || entry.userID != report.UserID {
		return ErrUnknownDispatch
	}
	if report.AccountID != "" && entry.scope != nil && !entry.scope[report.AccountID] {
		return ErrAccountOutsideScope
	}
	entry.accountID, entry.rejected = report.AccountID, report.Rejected
	if report.ConversationHash != "" {
		entry.conversationHash = report.ConversationHash
	}
	return nil
}

func (d *dispatch) close() {
	if d == nil {
		return
	}
	d.ledger.mu.Lock()
	delete(d.ledger.entries, d.token)
	d.ledger.mu.Unlock()
}

func (d *dispatch) snapshot() ledgerEntry {
	d.ledger.mu.Lock()
	defer d.ledger.mu.Unlock()
	if entry := d.ledger.entries[d.token]; entry != nil {
		return *entry
	}
	return ledgerEntry{}
}

// apply replaces header-derived attribution with the ledger's. With the plugin
// integration CPA sets no attribution headers, so a value found there could
// only have come from somewhere it should not, and is discarded.
func (d *dispatch) apply(result *Result) {
	if d == nil {
		return
	}
	entry := d.snapshot()
	result.UpstreamAccountID, result.ConversationHash = entry.accountID, entry.conversationHash
}

// failure maps a refused allocation to the Gateway's stable error. Any other
// failure is returned unchanged.
func (d *dispatch) failure(failure *Failure, retryAfter int) *Failure {
	if d == nil {
		return failure
	}
	switch d.snapshot().rejected {
	case allocationRejectedConcurrency:
		return &Failure{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "upstream_concurrency_exceeded", Message: "暂无可用的上游账号，请稍后重试", RetryAfter: max(retryAfter, 5)}
	case allocationRejectedUnavailable:
		return &Failure{Status: http.StatusServiceUnavailable, Type: "server_error", Code: "upstream_allocation_unavailable", Message: "暂时无法分配可用的上游账号，请稍后重试或联系 Owner", RetryAfter: retryAfter}
	}
	return failure
}

// UseAllocationLedger switches the client to the plugin integration: every
// dispatch that carries a user identity gets a request token, and attribution
// comes from the ledger instead of response headers.
func (c *Client) UseAllocationLedger(ledger *AllocationLedger) {
	if c != nil {
		c.ledger = ledger
	}
}

// openDispatch registers a dispatch and marks the outgoing request with its
// token. Without a ledger, or without a user to allocate for, it is a no-op.
func (c *Client) openDispatch(header http.Header, options ForwardOptions) (*dispatch, error) {
	if c.ledger == nil || options.UserID == "" {
		return nil, nil
	}
	opened, err := c.ledger.open(c.AccountProvider(), options.UserID, options.Groups)
	if err != nil {
		return nil, err
	}
	header.Set(requestTokenHeader, opened.token)
	return opened, nil
}
