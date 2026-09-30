package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/config"
)

type managedAccount struct {
	record     AccountRecord
	runner     Executor
	ready      bool
	models     map[string]bool
	refreshing bool
	// active tracks executions for readiness; slots count distinct conversations
	// plus requests without a trusted conversation identity. All use manager.mu.
	active        int64
	conversations map[string]int64
	unattributed  int64
	cooldown      time.Time
	quota         bool
}

func (a *managedAccount) activeSlots() int64 {
	return int64(len(a.conversations)) + a.unattributed
}

func (a *managedAccount) canAcquire(conversation string, limit int64) bool {
	return limit > 0 && (a.conversations[conversation] > 0 || a.activeSlots() < limit)
}

func (a *managedAccount) reserve(conversation string) {
	a.active++
	if conversation == "" {
		a.unattributed++
		return
	}
	if a.conversations == nil {
		a.conversations = make(map[string]int64)
	}
	a.conversations[conversation]++
}

func (a *managedAccount) release(conversation string) {
	a.active--
	if conversation == "" {
		a.unattributed--
	} else if a.conversations[conversation] > 1 {
		a.conversations[conversation]--
	} else {
		delete(a.conversations, conversation)
	}
}

type AccountManager struct {
	mu         sync.Mutex
	selectGate chan struct{}
	registry   *AccountRegistry
	accounts   []*managedAccount
	gateway    string
	token      string
	client     *http.Client
}

type accountRequestKey struct{}

var errAccountSelectionBusy = errors.New("account selection busy")

type accountRequest struct {
	userID           string
	conversationHash string
	directName       string
	selected         *string
	release          *func()
}

