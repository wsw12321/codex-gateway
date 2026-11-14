package proxy

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// NewCPAAntigravity uses the same CPA instance and internal secret as Codex.
// Provider routing comes from the reviewed model catalog, never caller headers.
// A reply may arrive only after minutes, so the usual Codex header timeout is
// too short; internal traffic must not traverse an environment proxy.
func NewCPAAntigravity(baseURL *url.URL, token string) *Client {
	client := New(baseURL, token)
	client.antigravity = true
	transport := client.http.Transport.(*http.Transport)
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 6 * time.Minute
	return client
}

func (c *Client) transportFailure(ctx context.Context, err error) *Failure {
	failure := transportFailure(ctx, err)
	if c.antigravity && failure.Code == "upstream_unavailable" {
		failure.Status = http.StatusServiceUnavailable
	}
	return failure
}
