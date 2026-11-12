package gatewayplugin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Scheduler metadata keys the host sets on every pick.
const (
	metadataCallerScope   = "caller_scope"
	metadataSession       = "canonical_session_id"
	metadataParentSession = "parent_session_id"
	metadataSelectionID   = "selection_id"
	metadataGenerate      = "generate"
)

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

// candidateAccount pairs a scheduler candidate with its resolved account.
type candidateAccount struct {
	authID  string
	account *Account
}

// Pick allocates the upstream account for one attempt. Every path either
// returns a candidate the Gateway authorized or an error that stops the
// request; nothing here delegates to a built-in strategy.
func (p *Plugin) Pick(ctx context.Context, request pluginapi.SchedulerPickRequest) (string, error) {
	for _, stale := range p.requests.sweep() {
		p.finishAttempt(stale, SlotKeep)
	}
	headers := http.Header(request.Options.Headers)
	trusted, ok := ParseTrusted(headers, true)
	if !ok {
		return "", ErrUnavailable
	}
	provider, candidates, err := p.resolveCandidates(request.Candidates)
	if err != nil {
		return "", p.reject(ctx, provider, trusted, "", err)
	}
	metadata := request.Options.Metadata
	scope := metadataString(metadata, metadataCallerScope)
	session := metadataString(metadata, metadataSession)
	parent := metadataString(metadata, metadataParentSession)
	if scope == "" {
		// The caller scope is what keeps two users' sessions apart. Without it
		// no session identity is usable.
		return "", p.reject(ctx, provider, trusted, "", ErrUnavailable)
	}
	conversation, rootSlotKey := "", ""
	if session != "" {
		if conversation = ConversationHash(scope, p.roots.Resolve(scope, session, parent)); conversation != "" {
			rootSlotKey = "root:" + conversation
		}
	}

	// Admission and slot acquisition are not atomic: the Gateway selection runs
	// between them without any lock held. Losing that race is admission, not
	// an upstream result, so the account is excluded and allocation repeats.
	var (
		chosen  candidateAccount
		bindKey bindingKey
		release func(SlotOutcome)
	)
	// excluded records why an account dropped out of this pick: a lost slot
	// race (ErrConcurrency) or a route that could not be confirmed.
	excluded := map[string]error{}
	for {
		chosen, bindKey, err = p.allocate(ctx, provider, trusted, candidates, excluded, scope, session, parent, request.Model, rootSlotKey)
		if err != nil {
			return "", p.reject(ctx, provider, trusted, conversation, err)
		}
		if release, ok = p.concurrency.Acquire(chosen.account.ID, rootSlotKey); !ok {
			excluded[chosen.account.ID] = ErrConcurrency
			continue
		}
		// An account whose route cannot be confirmed is skipped like a full
		// one: the request moves on to another permitted account, never to an
		// unbound route.
		if errEgress := p.ensureEgress(ctx, provider, chosen.account); errEgress != nil {
			release(SlotKeep)
			excluded[chosen.account.ID] = ErrUnavailable
			continue
		}
		break
	}
	next := &attempt{
		provider: provider, account: chosen.account.ID, rootKey: rootSlotKey, bindKey: bindKey, bound: session != "",
		selectionID: metadataString(metadata, metadataSelectionID), controlVersion: p.control.Version(chosen.account.ID), release: release,
	}
	if err = p.alloc.Report(ctx, provider, Attempt{Token: trusted.Token, UserID: trusted.UserID, AccountID: chosen.account.ID, ConversationHash: conversation}); err != nil {
		// The Gateway could not record the decision, so it could not bill or
		// attribute the request either.
		release(SlotKeep)
		return "", ErrUnavailable
	}
	if next.bound {
		p.bindings.Set(bindKey, chosen.account.ID)
	}
	if previous := p.requests.replace(trusted.Token, next); previous != nil {
		p.finishAttempt(previous, SlotDrop)
	}
	return chosen.authID, nil
}

// resolveCandidates maps candidates to routable accounts. All candidates must
// belong to one supported provider: the Gateway callback is derived from it.
func (p *Plugin) resolveCandidates(candidates []pluginapi.SchedulerAuthCandidate) (string, []candidateAccount, error) {
	if len(candidates) == 0 {
		return "", nil, ErrUnavailable
	}
	provider := ""
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		current := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if accountsPath(current) == "" || provider != "" && provider != current {
			return "", nil, ErrUnavailable
		}
		provider = current
		ids = append(ids, candidate.ID)
	}
	resolved, err := p.inventory.Resolve(ids)
	if err != nil {
		return provider, nil, ErrUnavailable
	}
	result := make([]candidateAccount, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		account := resolved[id]
		if account == nil || account.Provider != provider || account.Duplicate || account.Disabled ||
			seen[account.ID] || p.control.Blocked(account.ID) {
			continue
		}
		seen[account.ID] = true
		result = append(result, candidateAccount{authID: id, account: account})
	}
	if len(result) == 0 || len(result) > maxCandidates {
		return provider, nil, ErrUnavailable
	}
	return provider, result, nil
}

