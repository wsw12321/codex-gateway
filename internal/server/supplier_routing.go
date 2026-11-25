package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

const supplierStickyTTL = 5 * time.Minute
const mixedDispatchScopeID = "00000000-0000-4000-8000-000000000001"

type supplierSlot struct {
	active int
	until  time.Time
}

type mixedBinding struct {
	account string
	until   time.Time
}

type supplierRuntime struct {
	mu       sync.Mutex
	slots    map[string]map[string]supplierSlot
	bindings map[string]mixedBinding
	cooldown map[string]time.Time
}

func (runtime *supplierRuntime) clearCooldown(id string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	delete(runtime.cooldown, id)
}

func (runtime *supplierRuntime) dropIdleLocked(id string) {
	for root, slot := range runtime.slots[id] {
		if slot.active == 0 {
			delete(runtime.slots[id], root)
		} else {
			slot.until = time.Time{}
			runtime.slots[id][root] = slot
		}
	}
	for key, binding := range runtime.bindings {
		if binding.account == id {
			delete(runtime.bindings, key)
		}
	}
}

func (runtime *supplierRuntime) active(id string) int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	var active int
	for _, slot := range runtime.slots[id] {
		active += slot.active
	}
	return active
}

func (runtime *supplierRuntime) slotAvailableLocked(id, root string, limit int, now time.Time) bool {
	if runtime.cooldown[id].After(now) {
		return false
	}
	delete(runtime.cooldown, id)
	for key, slot := range runtime.slots[id] {
		if slot.active == 0 && !slot.until.After(now) {
			delete(runtime.slots[id], key)
		}
	}
	_, bound := runtime.slots[id][root]
	return bound || len(runtime.slots[id]) < limit
}

func (runtime *supplierRuntime) binding(key string, now time.Time) string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if binding := runtime.bindings[key]; binding.until.After(now) {
		return binding.account
	}
	delete(runtime.bindings, key)
	return ""
}

