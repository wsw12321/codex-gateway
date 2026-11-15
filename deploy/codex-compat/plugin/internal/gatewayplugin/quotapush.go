package gatewayplugin

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Quota push. CPA hands this plugin the upstream response headers of every
// attempt in its usage record, the same headers it records as its passive
// quota observation. The plugin reads the rate-limit headers from that record
// in memory and tells the Gateway only when an account's quota changed, so the
// Gateway's quota guard sees new usage within one response instead of at its
// next scheduled check. Nothing here sends a request to a provider, and no
// header value is ever logged.
//
// CPA delivers usage records from a single dispatcher with backpressure, so
// the record handler never blocks: it only stores the newest report per
// account, and one background goroutine delivers them.
const (
	quotaPushAction = "quota-observed"
	// quotaPushRetry is how long an account waits after a failed delivery,
	// including the 404 of a Gateway that predates this route.
	quotaPushRetry = time.Minute
	// quotaPushResetTolerance is how far two reset times may differ and still
	// name the same window period. A reset derived from Reset-After-Seconds
	// moves by a second or so between responses.
	quotaPushResetTolerance = 60
	maxQuotaPushAccounts    = 4096
	maxQuotaSignalHeaders   = 64
	maxQuotaSignalValue     = 512
)

// QuotaReport is one account's quota as observed on one upstream response.
// Codex carries the envelope of the Owner's quota route; Claude carries the
// windows of the credential page's Claude quota.
type QuotaReport struct {
	AccountID  string
	Codex      *quotaRPCResponse
	ObservedAt string
	Windows    []anthropicWindow
}

type quotaKey struct {
	provider string
	account  string
}

// quotaDigestEntry is one compared value of a report: a window, or the
// reached marker of a Codex limit.
type quotaDigestEntry struct {
	name    string
	used    int
	minutes int64
	status  string
	reset   int64
}

// quotaDigest is what decides whether a report is news to the Gateway. Its
// entries are sorted by name.
type quotaDigest []quotaDigestEntry

func (d quotaDigest) equal(other quotaDigest) bool {
	if len(d) != len(other) {
		return false
	}
	for i := range d {
		a, b := d[i], other[i]
		if a.name != b.name || a.used != b.used || a.minutes != b.minutes || a.status != b.status ||
			(a.reset == 0) != (b.reset == 0) {
			return false
		}
		if diff := a.reset - b.reset; diff > quotaPushResetTolerance || diff < -quotaPushResetTolerance {
			return false
		}
	}
	return true
}

type pendingQuota struct {
	digest quotaDigest
	report QuotaReport
	// offered is when the record arrived; a failed delivery backs off from it.
	offered time.Time
}

// quotaPusher keeps, per account, what the Gateway last acknowledged, the
// report being delivered, the newest report not yet delivered, and the end of
// a delivery backoff.
type quotaPusher struct {
	mu       sync.Mutex
	acked    map[quotaKey]quotaDigest
	inflight map[quotaKey]quotaDigest
	pending  map[quotaKey]pendingQuota
	backoff  map[quotaKey]time.Time
	running  bool
}

func newQuotaPusher() *quotaPusher {
	return &quotaPusher{
		acked: map[quotaKey]quotaDigest{}, inflight: map[quotaKey]quotaDigest{},
		pending: map[quotaKey]pendingQuota{}, backoff: map[quotaKey]time.Time{},
	}
}

// offer stores a report unless the Gateway already has it, an equal one is
// being delivered or waiting, or the account is backing off. It reports whether the caller must
// start the delivery goroutine.
func (q *quotaPusher) offer(key quotaKey, next pendingQuota) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if until, ok := q.backoff[key]; ok {
		if next.offered.Before(until) {
			return false
		}
		delete(q.backoff, key)
	}
	if waiting, ok := q.pending[key]; ok {
		if waiting.digest.equal(next.digest) {
			return false
		}
	} else {
		known, ok := q.inflight[key]
		if !ok {
			known, ok = q.acked[key]
		}
		if ok && known.equal(next.digest) {
			return false
		}
		if len(q.pending) >= maxQuotaPushAccounts {
			return false
		}
	}
	q.pending[key] = next
	if len(q.acked)+len(q.backoff) > maxQuotaPushAccounts {
		for stale := range q.acked {
			if _, waiting := q.pending[stale]; !waiting {
				delete(q.acked, stale)
			}
		}
		for stale := range q.backoff {
			delete(q.backoff, stale)
		}
	}
	if q.running {
		return false
	}
	q.running = true
	return true
}

