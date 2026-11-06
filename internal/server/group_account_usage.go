package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	// Codex usage is a live ChatGPT request; however many members look, each
	// account is read at most once per TTL. Claude usage is a passive CPA
	// snapshot and can refresh more often.
	accountUsageCodexTTL      = time.Minute
	accountUsageAnthropicTTL  = 15 * time.Second
	accountUsageFailureTTL    = 10 * time.Second
	accountUsageWait          = 20 * time.Second
	maxMyAccountUsageAccounts = 64
	codexFiveHourMinutes      = 300
	codexWeeklyMinutes        = 7 * 24 * 60
)

type groupAccountUsageRepository interface {
	ListGroupAccountUsageTargets(context.Context, string) ([]store.GroupAccountUsageTarget, error)
}

// accountUsageReader reads one account's normalized usage. A non-empty code
// is one of the fixed member-facing error codes, never upstream text.
type accountUsageReader func(ctx context.Context, provider, accountID string) (accountUsage, string)

type accountUsageWindow struct {
	UsedPercent *int       `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at"`
}

// accountUsage is the provider-neutral view a member may see. It deliberately
// carries no account identity, plan, email or cost.
type accountUsage struct {
	Source        string              `json:"source,omitempty"`
	UpdatedAt     *time.Time          `json:"updated_at,omitempty"`
	CooldownUntil *time.Time          `json:"cooldown_until,omitempty"`
	Limited       bool                `json:"limited"`
	FiveHour      *accountUsageWindow `json:"five_hour"`
	SevenDay      *accountUsageWindow `json:"seven_day"`
}

type myAccountUsageAccount struct {
	Provider string `json:"provider"`
	Alias    string `json:"alias"`
	accountUsage
	ErrorCode string `json:"error_code,omitempty"`
}

type myAccountUsageGroup struct {
	GroupID   string                  `json:"group_id"`
	GroupName string                  `json:"group_name"`
	Accounts  []myAccountUsageAccount `json:"accounts"`
}

type myAccountUsageResponse struct {
	Groups []myAccountUsageGroup `json:"groups"`
}

type accountUsageEntry struct {
	ready   chan struct{}
	usage   accountUsage
	code    string
	expires time.Time
}

// accountUsageCache shares reads between members: a fresh entry is reused and
// an in-flight read is awaited instead of starting another upstream request.
type accountUsageCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*accountUsageEntry
}

func newAccountUsageCache() *accountUsageCache {
	return &accountUsageCache{now: time.Now, entries: make(map[string]*accountUsageEntry)}
}

func (s *Server) accountUsageCache() *accountUsageCache {
	s.accountUsageOnce.Do(func() {
		if s.accountUsages == nil {
			s.accountUsages = newAccountUsageCache()
		}
	})
	return s.accountUsages
}

func accountUsageKey(provider, accountID string) string { return provider + "/" + accountID }

// lookup returns the fresh or in-flight entry for key, starting read in the
// background when neither exists. read must not depend on a request context
// that can be cancelled, because other members may be waiting on the result.
func (c *accountUsageCache) lookup(key string, read func() (accountUsage, string, time.Duration)) *accountUsageEntry {
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		select {
		case <-entry.ready:
			if c.now().Before(entry.expires) {
				c.mu.Unlock()
				return entry
			}
		default:
			c.mu.Unlock()
			return entry
		}
	}
	entry := &accountUsageEntry{ready: make(chan struct{})}
	c.entries[key] = entry
	c.pruneLocked()
	c.mu.Unlock()
	go func() {
		usage, code, ttl := read()
		c.mu.Lock()
		entry.usage, entry.code, entry.expires = usage, code, c.now().Add(ttl)
		c.mu.Unlock()
		close(entry.ready)
	}()
	return entry
}

