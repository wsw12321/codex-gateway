package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Check each dispatch, so replacing a sidecar with an old image cannot retain a
// cached authorization capability. The endpoint uses the normal internal token.
func (c *Client) requireAccountAccessCapability(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var response struct {
		Protocol string `json:"protocol"`
	}
	if err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts/capabilities", &response); err != nil {
		return err
	}
	if response.Protocol != "upstream_account_access_v1" {
		return errors.New("sidecar account access protocol unavailable")
	}
	return nil
}
