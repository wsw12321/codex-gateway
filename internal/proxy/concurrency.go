package proxy

import (
	"context"
	"net/http"
	"time"
)

// UpstreamConcurrency contains only confirmed measurements. Missing accounts
// are unknown, even when a previous snapshot reported zero.
type UpstreamConcurrency struct {
	SampledAt time.Time                    `json:"sampled_at"`
	Accounts  []UpstreamAccountConcurrency `json:"accounts"`
}

type UpstreamAccountConcurrency struct {
	ID string `json:"id"`
	// ActiveRequests retains the wire name but counts active root conversations.
	ActiveRequests int64 `json:"active_requests"`
}

func (c *Client) UpstreamAccountConcurrency(ctx context.Context) (UpstreamConcurrency, error) {
	var wire struct {
		SampledAt *time.Time `json:"sampled_at"`
		Accounts  *[]struct {
			ID             *string `json:"id"`
			ActiveRequests *int64  `json:"active_requests"`
		} `json:"accounts"`
	}
	if err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts/concurrency", &wire); err != nil {
		return UpstreamConcurrency{}, err
	}
	invalid := &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	if wire.SampledAt == nil || wire.SampledAt.IsZero() || wire.Accounts == nil {
		return UpstreamConcurrency{}, invalid
	}
	// A stale or implausibly future measurement is not a live count. Allow clock
	// skew, but never make an old sidecar cache look current.
	if age := time.Since(*wire.SampledAt); age > time.Minute || age < -time.Minute {
		return UpstreamConcurrency{}, invalid
	}
	result := UpstreamConcurrency{SampledAt: wire.SampledAt.UTC(), Accounts: make([]UpstreamAccountConcurrency, 0, len(*wire.Accounts))}
	seen := make(map[string]bool, len(*wire.Accounts))
	for _, account := range *wire.Accounts {
		if account.ID == nil || !upstreamAccountPattern.MatchString(*account.ID) || seen[*account.ID] ||
			account.ActiveRequests == nil || *account.ActiveRequests < 0 {
			return UpstreamConcurrency{}, invalid
		}
		seen[*account.ID] = true
		result.Accounts = append(result.Accounts, UpstreamAccountConcurrency{ID: *account.ID, ActiveRequests: *account.ActiveRequests})
	}
	return result, nil
}