func (runtime *supplierRuntime) bind(key, id string, now time.Time) {
	if key == "" {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.bindings == nil {
		runtime.bindings = make(map[string]mixedBinding)
	}
	if len(runtime.bindings) >= 8192 {
		for key, binding := range runtime.bindings {
			if !binding.until.After(now) {
				delete(runtime.bindings, key)
			}
		}
		if len(runtime.bindings) >= 8192 {
			return
		}
	}
	runtime.bindings[key] = mixedBinding{id, now.Add(supplierStickyTTL)}
}

func (s *Server) acquireSupplier(ctx context.Context, provider, id, model, root string, limit int) (store.ModelSupplier, bool, error) {
	runtime := &s.supplierRuntime
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	now := time.Now()
	if !runtime.slotAvailableLocked(id, root, limit, now) {
		return store.ModelSupplier{}, false, nil
	}
	// The caller has just checked the supplier's eligibility in a layer of
	// this request, which is what lets a member's supplier be found here.
	value, err := s.findRoutableModelSupplier(ctx, provider, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && (!value.Enabled || !slices.Contains(value.Models, model)) {
		return store.ModelSupplier{}, false, nil
	}
	if err != nil {
		return store.ModelSupplier{}, false, err
	}
	if runtime.slots == nil {
		runtime.slots = make(map[string]map[string]supplierSlot)
	}
	if runtime.slots[id] == nil {
		runtime.slots[id] = make(map[string]supplierSlot)
	}
	slot := runtime.slots[id][root]
	slot.active++
	slot.until = now.Add(supplierStickyTTL)
	runtime.slots[id][root] = slot
	return value, true, nil
}

func (runtime *supplierRuntime) release(id, root string, sticky bool, failure *gatewayproxy.Failure) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	slot := runtime.slots[id][root]
	slot.active--
	if failure != nil && failure.Code == "supplier_rate_limited" {
		if runtime.cooldown == nil {
			runtime.cooldown = make(map[string]time.Time)
		}
		runtime.cooldown[id] = time.Now().Add(time.Duration(min(max(failure.RetryAfter, 5), 300)) * time.Second)
	}
	if slot.active == 0 && (!sticky || failure != nil || slot.until.IsZero()) {
		delete(runtime.slots[id], root)
	} else {
		if failure == nil && !slot.until.IsZero() {
			slot.until = time.Now().Add(supplierStickyTTL)
		}
		runtime.slots[id][root] = slot
	}
}

func mixedConversation(header http.Header, body *countingBody, scope string) string {
	if scope == "" {
		return ""
	}
	digest := func(value string) string {
		if value == "" || len(value) > 512 {
			return ""
		}
		fingerprint := sha256.Sum256([]byte("gateway-mixed-root\x00" + scope + "\x00" + value))
		return "conv-" + hex.EncodeToString(fingerprint[:16])
	}
	for _, names := range [][]string{
		{"X-Claude-Code-Session-Id"}, {"X-Codex-Parent-Thread-Id", "Thread-Id", "Thread_id", "Session-Id", "Session_id"},
		{"X-Parent-Session-Id", "X-Http-Session-Id", "X-Session-Id", "X-Session-Affinity", "X-Slot-Session-Id"},
		{"X-Parent-Conversation-Id", "X-Conversation-Id", "X-Parent-Thread-Id", "X-Thread-Id"},
	} {
		for _, name := range names {
			if hash := digest(strings.TrimSpace(header.Get(name))); hash != "" {
				return hash
			}
		}
	}
	if body != nil && body.replay != nil && body.replayBytes <= 64<<10 {
		var input struct {
			ThreadID       string `json:"thread_id"`
			SessionID      string `json:"session_id"`
			PromptCacheKey string `json:"prompt_cache_key"`
			ConversationID string `json:"conversation_id"`
			Metadata       struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if json.NewDecoder(io.LimitReader(body.replay(), 64<<10)).Decode(&input) == nil {
			var claude struct {
				SessionID string `json:"session_id"`
			}
			_ = json.Unmarshal([]byte(input.Metadata.UserID), &claude)
			if claude.SessionID == "" {
				_, claude.SessionID, _ = strings.Cut(input.Metadata.UserID, "_session_")
			}
			for _, value := range []string{claude.SessionID, input.ThreadID, input.SessionID, input.PromptCacheKey, input.ConversationID} {
				if hash := digest(strings.TrimSpace(value)); hash != "" {
					return hash
				}
			}
		}
	}
	return ""
}

func (s *Server) mixedClient(provider string) *gatewayproxy.Client {
	switch provider {
	case store.UpstreamProviderAnthropic:
		return s.anthropic
	case store.UpstreamProviderAntigravity:
		return s.antigravity
	default:
		return s.upstream
	}
}

// mixedOfficialScope pins an official account to the layer it was selected
// in, so CPA checks it under that layer's rules again.
func mixedOfficialScope(groups []gatewayproxy.AllocationGroup, layer int, account string) []gatewayproxy.AllocationGroup {
	if layer >= 0 && layer < len(groups) && !groups[layer].AllAccounts {
		return []gatewayproxy.AllocationGroup{{GroupID: groups[layer].GroupID, AccountIDs: []string{account}, Tenant: groups[layer].Tenant}}
	}
	return []gatewayproxy.AllocationGroup{{GroupID: mixedDispatchScopeID, AccountIDs: []string{account}}}
}

func (s *Server) findRoutableModelSupplier(ctx context.Context, provider, id string) (store.ModelSupplier, error) {
	values, err := s.store.WithUpstreamProvider(provider).RoutableModelSuppliers(ctx, []string{id})
	if err != nil {
		return store.ModelSupplier{}, err
	}
	for _, value := range values {
		if value.ID == id {
			return value, nil
		}
	}
	return store.ModelSupplier{}, store.ErrNotFound
}

// memberLayerAccounts lists the accounts of the layers that hold members'
// accounts (self-use and seller plans).
func memberLayerAccounts(groups []gatewayproxy.AllocationGroup) []string {
	var ids []string
	for _, group := range groups {
		if group.Tenant {
			for _, id := range group.AccountIDs {
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

func (s *Server) officialCandidates(ctx context.Context, provider string) []string {
	client := s.mixedClient(provider)
	if client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	accounts, err := client.ListUpstreamAccounts(ctx)
	if err != nil {
		return nil
	}
	var ids []string
	for _, account := range accounts {
		if account.Status == "available" && upstreamAccountSourceStatusKnown(account) {
			ids = append(ids, account.ID)
		}
	}
	return ids
}

func (s *Server) responseSupplierCatalog(ctx context.Context, user string, allowed map[string]struct{}) []string {
	if !s.config.SuppliersEnabled {
		return nil
	}
	repo := s.store.WithUpstreamProvider(store.UpstreamProviderCodex)
	// Members' suppliers count only when one of the user's member layers
	// (self-use or a seller plan) reaches them now; any failure hides them.
	reachable, err := repo.TenantReachableAccounts(ctx, user)
	if err != nil {
		reachable = nil
	}
	values, err := repo.RoutableModelSuppliers(ctx, reachable)
	if err != nil {
		return nil
	}
	var platform []string
	byID := make(map[string][]string)
	usable := make(map[string]bool)
	for _, value := range values {
		if !value.Enabled {
			continue
		}
		byID[value.ID] = value.Models
		if slices.Contains(reachable, value.ID) {
			usable[value.ID] = true
		} else {
			platform = append(platform, value.ID)
		}
	}
	if len(platform) > store.MaxUpstreamAllocationCandidates {
		return nil
	}
	if len(platform) > 0 {
		eligible, err := repo.EligibleUpstreamAccountLimits(ctx, user, platform)
		if err != nil {
			return nil
		}
		for _, item := range eligible {
			usable[item.ID] = true
		}
	}
	var models []string
	for id := range usable {
		for _, model := range byID[id] {
			if _, ok := allowed[model]; ok && !slices.Contains(models, model) {
				models = append(models, model)
			}
		}
	}
	slices.Sort(models)
	return models
}

func mixedUnavailable() *gatewayproxy.Failure {
	return &gatewayproxy.Failure{Status: 503, Code: "upstream_allocation_unavailable", Type: "server_error", Message: "暂无可用的上游账号"}
}

func (s *Server) forwardMixed(w http.ResponseWriter, r *http.Request, prepared preparedAPIRequest, provider string, options gatewayproxy.ForwardOptions, official func(http.ResponseWriter, *http.Request, gatewayproxy.ForwardOptions) (gatewayproxy.Result, *gatewayproxy.Failure)) (gatewayproxy.Result, *gatewayproxy.Failure, bool) {
	if !s.config.SuppliersEnabled || r.Method != http.MethodPost || prepared.body == nil || prepared.body.replay == nil ||
		!(prepared.anthropic || prepared.gemini || provider == store.UpstreamProviderCodex && prepared.upstreamPath == "/v1/responses") {
		return gatewayproxy.Result{}, nil, false
	}
	ctx := r.Context()
	repo := s.store.WithUpstreamProvider(provider)
	values, err := repo.RoutableModelSuppliers(ctx, memberLayerAccounts(options.Groups))
	if err != nil {
		return gatewayproxy.Result{}, mixedUnavailable(), true
	}
	suppliers := make(map[string]bool)
	var candidates []string
	for _, value := range values {
		if value.Enabled && slices.Contains(value.Models, prepared.model) {
			suppliers[value.ID] = true
			candidates = append(candidates, value.ID)
		}
	}
	if len(candidates) == 0 {
		return gatewayproxy.Result{}, nil, false
	}
	for _, id := range s.officialCandidates(ctx, provider) {
		if !slices.Contains(candidates, id) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) > store.MaxUpstreamAllocationCandidates {
		return gatewayproxy.Result{}, mixedUnavailable(), true
	}
	// Eligibility is judged per funding layer, exactly as for CPA's own
	// accounts: a member's supplier or account only in the layer that pays.
	groups := options.Groups
	if len(groups) == 0 {
		groups = []gatewayproxy.AllocationGroup{{GroupID: gatewayproxy.PersonalLayerID, AccountIDs: []string{}, AllAccounts: true}}
	}
	asked := make([]store.UpstreamAllocationGroup, 0, len(groups))
	for _, group := range groups {
		var ids []string
		for _, candidate := range candidates {
			if group.AllAccounts || slices.Contains(group.AccountIDs, candidate) {
				ids = append(ids, candidate)
			}
		}
		asked = append(asked, store.UpstreamAllocationGroup{GroupID: group.GroupID, AccountIDs: ids})
	}
	eligible, err := repo.LayerEligibleAccounts(ctx, options.UserID, asked)
	if err != nil {
		return gatewayproxy.Result{}, mixedUnavailable(), true
	}
	limits := make(map[string]int)
	inLayer := make([]map[string]bool, len(eligible))
	for i, layer := range eligible {
		inLayer[i] = make(map[string]bool, len(layer.Accounts))
		for _, item := range layer.Accounts {
			inLayer[i][item.ID] = true
			limits[item.ID] = item.ConcurrentLimit
		}
	}
	conversation := mixedConversation(r.Header, prepared.body, options.AffinityScope)
	root := conversation
	bindKey := ""
	if root == "" {
		root = httpx.RequestID(ctx)
	} else {
		bindKey = provider + ":" + prepared.model + ":" + root
	}
	now := time.Now()
	bound := s.supplierRuntime.binding(bindKey, now)
	var result gatewayproxy.Result
	failure := mixedUnavailable()
	for attempt := 0; attempt < 4 && len(limits) > 0 && ctx.Err() == nil; attempt++ {
		var id string
		s.supplierRuntime.mu.Lock()
		for candidate, limit := range limits {
			if suppliers[candidate] && !s.supplierRuntime.slotAvailableLocked(candidate, root, limit, time.Now()) {
				delete(limits, candidate)
				if failure.Code == "upstream_allocation_unavailable" {
					failure = &gatewayproxy.Failure{Status: 429, Code: "upstream_concurrency_exceeded", Type: "rate_limit_error", Message: "暂无可用的上游账号，请稍后重试", RetryAfter: 5}
				}
			}
		}
		s.supplierRuntime.mu.Unlock()
		layerOf := -1
		if limits[bound] > 0 {
			id = bound
			for i := range inLayer {
				if inLayer[i][bound] {
					layerOf = i
					break
				}
			}
		} else {
			for i, group := range groups {
				var layer []string
				for _, candidate := range candidates {
					if limits[candidate] > 0 && inLayer[i][candidate] {
						layer = append(layer, candidate)
					}
				}
				if len(layer) == 0 {
					continue
				}
				id, err = repo.SelectLayerUpstreamAccount(ctx, options.UserID, store.UpstreamAllocationGroup{GroupID: group.GroupID, AccountIDs: layer}, time.Now())
				if err != nil && !errors.Is(err, store.ErrNoUpstreamAccount) {
					return result, mixedUnavailable(), true
				}
				if id != "" {
					layerOf = i
					break
				}
			}
		}
		if id == "" {
			break
		}
		limit := limits[id]
		delete(limits, id)
		incoming := r.Clone(ctx)
		incoming.ContentLength = prepared.body.replayBytes
		incoming.Body = io.NopCloser(prepared.body.replay())
		if suppliers[id] {
			value, acquired, err := s.acquireSupplier(ctx, provider, id, prepared.model, root, limit)
			if err != nil {
				return result, mixedUnavailable(), true
			}
			if !acquired {
				continue
			}
			result, failure = func() (result gatewayproxy.Result, failure *gatewayproxy.Failure) {
				defer func() {
					s.supplierRuntime.release(id, root, conversation != "" && !prepared.countTokens && !result.CompletedAt.IsZero(), failure)
				}()
				return s.dispatchSupplier(w, incoming, prepared, value, conversation)
			}()
		} else {
			pinned := options
			pinned.Groups = mixedOfficialScope(groups, layerOf, id)
			result, failure = official(w, incoming, pinned)
		}
		if failure == nil {
			if !prepared.countTokens {
				s.supplierRuntime.bind(bindKey, id, time.Now())
			}
			return result, nil, true
		}
		if !result.FirstByteAt.IsZero() || result.BytesOut != 0 || result.AbortStream || !mixedRetryable(failure) {
			return result, failure, true
		}
	}
	return result, failure, true
}

func mixedRetryable(failure *gatewayproxy.Failure) bool {
	switch failure.Code {
	case "supplier_rate_limited", "supplier_egress_unavailable", "supplier_unavailable", "supplier_credentials_rejected",
		"upstream_allocation_unavailable", "upstream_concurrency_exceeded", "upstream_access_protocol_unavailable":
		return true
	}
	return failure.Status == 429 || failure.Status >= 500 && failure.Code != "upstream_unavailable" && failure.Code != "upstream_timeout"
}

func (s *Server) dispatchSupplier(w http.ResponseWriter, incoming *http.Request, prepared preparedAPIRequest, value store.ModelSupplier, conversation string) (gatewayproxy.Result, *gatewayproxy.Failure) {
	ctx := incoming.Context()
	failure := mixedUnavailable()
	failure.Code = "supplier_egress_unavailable"
	base, valid := egress.SupplierBaseURL(value.BaseURL, supplierVersion(value.Provider))
	if !valid {
		return gatewayproxy.Result{}, failure
	}
	key, err := security.DecryptSupplierKey(s.config.APIKeyEncryptionKey, value.ID, value.Ciphertext)
	if err != nil {
		return gatewayproxy.Result{}, failure
	}
	route, err := s.resolveAccountEgress(ctx, value.Provider, value.ID, "")
	if err != nil {
		return gatewayproxy.Result{}, failure
	}
	transport, err := s.supplierTransport(value.ID, base.Hostname(), route)
	if err != nil {
		return gatewayproxy.Result{}, failure
	}
	defer transport.CloseIdleConnections()
	base.Path += strings.TrimPrefix(prepared.upstreamPath, "/"+supplierVersion(value.Provider))
	if prepared.anthropic {
		base.RawQuery = incoming.URL.RawQuery
	} else if prepared.gemini && strings.HasSuffix(prepared.upstreamPath, ":streamGenerateContent") {
		base.RawQuery = "alt=sse"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), incoming.Body)
	if err != nil {
		return gatewayproxy.Result{}, failure
	}
	request.ContentLength = incoming.ContentLength
	request.Header.Set("Content-Type", "application/json")
	for _, name := range []string{"Accept", "Anthropic-Version", "Anthropic-Beta", "OpenAI-Beta"} {
		if strings.HasPrefix(name, "Anthropic-") && !prepared.anthropic || name == "OpenAI-Beta" && value.Provider != store.UpstreamProviderCodex {
			continue
		}
		for _, value := range incoming.Header.Values(name) {
			request.Header.Add(name, value)
		}
	}
	supplierHeaders(request.Header, value.Provider, key)
	s.rememberActiveAttribution(httpx.RequestID(ctx), value.ID)
	s.rememberActiveConversation(httpx.RequestID(ctx), conversation)
	return gatewayproxy.ForwardSupplier(ctx, w, request, supplierHTTP(transport), value.Provider, prepared.upstreamPath, prepared.model, value.ID, conversation)
}