func NewAccountServer(runner Runner, registry *AccountRegistry, token, gatewayURL string) (*Server, error) {
	if registry == nil {
		return nil, errors.New("Antigravity account registry is required")
	}
	if gatewayURL == "" {
		gatewayURL = "http://gateway:8080"
	}
	parsed, err := url.Parse(gatewayURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("invalid Antigravity gateway URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Allocation is private service traffic. The CLI's internet egress proxy
	// must never receive callback credentials or decide account permissions.
	transport.Proxy = nil
	manager := &AccountManager{
		registry: registry, gateway: strings.TrimRight(gatewayURL, "/"), token: token,
		selectGate: make(chan struct{}, 1),
		client:     &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	for _, record := range registry.Records() {
		copy := runner
		copy.Credentials = AccountCredentials(record.Name)
		manager.accounts = append(manager.accounts, &managedAccount{record: record, runner: copy})
	}
	server := NewServer(manager, token)
	server.manager = manager
	// Bound request buffers and CLI process count as well as the individual
	// account limits obtained from the gateway on every request.
	server.gate = make(chan struct{}, 64)
	return server, nil
}

func (m *AccountManager) Check(ctx context.Context) ([]string, error) {
	m.Refresh(ctx)
	if !m.Ready() {
		return nil, errors.New("Antigravity accounts unavailable")
	}
	models, failure := m.Models(ctx)
	if failure != nil {
		return nil, errors.New("Antigravity accounts unavailable")
	}
	return models, nil
}

func (m *AccountManager) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, account := range m.accounts {
		if account.record.Enabled && account.ready && !time.Now().Before(account.cooldown) {
			return true
		}
	}
	return false
}

func (m *AccountManager) Refresh(ctx context.Context) {
	for _, account := range m.accounts {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		if account.active != 0 || account.refreshing || time.Now().Before(account.cooldown) {
			m.mu.Unlock()
			continue
		}
		account.refreshing = true
		m.mu.Unlock()
		models, err := account.runner.Check(ctx)
		m.mu.Lock()
		account.models = modelSet(models)
		account.ready = err == nil && len(account.models) > 0
		if !account.ready {
			account.models = nil
		}
		account.refreshing = false
		m.mu.Unlock()
	}
}

// Models advertises the union of ready accounts the authenticated user may
// access. Discovery does not reserve concurrency slots or update account LRU.
// Internal requests without a user identity can inspect the ready account pool.
func (m *AccountManager) Models(ctx context.Context) ([]string, *Failure) {
	request, _ := ctx.Value(accountRequestKey{}).(accountRequest)
	if request.userID != "" {
		id, err := uuid.Parse(request.userID)
		if err != nil || id.String() != request.userID {
			return nil, allocationFailure()
		}
	}
	m.mu.Lock()
	ids := []string{}
	for _, account := range m.accounts {
		if account.record.Enabled && account.ready && !time.Now().Before(account.cooldown) &&
			(request.directName == "" || account.record.Name == request.directName) {
			ids = append(ids, account.record.ID)
		}
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return nil, credentialFailure()
	}
	allowed := map[string]int64{}
	if request.userID != "" {
		var failure *Failure
		allowed, failure = m.eligibleLimits(ctx, request.userID, ids)
		if failure != nil {
			return nil, failure
		}
	} else {
		for _, id := range ids {
			allowed[id] = 1
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	available := map[string]bool{}
	for _, account := range m.accounts {
		if allowed[account.record.ID] > 0 && account.record.Enabled && account.ready && !time.Now().Before(account.cooldown) {
			for model := range account.models {
				available[model] = true
			}
		}
	}
	return orderedModels(available), nil
}

func (m *AccountManager) eligibleLimits(ctx context.Context, userID string, ids []string) (map[string]int64, *Failure) {
	var eligible struct {
		Accounts *[]struct {
			ID    string `json:"id"`
			Limit int64  `json:"concurrent_limit"`
		} `json:"accounts"`
	}
	if err := m.callback(ctx, "eligible", userID, ids, &eligible); err != nil || eligible.Accounts == nil {
		return nil, allocationFailure()
	}
	candidates := map[string]bool{}
	for _, id := range ids {
		candidates[id] = true
	}
	limits := map[string]int64{}
	for _, account := range *eligible.Accounts {
		if !candidates[account.ID] || limits[account.ID] != 0 || account.Limit < 1 || account.Limit > 2147483647 {
			return nil, allocationFailure()
		}
		limits[account.ID] = account.Limit
	}
	return limits, nil
}

func allocationFailure() *Failure {
	return &Failure{503, "upstream_allocation_unavailable", "Antigravity account allocation is unavailable"}
}

func accountBusyFailure() *Failure {
	return &Failure{429, "upstream_concurrency_exceeded", "No Antigravity account is currently available"}
}

func (m *AccountManager) Run(ctx context.Context, model, prompt string) (Result, *Failure) {
	return m.RunStream(ctx, model, prompt, nil)
}

func (m *AccountManager) RunStream(ctx context.Context, model, prompt string, emit func(string) error) (Result, *Failure) {
	if !config.IsLegacyAntigravityModel(model) {
		return Result{}, unsupported("model")
	}
	request, ok := ctx.Value(accountRequestKey{}).(accountRequest)
	if !ok {
		return Result{}, allocationFailure()
	}
	if request.directName == "" {
		id, err := uuid.Parse(request.userID)
		if err != nil || id.String() != request.userID {
			return Result{}, allocationFailure()
		}
	}
	tried := map[string]bool{}
	var last *Failure
	for {
		account, failure := m.acquire(ctx, request, model, tried)
		if failure != nil {
			// A selector outage always fails closed, even after a provider failure.
			if last != nil && failure.Code == "upstream_concurrency_exceeded" {
				return Result{}, last
			}
			return Result{}, failure
		}
		tried[account.record.ID] = true
		if request.selected != nil {
			*request.selected = account.record.ID
		}
		release := sync.OnceFunc(func() {
			m.mu.Lock()
			account.release(request.conversationHash)
			m.mu.Unlock()
		})
		if request.release != nil {
			// Install before executing so ServeHTTP also releases after a panic,
			// cancellation, final error, or blocked response write.
			*request.release = release
		} else {
			defer release()
		}
		var result Result
		started := false
		if streaming, ok := account.runner.(StreamingExecutor); ok && emit != nil {
			result, failure = streaming.RunStream(ctx, model, prompt, func(text string) error {
				started = true
				return emit(text)
			})
		} else {
			result, failure = account.runner.Run(ctx, model, prompt)
		}
		m.mu.Lock()
		if failure == nil {
			// A sibling request may have observed quota/auth failure after this
			// request began. Its active cooldown must survive a late success.
			if !time.Now().Before(account.cooldown) {
				account.ready = true
				account.quota = false
				account.cooldown = time.Time{}
			}
		} else if failure.Status == 429 {
			account.quota = true
			account.cooldown = time.Now().Add(time.Minute)
		} else if failure.Status == 401 || failure.Status == 403 || failure.Status == 503 {
			account.ready = false
			account.cooldown = time.Now().Add(time.Minute)
		}
		m.mu.Unlock()
		if failure == nil || started || ctx.Err() != nil || request.directName != "" || (failure.Status != 429 && failure.Status != 401 && failure.Status != 403 && failure.Status != 503) {
			return result, failure
		}
		release()
		if request.release != nil {
			*request.release = nil
		}
		// Retry only before the first stream update: a response must never mix
		// output or account attribution from different attempts.
		last = failure
	}
}

func (m *AccountManager) acquire(ctx context.Context, request accountRequest, model string, tried map[string]bool) (*managedAccount, *Failure) {
	select {
	case m.selectGate <- struct{}{}:
		defer func() { <-m.selectGate }()
	case <-ctx.Done():
		return nil, allocationFailure()
	}
	m.mu.Lock()
	ids := []string{}
	for _, account := range m.accounts {
		if request.directName != "" && account.record.Name != request.directName {
			continue
		}
		if account.record.Enabled && account.ready && account.models[model] && !account.refreshing && !tried[account.record.ID] && !time.Now().Before(account.cooldown) {
			ids = append(ids, account.record.ID)
		}
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return nil, accountBusyFailure()
	}
	limits := map[string]int64{}
	selected := ""
	if request.directName != "" {
		selected, limits[ids[0]] = ids[0], 1
	} else {
		var failure *Failure
		limits, failure = m.eligibleLimits(ctx, request.userID, ids)
		if failure != nil {
			return nil, failure
		}
		// Only live references create affinity. Recheck eligibility on every
		// request and keep using the selector so zero weights still drain accounts.
		preferred, remaining := []string{}, []string{}
		m.mu.Lock()
		for _, account := range m.accounts {
			if account.canAcquire(request.conversationHash, limits[account.record.ID]) && account.record.Enabled && account.ready && account.models[model] && !account.refreshing && !tried[account.record.ID] && !time.Now().Before(account.cooldown) {
				if account.conversations[request.conversationHash] > 0 {
					preferred = append(preferred, account.record.ID)
				} else {
					remaining = append(remaining, account.record.ID)
				}
			}
		}
		m.mu.Unlock()
		for _, candidates := range [][]string{preferred, remaining} {
			if len(candidates) == 0 {
				continue
			}
			selected, failure = m.selectAccount(ctx, request.userID, candidates)
			if failure != nil {
				if failure.Code == "upstream_concurrency_exceeded" {
					continue
				}
				return nil, failure
			}
			break
		}
		if selected == "" {
			return nil, accountBusyFailure()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, account := range m.accounts {
		// A previous request may have released the last reference during the
		// callback. In that case this request must acquire a fresh slot.
		if account.record.ID == selected && account.record.Enabled && account.ready && account.models[model] && !account.refreshing && !tried[selected] && account.canAcquire(request.conversationHash, limits[selected]) && !time.Now().Before(account.cooldown) {
			account.reserve(request.conversationHash)
			return account, nil
		}
	}
	return nil, accountBusyFailure()
}

func (m *AccountManager) selectAccount(ctx context.Context, userID string, ids []string) (string, *Failure) {
	var selection struct {
		ID string `json:"account_id"`
	}
	if err := m.callback(ctx, "select", userID, ids, &selection); err != nil {
		if errors.Is(err, errAccountSelectionBusy) {
			return "", accountBusyFailure()
		}
		return "", allocationFailure()
	}
	for _, id := range ids {
		if id == selection.ID {
			return id, nil
		}
	}
	return "", allocationFailure()
}

func (m *AccountManager) callback(ctx context.Context, action, user string, ids []string, destination any) error {
	body, _ := json.Marshal(map[string]any{"user_id": user, "account_ids": ids})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.gateway+"/internal/antigravity-accounts/"+action, bytes.NewReader(body))
	if err != nil {
		return errors.New("allocation unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(req)
	if err != nil {
		return errors.New("allocation unavailable")
	}
	defer response.Body.Close()
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if (response.StatusCode != 200 && response.StatusCode != 429) || err != nil || media != "application/json" {
		return errors.New("allocation unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	if err != nil || len(data) > 16384 || uniqueJSON(data) != nil {
		return errors.New("allocation unavailable")
	}
	if response.StatusCode == 429 {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if action == "select" && json.Unmarshal(data, &failure) == nil && failure.Error.Code == "upstream_concurrency_exceeded" {
			return errAccountSelectionBusy
		}
		return errors.New("allocation unavailable")
	}
	fields, valid := object(data)
	if !valid || len(fields) != 1 {
		return errors.New("allocation unavailable")
	}
	if action == "eligible" {
		var rows []json.RawMessage
		if json.Unmarshal(fields["accounts"], &rows) != nil || rows == nil {
			return errors.New("allocation unavailable")
		}
		for _, row := range rows {
			account, ok := object(row)
			if !ok || len(account) != 2 || account["id"] == nil || account["concurrent_limit"] == nil {
				return errors.New("allocation unavailable")
			}
		}
	} else if fields["account_id"] == nil {
		return errors.New("allocation unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("allocation unavailable")
	}
	return nil
}

func (m *AccountManager) accountStatus(account *managedAccount) map[string]any {
	cli, manual, quota, status := "unavailable", "enabled", "available", "unavailable"
	if account.ready {
		cli = "active"
	}
	if !account.record.Enabled {
		manual = "manual_disabled"
	}
	if account.quota && time.Now().Before(account.cooldown) {
		quota = "quota_exhausted"
	}
	if cli == "active" && manual == "enabled" && quota == "available" {
		status = "available"
	}
	return map[string]any{"id": account.record.ID, "status": status, "cliproxy_status": cli, "gateway_manual_status": manual, "gateway_quota_status": quota}
}

// Internal endpoints are reached only after Server's bridge-token check.
func (m *AccountManager) serveInternal(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/internal/upstream-accounts") {
		return false
	}
	if r.URL.RawQuery != "" {
		writeJSON(w, 400, map[string]string{"error": "account_status_request_invalid"})
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.URL.Path == "/internal/upstream-accounts/capabilities" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]string{"protocol": "upstream_account_access_v1"})
	case r.URL.Path == "/internal/upstream-accounts" && r.Method == http.MethodGet:
		accounts := []map[string]any{}
		for _, account := range m.accounts {
			row := m.accountStatus(account)
			row["masked_email"], row["display_name"], row["plan"], row["last_synced_at"] = account.record.MaskedEmail, account.record.Name, "unknown", time.Now().UTC()
			accounts = append(accounts, row)
		}
		writeJSON(w, 200, map[string]any{"accounts": accounts})
	case r.URL.Path == "/internal/upstream-accounts/concurrency" && r.Method == http.MethodGet:
		accounts := []map[string]any{}
		for _, account := range m.accounts {
			accounts = append(accounts, map[string]any{"id": account.record.ID, "active_requests": account.activeSlots()})
		}
		writeJSON(w, 200, map[string]any{"sampled_at": time.Now().UTC(), "accounts": accounts})
	default:
		prefix := "/internal/upstream-accounts/"
		id, found := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/status")
		if !found || r.Method != http.MethodPut {
			writeJSON(w, 404, map[string]string{"error": "upstream_account_not_found"})
			return true
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 257))
		fields, valid := object(data)
		var enabled *bool
		if err != nil || media != "application/json" || readErr != nil || len(data) > 256 || uniqueJSON(data) != nil || !valid || len(fields) != 1 || json.Unmarshal(fields["enabled"], &enabled) != nil || enabled == nil {
			writeJSON(w, 400, map[string]string{"error": "account_status_request_invalid"})
			return true
		}
		for _, account := range m.accounts {
			if account.record.ID == id {
				if err := m.registry.SetEnabled(id, *enabled); err != nil {
					writeJSON(w, 503, map[string]string{"error": "account_status_persistence_failed"})
					return true
				}
				account.record.Enabled = *enabled
				writeJSON(w, 200, m.accountStatus(account))
				return true
			}
		}
		writeJSON(w, 404, map[string]string{"error": "upstream_account_not_found"})
	}
	return true
}
