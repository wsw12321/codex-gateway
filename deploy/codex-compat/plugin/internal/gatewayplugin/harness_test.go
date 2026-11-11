package gatewayplugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testInternalKey   = "internal-key-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testManagementKey = "manage-key-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	testUser          = "11111111-1111-4111-8111-111111111111"
	testOtherUser     = "22222222-2222-4222-8222-222222222222"
	testGroupA        = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	testGroupB        = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func b64(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// fakeHost serves credential listings and documents from memory.
type fakeHost struct {
	mu      sync.Mutex
	entries []AuthEntry
	docs    map[string]string
	resets  []string
	gets    int
	listErr error
}

func (h *fakeHost) AuthList() ([]AuthEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listErr != nil {
		return nil, h.listErr
	}
	return append([]AuthEntry(nil), h.entries...), nil
}

func (h *fakeHost) AuthJSON(authIndex string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gets++
	doc, ok := h.docs[authIndex]
	if !ok {
		return nil, fmt.Errorf("unknown auth index")
	}
	return json.RawMessage(doc), nil
}

func (h *fakeHost) ResetCooldown(authIndex string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resets = append(h.resets, authIndex)
	return nil
}

// addCodex registers a Codex OAuth credential and returns its stable index.
func (h *fakeHost) addCodex(name, accountID string, lastRefresh time.Time) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.docs == nil {
		h.docs = map[string]string{}
	}
	index := "idx-" + name
	h.entries = append(h.entries, AuthEntry{
		ID: name, AuthIndex: index, Name: name, Provider: ProviderCodex, Status: "active", Source: "file",
		Size: int64(len(accountID)), ModTime: lastRefresh, UpdatedAt: lastRefresh, LastRefresh: lastRefresh, AccountType: "oauth",
	})
	h.docs[index] = fmt.Sprintf(`{"type":"codex","email":"%s@example.com","account_id":%q,"refresh_token":"secret"}`, name, accountID)
	return CodexAccountIndex(accountID)
}

// fakeAllocator plays the Gateway's allocation callbacks.
type fakeAllocator struct {
	mu sync.Mutex
	// permitted lists the accounts each user may use; nil means every account.
	permitted map[string]map[string]bool
	limits    map[string]int64
	// prefer orders cold selection; the first offered account in it wins.
	prefer []string
	// exhausted makes Select answer 429 for a pool made only of these.
	exhausted   map[string]bool
	eligibleErr error
	selectErr   error
	reportErr   error

	eligibleCalls [][]string
	selectCalls   [][]string
	reports       []Attempt
	beforeSelect  func()
}

func (a *fakeAllocator) Eligible(_ context.Context, _, userID string, accounts []string) ([]Eligibility, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.eligibleCalls = append(a.eligibleCalls, append([]string(nil), accounts...))
	if a.eligibleErr != nil {
		return nil, a.eligibleErr
	}
	var result []Eligibility
	for _, id := range accounts {
		if allowed := a.permitted[userID]; a.permitted != nil && !allowed[id] {
			continue
		}
		limit := a.limits[id]
		if limit == 0 {
			limit = 1
		}
		result = append(result, Eligibility{ID: id, ConcurrentLimit: limit})
	}
	return result, nil
}

func (a *fakeAllocator) Select(_ context.Context, _, _ string, accounts []string) (string, error) {
	a.mu.Lock()
	hook := a.beforeSelect
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.selectCalls = append(a.selectCalls, append([]string(nil), accounts...))
	if a.selectErr != nil {
		return "", a.selectErr
	}
	offered := map[string]bool{}
	free := false
	for _, id := range accounts {
		offered[id] = true
		free = free || !a.exhausted[id]
	}
	if !free {
		return "", ErrConcurrency
	}
	for _, id := range a.prefer {
		if offered[id] && !a.exhausted[id] {
			return id, nil
		}
	}
	sorted := append([]string(nil), accounts...)
	sort.Strings(sorted)
	for _, id := range sorted {
		if !a.exhausted[id] {
			return id, nil
		}
	}
	return "", ErrConcurrency
}

func (a *fakeAllocator) Report(_ context.Context, _ string, attempt Attempt) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reportErr != nil {
		return a.reportErr
	}
	a.reports = append(a.reports, attempt)
	return nil
}

func (a *fakeAllocator) lastReport(t *testing.T) Attempt {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.reports) == 0 {
		t.Fatal("no attempt was reported")
	}
	return a.reports[len(a.reports)-1]
}

type fixture struct {
	t      *testing.T
	plugin *Plugin
	host   *fakeHost
	alloc  *fakeAllocator
	dir    string
	clock  time.Time
	seq    int
	// ids maps a credential name to its stable account index.
	ids map[string]string
}