// next takes one waiting report, or ends the delivery goroutine's run.
func (q *quotaPusher) next() (quotaKey, pendingQuota, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for key, waiting := range q.pending {
		delete(q.pending, key)
		q.inflight[key] = waiting.digest
		return key, waiting, true
	}
	q.running = false
	return quotaKey{}, pendingQuota{}, false
}

// done records a delivery. A failure forgets what the Gateway had, so the
// next change after the backoff is delivered again.
func (q *quotaPusher) done(key quotaKey, delivered pendingQuota, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, key)
	if err == nil {
		q.acked[key] = delivered.digest
		return
	}
	delete(q.acked, key)
	q.backoff[key] = delivered.offered.Add(quotaPushRetry)
}

func (q *quotaPusher) stopped() {
	q.mu.Lock()
	q.running = false
	clear(q.inflight)
	q.mu.Unlock()
}

// idle reports whether no report is waiting or being delivered.
func (q *quotaPusher) idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.running && len(q.pending) == 0
}

// observeQuota reads the rate-limit headers of one usage record. The account
// is the one this plugin allocated for the record's selection; nothing the
// record names is trusted for that.
func (p *Plugin) observeQuota(record pluginapi.UsageRecord) {
	if p.unavailable != nil || p.quotaPush == nil || record.SelectionID == "" || len(record.ResponseHeaders) == 0 {
		return
	}
	selected, _ := p.requests.selection(record.SelectionID)
	if selected == nil {
		return
	}
	key := quotaKey{provider: selected.provider, account: selected.account}
	signals := quotaSignalsFromHeaders(key.provider, record.ResponseHeaders)
	if len(signals) == 0 {
		return
	}
	now := p.now()
	report, digest, ok := buildQuotaReport(key.provider, key.account, signals, now)
	if !ok {
		return
	}
	if p.quotaPush.offer(key, pendingQuota{digest: digest, report: report, offered: now}) {
		go p.deliverQuota()
	}
}

// deliverQuota sends waiting reports one at a time until none is left.
func (p *Plugin) deliverQuota() {
	defer func() {
		if recovered := recover(); recovered != nil {
			logf("quota push stopped after a panic: %T", recovered)
			p.quotaPush.stopped()
		}
	}()
	timeout := p.cfg.CallbackTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	for {
		key, waiting, ok := p.quotaPush.next()
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := p.alloc.ReportQuota(ctx, key.provider, waiting.report)
		cancel()
		if err != nil {
			logf("quota push for account %s was not delivered: %T", key.account, err)
		}
		p.quotaPush.done(key, waiting, err)
	}
}

