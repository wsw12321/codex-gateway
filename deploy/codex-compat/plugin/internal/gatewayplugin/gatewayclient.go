package gatewayplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
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

// Egress is the route the Gateway binds one account to. ProxyURL is empty for
// the deployment's global egress.
type Egress struct {
	ProxyURL string
	Revision string
}

// Allocator is the Gateway's side of account allocation.
type Allocator interface {
	// ResolveEgress returns the egress the account is bound to right now.
	ResolveEgress(ctx context.Context, provider, accountID string) (Egress, error)
	// Eligible returns the subset of accounts the user may use right now.
	Eligible(ctx context.Context, provider, userID string, accounts []string) ([]Eligibility, error)
	// Select asks the Gateway to choose one of accounts by weight and spend.
	Select(ctx context.Context, provider, userID string, accounts []string) (string, error)
	// Report records the final decision of one attempt for attribution.
	Report(ctx context.Context, provider string, attempt Attempt) error
	// RegisterEgress commits the egress the Owner chose before a login or an
	// import to the account that login produced.
	RegisterEgress(ctx context.Context, provider, registrationID, accountID string) error
}

// GatewayClient calls the fixed Gateway callbacks. It never uses a proxy,
// never follows a redirect, and bounds every call.
type GatewayClient struct {
	origin string
	key    string
	client *http.Client
	// egressHost is the only authority an account-bound route may name.
	egressHost string
}

// NewGatewayClient builds a client for origin authenticated with key.
func NewGatewayClient(origin, key, egressHost string, timeout time.Duration) *GatewayClient {
	return &GatewayClient{
		origin:     origin,
		key:        key,
		egressHost: egressHost,
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
	if path == "" {
		return 0, nil, ErrUnavailable
	}
	return c.postPath(ctx, path+action, body, limit)
}

func (c *GatewayClient) postPath(ctx context.Context, path string, body any, limit int64) (int, []byte, error) {
	if c.key == "" {
		return 0, nil, ErrUnavailable
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+path, bytes.NewReader(raw))
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

// gatewayProvider names a provider the way the Gateway's egress tables do.
func gatewayProvider(provider string) string {
	if provider == ProviderClaude {
		return "anthropic"
	}
	return provider
}

// ResolveEgress implements Allocator. The Gateway is the only source of an
// account's route: whatever a credential file says about its proxy is not
// trusted. An account-bound route must point at the fixed egress listener and
// carry its route credential; anything else is refused.
func (c *GatewayClient) ResolveEgress(ctx context.Context, provider, accountID string) (Egress, error) {
	if accountsPath(provider) == "" || !validAccountID(accountID) {
		return Egress{}, ErrUnavailable
	}
	body := struct {
		Provider  string `json:"provider"`
		AccountID string `json:"account_id"`
	}{gatewayProvider(provider), accountID}
	status, data, err := c.postPath(ctx, "/internal/upstream-accounts/resolve-egress", body, 16<<10)
	if err != nil || status != http.StatusOK {
		return Egress{}, ErrUnavailable
	}
	var resolution struct {
		Mode     string `json:"mode"`
		ProxyURL string `json:"proxy_url"`
		Revision string `json:"revision"`
	}
	if _, errObject := decodeObject(data); errObject != nil || json.Unmarshal(data, &resolution) != nil || resolution.Revision == "" {
		return Egress{}, ErrUnavailable
	}
	switch resolution.Mode {
	case "global":
		if resolution.ProxyURL != "" {
			return Egress{}, ErrUnavailable
		}
	case "account":
		parsed, errParse := url.Parse(resolution.ProxyURL)
		if errParse != nil || parsed.Scheme != "http" || parsed.Host != c.egressHost || parsed.User == nil ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Egress{}, ErrUnavailable
		}
	default:
		return Egress{}, ErrUnavailable
	}
	return Egress{ProxyURL: resolution.ProxyURL, Revision: resolution.Revision}, nil
}

// RegisterEgress implements Allocator. The Gateway binds the account to the
// egress recorded under the registration; the answer must name the same
// registration and account.
func (c *GatewayClient) RegisterEgress(ctx context.Context, provider, registrationID, accountID string) error {
	if accountsPath(provider) == "" || !validUUID(registrationID) || !validAccountID(accountID) {
		return ErrUnavailable
	}
	body := struct {
		RegistrationID string `json:"registration_id"`
		Provider       string `json:"provider"`
		AccountID      string `json:"account_id"`
	}{registrationID, gatewayProvider(provider), accountID}
	status, data, err := c.postPath(ctx, "/internal/upstream-accounts/register-egress", body, 4<<10)
	if err != nil || status != http.StatusOK {
		return ErrUnavailable
	}
	var answer struct {
		RegistrationID string `json:"registration_id"`
		AccountID      string `json:"account_id"`
		Status         string `json:"status"`
	}
	if _, errObject := decodeObject(data); errObject != nil || json.Unmarshal(data, &answer) != nil ||
		answer.RegistrationID != registrationID || answer.AccountID != accountID || answer.Status != "committed" {
		return ErrUnavailable
	}
	return nil
}
