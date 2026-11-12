package gatewayplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// errEgressPending means the account's credential did not carry the route the
// Gateway binds it to. The route has been written, but CPA took its snapshot
// of the credential before this pick, so this attempt cannot use the account.
var errEgressPending = errors.New("account egress pending")

// ensureEgress makes sure CPA will reach the provider for this account through
// the route the Gateway binds it to. The Gateway is the only source of truth:
// a proxy that came with an uploaded or hand-edited credential is replaced,
// never trusted. Any doubt keeps the account out of this attempt.
func (p *Plugin) ensureEgress(ctx context.Context, provider string, account *Account) error {
	route, err := p.alloc.ResolveEgress(ctx, provider, account.ID)
	if err != nil {
		return err
	}
	if route.ProxyURL == account.ProxyURL {
		return nil
	}
	if err = p.applyProxy(ctx, *account, route.ProxyURL); err != nil {
		logf("egress of account %s was not applied: %T", account.ID, err)
		return err
	}
	p.inventory.NoteProxy(account.AuthID, route.ProxyURL)
	return errEgressPending
}

// syncEgress applies the account's current route outside any request, so a
// binding change takes effect before the next request instead of excluding
// the account once. It reports the route revision now in force.
func (p *Plugin) syncEgress(ctx context.Context, provider string, account Account) (string, error) {
	route, err := p.alloc.ResolveEgress(ctx, provider, account.ID)
	if err != nil {
		return "", err
	}
	if route.ProxyURL != account.ProxyURL {
		if err = p.applyProxy(ctx, account, route.ProxyURL); err != nil {
			return "", err
		}
		p.inventory.NoteProxy(account.AuthID, route.ProxyURL)
	}
	return route.Revision, nil
}

// loopbackApplyProxy writes a credential's proxy through CPA's own management
// API. CPA updates the credential in memory and on disk, and every operation
// on it from then on (inference, refresh, model listing) uses that proxy.
func (p *Plugin) loopbackApplyProxy(ctx context.Context, account Account, proxyURL string) error {
	if p.cfg.ManagementKey == "" || account.Name == "" {
		return ErrUnavailable
	}
	body, err := json.Marshal(map[string]string{"name": account.Name, "proxy_url": proxyURL})
	if err != nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, p.cfg.LoopbackURL+"/v0/management/auth-files/fields", bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+p.cfg.ManagementKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.loopback.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	var result struct {
		Status string `json:"status"`
	}
	if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(data, &result) != nil || result.Status != "ok" {
		return ErrUnavailable
	}
	return nil
}