// quotaSignalsFromHeaders keeps the provider's rate-limit headers the way CPA
// records them as its passive observation: canonical names, the last value of
// each, trimmed, bounded and free of control characters.
func quotaSignalsFromHeaders(provider string, headers http.Header) map[string]string {
	var prefix string
	switch provider {
	case ProviderCodex:
		prefix = codexSignalsPrefix
	case ProviderClaude:
		prefix = anthropicSignalsPrefix
	default:
		return nil
	}
	signals := map[string]string{}
	for name, values := range headers {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !strings.HasPrefix(canonical, prefix) || len(values) == 0 {
			continue
		}
		value := strings.TrimSpace(values[len(values)-1])
		if !validSignalValue(value) {
			continue
		}
		signals[canonical] = value
	}
	if len(signals) <= maxQuotaSignalHeaders {
		return signals
	}
	// Keep the default Codex limit ahead of additional limits, as CPA does,
	// so truncation never drops the windows the guard reads.
	names := make([]string, 0, len(signals))
	for name := range signals {
		names = append(names, name)
	}
	rank := func(name string) int {
		rest := strings.TrimPrefix(name, codexSignalsPrefix)
		if strings.HasPrefix(rest, "Primary-") || strings.HasPrefix(rest, "Secondary-") || rest == "Limit-Reached" || rest == "Allowed" {
			return 0
		}
		return 1
	}
	sort.Slice(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	kept := make(map[string]string, maxQuotaSignalHeaders)
	for _, name := range names[:maxQuotaSignalHeaders] {
		kept[name] = signals[name]
	}
	return kept
}

func validSignalValue(value string) bool {
	if value == "" || len(value) > maxQuotaSignalValue {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

// buildQuotaReport normalizes the signals exactly as the quota routes do. A
// response whose signals do not form a valid snapshot is not reported: the
// scheduled check still reads CPA's record and reports its fixed error code.
func buildQuotaReport(provider, account string, signals map[string]string, at time.Time) (QuotaReport, quotaDigest, bool) {
	// Microseconds keep two reports in one second apart and survive the
	// Gateway's timestamp column unchanged.
	observed := at.UTC().Truncate(time.Microsecond)
	switch provider {
	case ProviderCodex:
		set, err := normalizeCodexSignals(signals, observed)
		if err != nil {
			return QuotaReport{}, nil, false
		}
		envelope := &quotaRPCResponse{ID: quotaRPCID, Result: set, ObservedAt: observed.Format(time.RFC3339Nano)}
		return QuotaReport{AccountID: account, Codex: envelope}, codexQuotaDigest(set), true
	case ProviderClaude:
		snapshot := anthropicSnapshot(credentialObservation{Quota: signalSnapshot{ObservedAt: observed, Signals: signals}}, observed)
		if len(snapshot.Windows) == 0 {
			return QuotaReport{}, nil, false
		}
		report := QuotaReport{AccountID: account, ObservedAt: observed.Format(time.RFC3339Nano), Windows: snapshot.Windows}
		return report, claudeQuotaDigest(snapshot.Windows), true
	}
	return QuotaReport{}, nil, false
}

func codexQuotaDigest(set quotaRateLimitSet) quotaDigest {
	ids := make([]string, 0, len(set.RateLimitsByLimitID))
	for id := range set.RateLimitsByLimitID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var digest quotaDigest
	for _, id := range ids {
		limit := set.RateLimitsByLimitID[id]
		for _, window := range []struct {
			name  string
			value *quotaWindow
		}{{"primary", limit.Primary}, {"secondary", limit.Secondary}} {
			if window.value == nil {
				continue
			}
			entry := quotaDigestEntry{name: id + "/" + window.name, used: window.value.UsedPercent}
			if window.value.WindowDurationMins != nil {
				entry.minutes = *window.value.WindowDurationMins
			}
			if window.value.ResetsAt != nil {
				entry.reset = *window.value.ResetsAt
			}
			digest = append(digest, entry)
		}
		if limit.RateLimitReachedType != nil {
			digest = append(digest, quotaDigestEntry{name: id + "/reached", status: *limit.RateLimitReachedType})
		}
	}
	return digest
}

func claudeQuotaDigest(windows []anthropicWindow) quotaDigest {
	digest := make(quotaDigest, 0, len(windows))
	for _, window := range windows {
		entry := quotaDigestEntry{name: window.Window, used: -1, status: window.Status}
		if window.UsedPercent != nil {
			entry.used = int(math.Round(*window.UsedPercent))
		}
		if reset, err := time.Parse(time.RFC3339, window.ResetAt); err == nil {
			entry.reset = reset.Unix()
		}
		digest = append(digest, entry)
	}
	sort.Slice(digest, func(i, j int) bool { return digest[i].name < digest[j].name })
	return digest
}
