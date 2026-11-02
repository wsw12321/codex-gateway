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

func setGroupAccountScope(headers http.Header, options ForwardOptions) error {
	if len(options.Groups) == 0 {
		return nil
	}
	if !gatewayUserPattern.MatchString(options.UserID) {
		return errors.New("group account scope requires an authenticated user")
	}
	encoded, err := json.Marshal(options.Groups)
	if err != nil || len(encoded) > 8192 {
		return errors.New("upstream group context is too large")
	}
	headers.Set(gatewayGroupsHeader, string(encoded))
	return nil
}
