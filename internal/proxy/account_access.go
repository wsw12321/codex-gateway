package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// GroupUpstreamAccountScopeV1 requires the sidecar to constrain every attempt,
// including affinity and retries, to the admission-time group account pool.
const GroupUpstreamAccountScopeV1 = "group_upstream_account_scope_v1"

// GroupUpstreamAccountLayersV1 marks a sidecar that selects accounts layer by
// layer in billing order. It is served on its own route because older
// gateways decode the account-access capability strictly.
const GroupUpstreamAccountLayersV1 = "group_upstream_account_layers_v1"

const AccountEgressProtocolV1 = "account_egress_v1"

// Check each dispatch, so replacing a sidecar with an old image cannot retain a
// cached authorization capability. The endpoint uses the normal internal token.
func (c *Client) requireAccountAccessCapability(ctx context.Context, groupScoped bool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var response struct {
		Protocol          string `json:"protocol"`
		AnthropicMessages string `json:"anthropic_messages,omitempty"`
		GroupAccountScope string `json:"group_upstream_account_scope,omitempty"`
		AccountEgress     string `json:"account_egress,omitempty"`
	}
	if err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts/capabilities", &response); err != nil {
		return err
	}
	if response.Protocol != AccountAccessProtocolV1 {
		return errors.New("sidecar account access protocol unavailable")
	}
	if c.requireEgress && response.AccountEgress != AccountEgressProtocolV1 {
		return errors.New("sidecar account egress protocol unavailable")
	}
	if c.anthropic && response.AnthropicMessages != AnthropicMessagesProtocolV1 {
		return errors.New("sidecar Anthropic protocol unavailable")
	}
	if groupScoped && response.GroupAccountScope != GroupUpstreamAccountScopeV1 {
		return errors.New("sidecar group account scope protocol unavailable")
	}
	return nil
}

// resolveFundingLayers decides what the sidecar receives for this dispatch.
// A lone personal layer equals sending no scope. Several layers need the
// layered protocol; a sidecar without it (404) gets only the first layer,
// which is exactly the pre-layer contract, so either side can be upgraded
// first. Any other capability failure fails closed.
func (c *Client) resolveFundingLayers(ctx context.Context, groups []AllocationGroup) ([]AllocationGroup, error) {
	if len(groups) == 1 && groups[0].AllAccounts {
		return nil, nil
	}
	if len(groups) <= 1 {
		return groups, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var response struct {
		Protocol string `json:"protocol"`
	}
	err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts/funding-layers", &response)
	var apiErr *InternalAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		if groups[0].AllAccounts {
			return nil, nil
		}
		return groups[:1], nil
	}
	if err != nil {
		return nil, err
	}
	if response.Protocol != GroupUpstreamAccountLayersV1 {
		return nil, errors.New("sidecar funding layer protocol unavailable")
	}
	return groups, nil
}

// prepareAccountScope negotiates the funding layers and the account-access
// capability for an authenticated dispatch.
func (c *Client) prepareAccountScope(ctx context.Context, options *ForwardOptions) error {
	groups, err := c.resolveFundingLayers(ctx, options.Groups)
	if err != nil {
		return err
	}
	options.Groups = groups
	return c.requireAccountAccessCapability(ctx, len(groups) > 0)
}

func setGroupAccountScope(headers http.Header, options ForwardOptions) error {
	if len(options.Groups) == 0 {
		return nil
	}
	if !gatewayUserPattern.MatchString(options.UserID) {
		return errors.New("group account scope requires an authenticated user")
	}
	if err := validateFundingLayers(options.Groups); err != nil {
		return err
	}
	encoded, err := json.Marshal(options.Groups)
	if err != nil || len(encoded) > 8192 {
		return errors.New("upstream group context is too large")
	}
	headers.Set(gatewayGroupsHeader, string(encoded))
	return nil
}

// validateFundingLayers enforces the wire contract CPA checks: only the last
// layer may be personal, and only the personal layer may skip a pool.
func validateFundingLayers(groups []AllocationGroup) error {
	for i, group := range groups {
		personal := group.GroupID == PersonalLayerID
		if personal != group.AllAccounts || (personal && (i != len(groups)-1 || len(group.AccountIDs) != 0)) {
			return errors.New("invalid upstream funding layer")
		}
	}
	return nil
}