// newFixture builds a plugin over n Codex accounts named a, b, c, ...
func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	f := &fixture{t: t, host: &fakeHost{}, alloc: &fakeAllocator{limits: map[string]int64{}, exhausted: map[string]bool{}}, dir: t.TempDir(), clock: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), ids: map[string]string{}}
	for i := 0; i < n; i++ {
		name := string(rune('a' + i))
		f.ids[name] = f.host.addCodex(name, "acct-"+name, f.clock)
	}
	f.build()
	return f
}

func (f *fixture) build() {
	cfg := Config{InternalKey: testInternalKey, ManagementKey: testManagementKey, AuthDir: f.dir, AntigravityEnabled: true, BindingTTL: time.Hour, HoldTTL: 5 * time.Minute, CallbackTimeout: time.Second, GatewayURL: "http://gateway:8080", LoopbackURL: "http://127.0.0.1:8317"}
	f.plugin = NewWithConfig(cfg, f.host, f.alloc)
	if f.plugin.unavailable != nil {
		return
	}
	now := func() time.Time { return f.clock }
	f.plugin.now, f.plugin.concurrency.now, f.plugin.bindings.now, f.plugin.roots.now = now, now, now, now
	f.plugin.requests.now, f.plugin.inventory.now, f.plugin.control.now = now, now, now
}

func (f *fixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

// pickOptions describes one scheduler call.
type pickOptions struct {
	user      string
	affinity  byte
	session   string
	parent    string
	groups    string
	token     string
	only      []string
	model     string
	noToken   bool
	noUser    bool
	extraHdr  http.Header
	selection string
}

func (f *fixture) request(o pickOptions) pluginapi.SchedulerPickRequest {
	f.seq++
	if o.user == "" {
		o.user = testUser
	}
	if o.affinity == 0 {
		o.affinity = 1
	}
	if o.token == "" {
		o.token = b64(byte(100 + f.seq%100))
		raw := make([]byte, 32)
		raw[0], raw[1] = byte(f.seq>>8), byte(f.seq)
		o.token = base64.RawURLEncoding.EncodeToString(raw)
	}
	if o.model == "" {
		o.model = "gpt-6-astra"
	}
	if o.selection == "" {
		o.selection = fmt.Sprintf("sel-%d", f.seq)
	}
	headers := http.Header{}
	headers.Set(HeaderAffinity, b64(o.affinity))
	if !o.noUser {
		headers.Set(HeaderUser, o.user)
	}
	if !o.noToken {
		headers.Set(HeaderRequest, o.token)
	}
	if o.groups != "" {
		headers.Set(HeaderGroups, o.groups)
	}
	for name, values := range o.extraHdr {
		headers[name] = values
	}
	metadata := map[string]any{metadataCallerScope: fmt.Sprintf("scope-%d", o.affinity), metadataSelectionID: o.selection, metadataGenerate: true}
	if o.session != "" {
		metadata[metadataSession] = o.session
	}
	if o.parent != "" {
		metadata[metadataParentSession] = o.parent
	}
	names := o.only
	if names == nil {
		for name := range f.ids {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	request := pluginapi.SchedulerPickRequest{Model: o.model, Providers: []string{ProviderCodex}, Options: pluginapi.SchedulerOptions{Headers: headers, Metadata: metadata}}
	for _, name := range names {
		request.Candidates = append(request.Candidates, pluginapi.SchedulerAuthCandidate{ID: name, Provider: ProviderCodex, Status: "active"})
	}
	return request
}

// pick runs one allocation and returns the chosen credential name.
func (f *fixture) pick(o pickOptions) (string, error) {
	return f.plugin.Pick(context.Background(), f.request(o))
}

func (f *fixture) mustPick(o pickOptions) string {
	f.t.Helper()
	name, err := f.pick(o)
	if err != nil {
		f.t.Fatalf("pick failed: %v", err)
	}
	return name
}

// run performs a whole request: interception, pick and completion.
func (f *fixture) run(o pickOptions, outcome pluginapi.RequestCompletionOutcome) (string, error) {
	request := f.request(o)
	token := http.Header(request.Options.Headers).Get(HeaderRequest)
	requestID := "req-" + token
	f.plugin.requests.begin(token, requestID, true)
	name, err := f.plugin.Pick(context.Background(), request)
	f.plugin.Complete(requestID, outcome)
	return name, err
}

func (f *fixture) active(name string) int64 {
	for _, row := range f.plugin.concurrency.Snapshot(nil) {
		if row.ID == f.ids[name] {
			return row.ActiveRequests
		}
	}
	return 0
}

func groupsHeader(layers ...string) string {
	out := "["
	for i, layer := range layers {
		if i > 0 {
			out += ","
		}
		out += layer
	}
	return out + "]"
}

func groupLayer(group string, ids ...string) string {
	raw, _ := json.Marshal(ids)
	if ids == nil {
		raw = []byte("[]")
	}
	return fmt.Sprintf(`{"group_id":%q,"account_ids":%s}`, group, raw)
}

const personalLayer = `{"group_id":"00000000-0000-0000-0000-000000000000","all_accounts":true}`
