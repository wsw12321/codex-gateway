package gatewayplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"
)

// Allocation outcomes. They are the only reasons a pick is refused, and both
// stop the request: there is no fallback to another strategy.
var (
	// ErrUnavailable means allocation could not be decided: the Gateway was
	// unreachable, answered outside the contract, or the request carried no
	// usable identity.
	ErrUnavailable = errors.New("gateway_allocation_unavailable")
	// ErrConcurrency means every permitted account is at its limit.
	ErrConcurrency = errors.New("upstream_concurrency_exceeded")
)

const maxCandidates = 256

// Eligibility is one account the user may use, with its Owner-configured limit.
type Eligibility struct {
	ID              string `json:"id"`
	ConcurrentLimit int64  `json:"concurrent_limit"`
}

// Attempt is the plugin's report of one allocation decision.
type Attempt struct {
	Token            string
	UserID           string
	AccountID        string
	ConversationHash string
	// Rejected carries the outcome code when no account was allocated.
	Rejected string
}

// Allocator is the Gateway's side of account allocation.
type Allocator interface {
	// Eligible returns the subset of accounts the user may use right now.
	Eligible(ctx context.Context, provider, userID string, accounts []string) ([]Eligibility, error)
	// Select asks the Gateway to choose one of accounts by weight and spend.
	Select(ctx context.Context, provider, userID string, accounts []string) (string, error)
	// Report records the final decision of one attempt for attribution.
	Report(ctx context.Context, provider string, attempt Attempt) error
}

// GatewayClient calls the fixed Gateway callbacks. It never uses a proxy,
// never follows a redirect, and bounds every call.
type GatewayClient struct {
	origin string
	key    string
	client *http.Client
}

// NewGatewayClient builds a client for origin authenticated with key.
func NewGatewayClient(origin, key string, timeout time.Duration) *GatewayClient {
	return &GatewayClient{
		origin: origin,
		key:    key,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:        nil,
				MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: timeout,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// accountsPath derives the callback from the resolved credential provider,
// never from anything the request carried.
func accountsPath(provider string) string {
	switch provider {
	case ProviderCodex:
		return "/internal/upstream-accounts/"
	case ProviderAntigravity:
		return "/internal/antigravity-accounts/"
	case ProviderClaude:
		return "/internal/anthropic-accounts/"
	}
	return ""
}

func (c *GatewayClient) post(ctx context.Context, provider, action string, body any, limit int64) (int, []byte, error) {
	path := accountsPath(provider)
	if path == "" || c.key == "" {
		return 0, nil, ErrUnavailable
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+path+action, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+c.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return 0, nil, ErrUnavailable
	}
	return response.StatusCode, data, nil
}

func sortedUnique(accounts []string) ([]string, map[string]bool, bool) {
	if len(accounts) == 0 || len(accounts) > maxCandidates {
		return nil, nil, false
	}
	seen := make(map[string]bool, len(accounts))
	ids := make([]string, 0, len(accounts))
	for _, id := range accounts {
		if !validAccountID(id) {
			return nil, nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, seen, true
}

type selectionBody struct {
	AccountIDs []string `json:"account_ids"`
	UserID     string   `json:"user_id"`
}

// Eligible implements Allocator. Any answer outside the contract, including
// an account that was not asked about, fails the whole pick.
func (c *GatewayClient) Eligible(ctx context.Context, provider, userID string, accounts []string) ([]Eligibility, error) {
	ids, asked, ok := sortedUnique(accounts)
	if !ok {
		return nil, ErrUnavailable
	}
	status, data, err := c.post(ctx, provider, "eligible", selectionBody{ids, userID}, 16<<10)
	if err != nil || status != http.StatusOK {
		return nil, ErrUnavailable
	}
	fields, err := decodeObject(data, "accounts")
	var allowed []Eligibility
	if err != nil || json.Unmarshal(fields["accounts"], &allowed) != nil {
		return nil, ErrUnavailable
	}
	permitted := make(map[string]bool, len(allowed))
	for _, item := range allowed {
		if !asked[item.ID] || permitted[item.ID] || item.ConcurrentLimit < 1 || item.ConcurrentLimit > 1<<31-1 {
			return nil, ErrUnavailable
		}
		permitted[item.ID] = true
	}
	return allowed, nil
}

// Select implements Allocator. A 429 means the offered pool is exhausted.
func (c *GatewayClient) Select(ctx context.Context, provider, userID string, accounts []string) (string, error) {
	ids, asked, ok := sortedUnique(accounts)
	if !ok {
		return "", ErrUnavailable
	}
	status, data, err := c.post(ctx, provider, "select", selectionBody{ids, userID}, 1<<10)
	if err != nil {
		return "", ErrUnavailable
	}
	if status == http.StatusTooManyRequests {
		return "", ErrConcurrency
	}
	if status != http.StatusOK {
		return "", ErrUnavailable
	}
	fields, err := decodeObject(data, "account_id")
	var selected string
	if err != nil || json.Unmarshal(fields["account_id"], &selected) != nil || !asked[selected] {
		return "", ErrUnavailable
	}
	return selected, nil
}

// Report implements Allocator.
func (c *GatewayClient) Report(ctx context.Context, provider string, attempt Attempt) error {
	body := struct {
		RequestToken     string `json:"request_token"`
		UserID           string `json:"user_id"`
		AccountID        string `json:"account_id,omitempty"`
		ConversationHash string `json:"conversation_hash,omitempty"`
		Rejected         string `json:"rejected,omitempty"`
	}{attempt.Token, attempt.UserID, attempt.AccountID, attempt.ConversationHash, attempt.Rejected}
	status, _, err := c.post(ctx, provider, "attempt", body, 1<<10)
	if err != nil || status != http.StatusNoContent && status != http.StatusOK {
		return ErrUnavailable
	}
	return nil
}