// allocate applies, in order: the request's account-pool scope, the user's
// eligibility as the Gateway reports it now, per-account concurrency, the
// conversation's existing binding, and finally a weighted Gateway selection
// walked through the funding layers in billing order.
func (p *Plugin) allocate(ctx context.Context, provider string, trusted Trusted, candidates []candidateAccount, excluded map[string]error, scope, session, parent, model, rootSlotKey string) (candidateAccount, bindingKey, error) {
	bindKey := newBindingKey(scope, provider, session, model)
	if len(excluded) > 0 {
		remaining := candidates[:0:0]
		outcome := ErrUnavailable
		for _, candidate := range candidates {
			reason, out := excluded[candidate.account.ID]
			if !out {
				remaining = append(remaining, candidate)
			} else if errors.Is(reason, ErrConcurrency) {
				// A full account is worth retrying; an unroutable one is not.
				outcome = ErrConcurrency
			}
		}
		if candidates = remaining; len(candidates) == 0 {
			return candidateAccount{}, bindKey, outcome
		}
	}
	if trusted.Scope.Present && trusted.Scope.Restricted {
		scoped := candidates[:0:0]
		for _, candidate := range candidates {
			if trusted.Scope.IDs[candidate.account.ID] {
				scoped = append(scoped, candidate)
			}
		}
		if candidates = scoped; len(candidates) == 0 {
			return candidateAccount{}, bindKey, ErrConcurrency
		}
	}
	byID := make(map[string]candidateAccount, len(candidates))
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.account.ID] = candidate
		ids = append(ids, candidate.account.ID)
	}
	eligible, err := p.alloc.Eligible(ctx, provider, trusted.UserID, ids)
	if err != nil {
		return candidateAccount{}, bindKey, ErrUnavailable
	}
	permitted := make([]string, 0, len(eligible))
	for _, item := range eligible {
		p.concurrency.SetLimit(item.ID, item.ConcurrentLimit)
		permitted = append(permitted, item.ID)
	}
	if len(permitted) == 0 {
		p.unbind(bindKey, session, "")
		return candidateAccount{}, bindKey, ErrUnavailable
	}
	admissible := p.concurrency.Admissible(rootSlotKey, permitted)
	if len(admissible) == 0 {
		return candidateAccount{}, bindKey, ErrConcurrency
	}
	allowed := make(map[string]bool, len(admissible))
	for _, id := range admissible {
		allowed[id] = true
	}
	if session != "" {
		if bound, ok := p.bindings.Get(bindKey); ok {
			if allowed[bound] {
				return byID[bound], bindKey, nil
			}
			// The bound account is no longer permitted or has no free slot:
			// unbind this conversation only and allocate again.
			p.bindings.Delete(bindKey, bound)
		} else if parent != "" {
			// A subagent with no binding of its own follows its parent.
			if bound, ok = p.bindings.Get(newBindingKey(scope, provider, parent, model)); ok && allowed[bound] {
				return byID[bound], bindKey, nil
			}
		}
	}
	selected, err := p.selectLayered(ctx, provider, trusted, admissible)
	if err != nil {
		return candidateAccount{}, bindKey, err
	}
	return byID[selected], bindKey, nil
}

// selectLayered asks the Gateway to choose within the first funding layer that
// still has a candidate. A 429 means that layer is exhausted, so the next one
// is tried; it never falls back outside the admitted layers.
func (p *Plugin) selectLayered(ctx context.Context, provider string, trusted Trusted, admissible []string) (string, error) {
	if !trusted.Scope.Present || len(trusted.Scope.Layers) == 0 {
		return p.alloc.Select(ctx, provider, trusted.UserID, admissible)
	}
	for _, layer := range trusted.Scope.Layers {
		inLayer := admissible
		if !layer.All {
			allowed := make(map[string]bool, len(layer.AccountIDs))
			for _, id := range layer.AccountIDs {
				allowed[id] = true
			}
			inLayer = make([]string, 0, len(admissible))
			for _, id := range admissible {
				if allowed[id] {
					inLayer = append(inLayer, id)
				}
			}
		}
		if len(inLayer) == 0 {
			continue
		}
		selected, err := p.alloc.Select(ctx, provider, trusted.UserID, inLayer)
		if err == nil {
			return selected, nil
		}
		if !errors.Is(err, ErrConcurrency) {
			return "", err
		}
	}
	return "", ErrConcurrency
}

// unbind removes a conversation's binding. With account set it only removes a
// binding to that account.
func (p *Plugin) unbind(key bindingKey, session, account string) {
	if session == "" {
		return
	}
	if account == "" {
		if bound, ok := p.bindings.Get(key); ok {
			p.bindings.Delete(key, bound)
		}
		return
	}
	p.bindings.Delete(key, account)
}

// finishAttempt gives an attempt's slot back. An account-attributed failure
// also unbinds the conversation from that account.
func (p *Plugin) finishAttempt(finished *attempt, outcome SlotOutcome) {
	if finished == nil {
		return
	}
	if outcome == SlotDrop && finished.bound {
		p.bindings.Delete(finished.bindKey, finished.account)
	}
	if finished.release != nil {
		finished.release(outcome)
	}
}

// reject reports a refused allocation to the Gateway on a best-effort basis
// and returns the outcome. The report lets the Gateway tell "no account is
// free" from a real upstream usage limit without parsing CPA's error body.
func (p *Plugin) reject(ctx context.Context, provider string, trusted Trusted, conversation string, outcome error) error {
	if !errors.Is(outcome, ErrConcurrency) {
		outcome = ErrUnavailable
	}
	if provider != "" && trusted.Token != "" {
		reportCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_ = p.alloc.Report(reportCtx, provider, Attempt{Token: trusted.Token, UserID: trusted.UserID, ConversationHash: conversation, Rejected: outcome.Error()})
		cancel()
	}
	return outcome
}