// store records a usage result obtained elsewhere, such as an Owner's manual
// quota query, so members see it without another upstream request.
func (c *accountUsageCache) store(key string, usage accountUsage, ttl time.Duration) {
	entry := &accountUsageEntry{ready: make(chan struct{}), usage: usage}
	close(entry.ready)
	c.mu.Lock()
	entry.expires = c.now().Add(ttl)
	c.entries[key] = entry
	c.pruneLocked()
	c.mu.Unlock()
}

func (c *accountUsageCache) pruneLocked() {
	if len(c.entries) <= 1024 {
		return
	}
	now := c.now()
	for key, entry := range c.entries {
		select {
		case <-entry.ready:
			if !now.Before(entry.expires) {
				delete(c.entries, key)
			}
		default:
		}
	}
}

func (s *Server) groupAccountUsageStorage() groupAccountUsageRepository {
	if s.groupAccountUsageRepo != nil {
		return s.groupAccountUsageRepo
	}
	return s.store
}

// myAccountUsage returns the usage windows of every account the signed-in
// user may inspect through a group that opted in. The account set is derived
// only from the session user; the request carries no account identifiers.
func (s *Server) myAccountUsage(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_account_usage_request", "账号用量请求不能包含查询参数")
		return
	}
	accounts, err := s.groupAccountUsageStorage().ListGroupAccountUsageTargets(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		internalError(s, w, r, "list group account usage accounts", err)
		return
	}
	if len(accounts) > maxMyAccountUsageAccounts {
		accounts = accounts[:maxMyAccountUsageAccounts]
	}
	targets := make([]accountUsageTarget, 0, len(accounts))
	for _, account := range accounts {
		targets = append(targets, accountUsageTarget{provider: account.Provider, accountID: account.AccountID})
	}
	results := s.collectAccountUsage(r.Context(), targets)
	response := myAccountUsageResponse{Groups: []myAccountUsageGroup{}}
	for _, account := range accounts {
		item := myAccountUsageAccount{Provider: account.Provider, Alias: account.Alias}
		result := results[accountUsageKey(account.Provider, account.AccountID)]
		item.accountUsage, item.ErrorCode = result.usage, result.code
		if last := len(response.Groups) - 1; last < 0 || response.Groups[last].GroupID != account.GroupID {
			response.Groups = append(response.Groups, myAccountUsageGroup{GroupID: account.GroupID, GroupName: account.GroupName, Accounts: []myAccountUsageAccount{}})
		}
		group := &response.Groups[len(response.Groups)-1]
		group.Accounts = append(group.Accounts, item)
	}
	writeJSON(w, http.StatusOK, response)
}

type accountUsageTarget struct{ provider, accountID string }

// accountUsageResult is one account's usage or its fixed error code.
type accountUsageResult struct {
	usage accountUsage
	code  string
}

// collectAccountUsage starts or joins one shared read per distinct account
// and waits at most accountUsageWait in total. A read still running when the
// wait ends reports usage_timeout and keeps filling the cache for the next
// refresh. Results are keyed by accountUsageKey.
func (s *Server) collectAccountUsage(ctx context.Context, targets []accountUsageTarget) map[string]accountUsageResult {
	cache := s.accountUsageCache()
	// Shared reads outlive this request; keep its values for audit attribution.
	background := context.WithoutCancel(ctx)
	entries := make(map[string]*accountUsageEntry, len(targets))
	for _, target := range targets {
		key := accountUsageKey(target.provider, target.accountID)
		if entries[key] == nil {
			provider, accountID := target.provider, target.accountID
			entries[key] = cache.lookup(key, func() (accountUsage, string, time.Duration) {
				return s.readAccountUsage(background, provider, accountID)
			})
		}
	}
	wait, cancel := context.WithTimeout(ctx, accountUsageWait)
	defer cancel()
	results := make(map[string]accountUsageResult, len(entries))
	for key, entry := range entries {
		select {
		case <-entry.ready:
			results[key] = accountUsageResult{usage: entry.usage, code: entry.code}
		case <-wait.Done():
			results[key] = accountUsageResult{code: "usage_timeout"}
		}
	}
	return results
}

