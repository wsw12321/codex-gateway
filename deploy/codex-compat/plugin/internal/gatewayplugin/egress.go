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
	proxyURL, err := p.accountProxy(route)
	if err != nil {
		return err
	}
	if proxyURL == account.ProxyURL {
		return nil
	}
	if err = p.applyProxy(ctx, *account, proxyURL); err != nil {
		logf("egress of account %s was not applied: %T", account.ID, err)
		return err
	}
	p.inventory.NoteProxy(account.AuthID, proxyURL)
	return errEgressPending
}

// accountProxy is the proxy URL a credential must carry for route. The global
// egress is written out like any other route: CPA has no proxy of its own, so
// the URL in the credential is the only way out, and a credential without one
// cannot reach a provider at all.
func (p *Plugin) accountProxy(route Egress) (string, error) {
	if route.ProxyURL != "" {
		return route.ProxyURL, nil
	}
	if p.cfg.GlobalProxyURL == "" {
		return "", ErrUnavailable
	}
	return p.cfg.GlobalProxyURL, nil
}

// syncEgress applies the account's current route outside any request, so a
// binding change takes effect before the next request instead of excluding
// the account once. It reports the route revision now in force.
func (p *Plugin) syncEgress(ctx context.Context, provider string, account Account) (string, error) {
	route, err := p.alloc.ResolveEgress(ctx, provider, account.ID)
	if err != nil {
		return "", err
	}
	proxyURL, err := p.accountProxy(route)
	if err != nil {
		return "", err
	}
	if proxyURL != account.ProxyURL {
		if err = p.applyProxy(ctx, account, proxyURL); err != nil {
			return "", err
		}
		p.inventory.NoteProxy(account.AuthID, proxyURL)
	}
	return route.Revision, nil
}

// ReconcileEgress writes every credential's current route, duplicates
// included, outside any request. CPA refreshes credentials on its own schedule
// with whatever proxy the file carries; this pass bounds how long a binding
// change, a missed push or a credential that arrived without a route can
// last. A failure leaves the credential as it was and is retried on the next
// pass; requests keep checking the route on every pick regardless.
func (p *Plugin) ReconcileEgress(ctx context.Context) (accounts, failed int) {
	if p.unavailable != nil {
		return 0, 0
	}
	all, err := p.inventory.All()
	if err != nil {
		return 0, 1
	}
	for _, account := range all {
		if ctx.Err() != nil {
			return accounts, failed + 1
		}
		accounts++
		callCtx, cancel := context.WithTimeout(ctx, 4*p.cfg.CallbackTimeout)
		_, errSync := p.syncEgress(callCtx, account.Provider, account)
		cancel()
		if errSync != nil {
			failed++
		}
	}
	if failed > 0 {
		logf("egress reconcile left %d of %d credentials unconfirmed", failed, accounts)
	}
	return accounts, failed
}

// reconcileWarmup covers the time CPA needs to load its credentials after the
// plugin registers: until a pass has seen and confirmed at least one
// credential, passes run every reconcileRetry instead of every interval.
const (
	reconcileWarmup = time.Minute
	reconcileRetry  = 2 * time.Second
)

// startReconcile runs ReconcileEgress in the background until stopReconcile.
// It is idempotent and does nothing when the interval is zero.
func (p *Plugin) startReconcile() {
	if p.unavailable != nil || p.cfg.EgressReconcileInterval <= 0 {
		return
	}
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()
	if p.reconcileStop != nil {
		return
	}
	stop := make(chan struct{})
	p.reconcileStop = stop
	started := p.now()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logf("egress reconcile stopped after a panic: %T", recovered)
			}
		}()
		delay := reconcileRetry
		for {
			timer := time.NewTimer(delay)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), p.cfg.EgressReconcileInterval)
			accounts, failed := p.ReconcileEgress(ctx)
			cancel()
			delay = p.cfg.EgressReconcileInterval
			if (accounts == 0 || failed > 0) && p.now().Sub(started) < reconcileWarmup {
				delay = reconcileRetry
			}
		}
	}()
}

// stopReconcile ends the background passes, if any.
func (p *Plugin) stopReconcile() {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()
	if p.reconcileStop != nil {
		close(p.reconcileStop)
		p.reconcileStop = nil
	}
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