type groupPoolUsageRepository interface {
	ListGroupPoolAccounts(context.Context, string) ([]store.GroupPoolAccount, error)
}

func (s *Server) groupPoolUsageStorage() groupPoolUsageRepository {
	if s.groupPoolUsageRepo != nil {
		return s.groupPoolUsageRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

// groupPoolUsageAccount is the Owner's view of one pooled account: unlike the
// member view it names the account, since the Owner manages it.
type groupPoolUsageAccount struct {
	AccountID   string `json:"account_id"`
	Provider    string `json:"provider"`
	Alias       string `json:"alias,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	EmailMasked string `json:"email_masked,omitempty"`
	accountUsage
	ErrorCode string `json:"error_code,omitempty"`
}

type groupPoolUsageResponse struct {
	GroupID   string                  `json:"group_id"`
	Accounts  []groupPoolUsageAccount `json:"accounts"`
	Truncated bool                    `json:"truncated"`
}

// groupPoolAccountUsage returns the five-hour and weekly windows of every
// account in the group's selected Codex and Claude pools for the Owner,
// whether or not members may see them. Reads share the member cache, so an
// Owner looking never adds upstream requests beyond its TTL.
func (s *Server) groupPoolAccountUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validGroupID(id) {
		s.groupStoreError(w, r, "read group pool account usage", store.ErrNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		s.groupStoreError(w, r, "read group pool account usage", store.ErrInvalid)
		return
	}
	repository := s.groupPoolUsageStorage()
	if repository == nil {
		internalError(s, w, r, "read group pool account usage", errors.New("group repository unavailable"))
		return
	}
	accounts, err := repository.ListGroupPoolAccounts(r.Context(), id)
	if err != nil {
		s.groupStoreError(w, r, "read group pool account usage", err)
		return
	}
	response := groupPoolUsageResponse{GroupID: id, Accounts: make([]groupPoolUsageAccount, 0, len(accounts))}
	if len(accounts) > maxMyAccountUsageAccounts {
		accounts, response.Truncated = accounts[:maxMyAccountUsageAccounts], true
	}
	targets := make([]accountUsageTarget, 0, len(accounts))
	for _, account := range accounts {
		targets = append(targets, accountUsageTarget{provider: account.Provider, accountID: account.AccountID})
	}
	results := s.collectAccountUsage(r.Context(), targets)
	for _, account := range accounts {
		result := results[accountUsageKey(account.Provider, account.AccountID)]
		response.Accounts = append(response.Accounts, groupPoolUsageAccount{
			AccountID: account.AccountID, Provider: account.Provider, Alias: account.Alias,
			DisplayName: account.DisplayName, EmailMasked: account.EmailMasked,
			accountUsage: result.usage, ErrorCode: result.code,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) readAccountUsage(ctx context.Context, provider, accountID string) (accountUsage, string, time.Duration) {
	read := s.accountUsageRead
	if read == nil {
		read = s.readProviderAccountUsage
	}
	usage, code := read(ctx, provider, accountID)
	if code != "" {
		if s.logger != nil {
			s.logger.Warn("group member account usage read failed", "provider", provider, "code", code)
		}
		return accountUsage{}, code, accountUsageFailureTTL
	}
	s.recordUpstreamQuotaReading(ctx, provider, accountID, usage)
	if provider == store.UpstreamProviderCodex {
		return usage, "", accountUsageCodexTTL
	}
	return usage, "", accountUsageAnthropicTTL
}

func (s *Server) readProviderAccountUsage(ctx context.Context, provider, accountID string) (accountUsage, string) {
	if !validUpstreamAccountID(accountID) {
		return accountUsage{}, "usage_unavailable"
	}
	switch provider {
	case store.UpstreamProviderCodex:
		return s.readCodexAccountUsage(ctx, accountID)
	case store.UpstreamProviderAnthropic:
		return s.readAnthropicAccountUsage(ctx, accountID)
	default:
		return accountUsage{}, "usage_unavailable"
	}
}

// readCodexAccountUsage shares the Owner quota query's per-account limiter,
// so members and the Owner together never exceed its ChatGPT request rate.
func (s *Server) readCodexAccountUsage(ctx context.Context, accountID string) (accountUsage, string) {
	if s.upstream == nil {
		return accountUsage{}, "usage_unavailable"
	}
	now := time.Now().UTC()
	quota, code := s.queryCodexQuota(ctx, accountID, "group_account_usage")
	switch code {
	case "":
		return codexAccountUsage(quota, now), ""
	case "upstream_account_disabled":
		return accountUsage{}, "account_disabled"
	case "upstream_quota_query_rate_limited", "upstream_quota_rate_limited":
		return accountUsage{}, "usage_rate_limited"
	default:
		return accountUsage{}, "usage_unavailable"
	}
}

// codexAccountUsage keeps only the default codex bucket; additional
// per-feature buckets are not shown to members.
func codexAccountUsage(quota gatewayproxy.UpstreamQuota, at time.Time) accountUsage {
	limits := quota.Result.RateLimits
	usage := accountUsage{Source: "live", UpdatedAt: &at,
		Limited: limits.RateLimitReachedType != nil && strings.TrimSpace(*limits.RateLimitReachedType) != ""}
	for _, window := range []*gatewayproxy.RateLimitWindow{limits.Primary, limits.Secondary} {
		if window == nil || window.WindowDurationMins == nil {
			continue
		}
		used := window.UsedPercent
		value := &accountUsageWindow{UsedPercent: &used}
		if window.ResetsAt != nil {
			reset := time.Unix(*window.ResetsAt, 0).UTC()
			value.ResetAt = &reset
		}
		switch *window.WindowDurationMins {
		case codexFiveHourMinutes:
			usage.FiveHour = value
		case codexWeeklyMinutes:
			usage.SevenDay = value
		}
	}
	return usage
}

// readAnthropicAccountUsage reads the same passive CPA snapshot the Owner's
// account page shows; it never triggers a request to Claude.
func (s *Server) readAnthropicAccountUsage(ctx context.Context, accountID string) (accountUsage, string) {
	var result struct {
		ObservedAt    *time.Time          `json:"observed_at"`
		CooldownUntil *time.Time          `json:"cooldown_until"`
		Windows       []cpaObservedWindow `json:"windows"`
	}
	readCtx, cancel := context.WithTimeout(ctx, upstreamQuotaTimeout)
	defer cancel()
	if err := s.cpaCall(readCtx, http.MethodPost, "anthropic/accounts/"+accountID+"/quota", struct{}{}, &result); err != nil {
		return accountUsage{}, "usage_unavailable"
	}
	usage, ok := anthropicAccountUsage(result.ObservedAt, result.CooldownUntil, result.Windows, time.Now())
	if !ok {
		return accountUsage{}, "usage_unavailable"
	}
	return usage, ""
}

func anthropicAccountUsage(observedAt, cooldownUntil *time.Time, windows []cpaObservedWindow, now time.Time) (accountUsage, bool) {
	usage := accountUsage{Source: "observed", UpdatedAt: observedAt, CooldownUntil: cooldownUntil,
		Limited: cooldownUntil != nil && cooldownUntil.After(now)}
	for _, window := range windows {
		switch window.Status {
		case "allowed", "allowed_warning", "rejected", "unknown":
		default:
			return accountUsage{}, false
		}
		value := &accountUsageWindow{ResetAt: window.ResetAt}
		if window.UsedPercent != nil {
			if *window.UsedPercent < 0 || *window.UsedPercent > 100 {
				return accountUsage{}, false
			}
			used := int(math.Round(*window.UsedPercent))
			value.UsedPercent = &used
		}
		if window.Status == "rejected" {
			usage.Limited = true
		}
		switch window.Window {
		case "five_hour":
			usage.FiveHour = value
		case "seven_day":
			usage.SevenDay = value
		default:
			return accountUsage{}, false
		}
	}
	return usage, true
}
