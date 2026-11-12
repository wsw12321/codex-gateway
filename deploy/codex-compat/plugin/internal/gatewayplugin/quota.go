package gatewayplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Quota answers come from CPA's passive observation only. CPA records the
// rate-limit headers of the responses it receives for its own requests
// (x-codex-* for Codex, anthropic-ratelimit-unified-* for Claude) on each
// credential; this plugin reads that record through CPA's management API and
// projects it. It never sends a request to a provider: the request handling
// belongs to CPA alone, and CPA has no live quota request of its own.
//
// The Codex answer keeps the source-patched image's /internal/*/quota
// envelope, which the Gateway decodes strictly, and adds the observation
// time. Antigravity has no passive source, so its quota is unavailable.
const (
	quotaRPCMethod         = "account/rateLimits/read"
	quotaRPCID             = 6
	maxQuotaRequestBytes   = 256
	maxFacadeRequestBytes  = 128
	maxQuotaExtraLimits    = 32
	maxQuotaLimitIDBytes   = 32
	maxQuotaWindowSeconds  = 31 * 24 * 60 * 60
	minQuotaResetUnix      = 946684800  // 2000-01-01T00:00:00Z
	maxQuotaResetUnix      = 4102444800 // 2100-01-01T00:00:00Z
	maxAuthFileListBytes   = 1 << 20
	codexPrimaryLimitID    = "codex"
	codexSignalsPrefix     = "X-Codex-"
	anthropicFiveHour      = "five_hour"
	anthropicSevenDay      = "seven_day"
	anthropicSignalsPrefix = "Anthropic-Ratelimit-Unified-"
)

var (
	errQuotaRequestTooLarge = errors.New("quota request too large")
	errQuotaRequestInvalid  = errors.New("quota request invalid")
	errQuotaNotObserved     = errors.New("quota not observed")
)

// quotaSchemaError names a failed schema check without retaining any value
// from the observation.
type quotaSchemaError string

func (e quotaSchemaError) Error() string { return string(e) }

func quotaSchemaCode(err error) string {
	var schema quotaSchemaError
	if errors.As(err, &schema) {
		switch schema {
		case "quota_field_type_invalid", "quota_percent_out_of_range",
			"quota_window_invalid", "quota_reset_invalid":
			return string(schema)
		}
	}
	return "quota_schema_changed"
}

// ---- wire types shared with the Gateway ------------------------------------

type quotaRPCResponse struct {
	ID     int               `json:"id"`
	Result quotaRateLimitSet `json:"result"`
	// ObservedAt is when CPA received the response the snapshot comes from.
	ObservedAt string `json:"observed_at"`
}

type quotaRateLimitSet struct {
	RateLimits            quotaRateLimit            `json:"rateLimits"`
	RateLimitsByLimitID   map[string]quotaRateLimit `json:"rateLimitsByLimitId"`
	RateLimitResetCredits any                       `json:"rateLimitResetCredits"`
}

type quotaRateLimit struct {
	LimitID              string       `json:"limitId"`
	LimitName            *string      `json:"limitName"`
	Primary              *quotaWindow `json:"primary"`
	Secondary            *quotaWindow `json:"secondary"`
	RateLimitReachedType *string      `json:"rateLimitReachedType"`
}

type quotaWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
	ResetsAt           *int64 `json:"resetsAt"`
}

// facadeQuota is one model's remaining share on the credential page.
type facadeQuota struct {
	Model             string  `json:"model"`
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time,omitempty"`
}

// anthropicWindow is one passive Claude usage window.
type anthropicWindow struct {
	Window      string   `json:"window"`
	UsedPercent *float64 `json:"used_percent,omitempty"`
	ResetAt     string   `json:"reset_at,omitempty"`
	Status      string   `json:"status"`
}

// ---- request decoding -------------------------------------------------------

// decodeQuotaRPCRequest accepts exactly {"method":"account/rateLimits/read","id":6}
// in any key order: no other key, no duplicate, no fractional id, nothing after.
func decodeQuotaRPCRequest(body []byte) (int, error) {
	if len(body) > maxQuotaRequestBytes {
		return 0, errQuotaRequestTooLarge
	}
	fields, err := decodeObject(bytes.TrimSpace(body), "method", "id")
	if err != nil {
		return 0, errQuotaRequestInvalid
	}
	var method string
	var id int
	if json.Unmarshal(fields["method"], &method) != nil || json.Unmarshal(fields["id"], &id) != nil ||
		method != quotaRPCMethod || id != quotaRPCID || string(fields["id"]) != strconv.Itoa(quotaRPCID) {
		return 0, errQuotaRequestInvalid
	}
	return id, nil
}

// decodeFacadeRequest accepts the credential page's empty object.
func decodeFacadeRequest(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(body) > maxFacadeRequestBytes {
		return false
	}
	if len(trimmed) == 0 {
		return true
	}
	fields, err := decodeObject(trimmed)
	return err == nil && len(fields) == 0
}

func quotaAccountID(query url.Values) (string, bool) {
	ids := query["id"]
	if len(ids) != 1 || len(query) != 1 || !validAccountID(ids[0]) {
		return "", false
	}
	return ids[0], true
}

// ---- Codex ------------------------------------------------------------------

// codexQuota serves the Owner's Codex quota query from CPA's latest
// observation of the account.
func (p *Plugin) codexQuota(query url.Values, body []byte) []byte {
	result, status, code := p.observeCodexQuota(query, body)
	if code != "" {
		return managementError(status, code)
	}
	return managementJSON(http.StatusOK, result)
}

// codexQuotaFacade serves the credential page: the same observation, reduced
// to the remaining share of each limit's primary window.
func (p *Plugin) codexQuotaFacade(query url.Values, body []byte) []byte {
	if !decodeFacadeRequest(body) {
		return facadeError(http.StatusBadRequest)
	}
	rpc, _ := json.Marshal(map[string]any{"method": quotaRPCMethod, "id": quotaRPCID})
	result, status, code := p.observeCodexQuota(query, rpc)
	if code != "" {
		if status == http.StatusNotFound {
			return facadeError(http.StatusNotFound)
		}
		if code == "quota_not_observed" {
			return managementJSON(http.StatusOK, map[string]any{"status": "ok", "quota": []facadeQuota{}})
		}
		return facadeError(http.StatusBadGateway)
	}
	ids := make([]string, 0, len(result.Result.RateLimitsByLimitID))
	for id := range result.Result.RateLimitsByLimitID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	quota := make([]facadeQuota, 0, len(ids))
	for _, id := range ids {
		if primary := result.Result.RateLimitsByLimitID[id].Primary; primary != nil {
			quota = append(quota, facadeQuota{Model: id, RemainingFraction: float64(100-primary.UsedPercent) / 100})
		}
	}
	return managementJSON(http.StatusOK, map[string]any{"status": "ok", "quota": quota, "observed_at": result.ObservedAt})
}

func facadeError(status int) []byte {
	return managementJSON(status, map[string]string{"status": "error"})
}

// observeCodexQuota returns the normalized snapshot, or an HTTP status with
// the fixed error code the Gateway maps.
func (p *Plugin) observeCodexQuota(query url.Values, body []byte) (quotaRPCResponse, int, string) {
	id, err := decodeQuotaRPCRequest(body)
	if errors.Is(err, errQuotaRequestTooLarge) {
		return quotaRPCResponse{}, http.StatusRequestEntityTooLarge, "quota_request_too_large"
	}
	if err != nil {
		return quotaRPCResponse{}, http.StatusBadRequest, "quota_request_invalid"
	}
	accountID, ok := quotaAccountID(query)
	if !ok {
		return quotaRPCResponse{}, http.StatusNotFound, "upstream_account_not_found"
	}
	account, found, err := p.inventory.Find(ProviderCodex, accountID)
	if err != nil {
		return quotaRPCResponse{}, http.StatusServiceUnavailable, "account_registry_unavailable"
	}
	if !found {
		return quotaRPCResponse{}, http.StatusNotFound, "upstream_account_not_found"
	}
	if hostStatus(account) == "disabled" {
		return quotaRPCResponse{}, http.StatusConflict, "upstream_account_disabled"
	}
	// The credential must still be the ChatGPT account the stable id was
	// derived from, so a file replaced since the last inventory refresh
	// cannot answer for another account. This reads the stored document only.
	document, err := p.credentialDocument(account)
	if err != nil {
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_credential_unavailable"
	}
	chatgptAccount, ok := codexIdentity(document)
	if !ok || chatgptAccount == "" || CodexAccountIndex(chatgptAccount) != account.ID {
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_account_identity_unavailable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observation, err := p.observeCredential(ctx, account)
	if err != nil {
		return quotaRPCResponse{}, http.StatusServiceUnavailable, "quota_observation_unavailable"
	}
	snapshot, found := latestSnapshot(observation, codexSignalsPrefix)
	if !found {
		return quotaRPCResponse{}, http.StatusConflict, "quota_not_observed"
	}
	result, err := normalizeCodexSignals(snapshot.Signals, snapshot.ObservedAt)
	if errors.Is(err, errQuotaNotObserved) {
		return quotaRPCResponse{}, http.StatusConflict, "quota_not_observed"
	}
	if err != nil {
		return quotaRPCResponse{}, http.StatusBadGateway, quotaSchemaCode(err)
	}
	return quotaRPCResponse{ID: id, Result: result, ObservedAt: snapshot.ObservedAt.UTC().Format(time.RFC3339)}, 0, ""
}

// codexWindowNames are the two windows of every Codex limit.
var codexWindowNames = [...]string{"Primary", "Secondary"}

// normalizeCodexSignals converts the x-codex-* headers CPA recorded into the
// app-server rateLimits shape. The default limit's headers carry no limit
// name (x-codex-primary-used-percent); every other limit is namespaced by a
// short name on HTTP responses (x-codex-<name>-primary-used-percent) and by
// x-codex-additional-<name>- on websocket events. A limit whose name does not
// form a valid id is left out rather than failing the whole snapshot.
func normalizeCodexSignals(signals map[string]string, observedAt time.Time) (quotaRateLimitSet, error) {
	limits := map[string]map[string]string{}
	for name, value := range signals {
		canonical := http.CanonicalHeaderKey(name)
		rest, ok := strings.CutPrefix(canonical, codexSignalsPrefix)
		if !ok {
			continue
		}
		limit, field, ok := splitCodexSignal(rest)
		if !ok {
			continue
		}
		if limits[limit] == nil {
			limits[limit] = map[string]string{}
		}
		limits[limit][field] = value
	}
	byLimitID := map[string]quotaRateLimit{}
	names := make([]string, 0, len(limits))
	for name := range limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		limitID := codexPrimaryLimitID
		if name != "" {
			limitID = codexLimitID(name)
			if !validQuotaLimitID(limitID) || limitID == codexPrimaryLimitID || len(byLimitID) > maxQuotaExtraLimits {
				continue
			}
			if _, exists := byLimitID[limitID]; exists {
				continue
			}
		}
		normalized, present, err := normalizeCodexSignalLimit(limitID, limits[name], observedAt)
		if err != nil {
			return quotaRateLimitSet{}, err
		}
		if present {
			byLimitID[limitID] = normalized
		}
	}
	if len(byLimitID) == 0 {
		return quotaRateLimitSet{}, errQuotaNotObserved
	}
	primary, ok := byLimitID[codexPrimaryLimitID]
	if !ok {
		primary = quotaRateLimit{LimitID: codexPrimaryLimitID}
		byLimitID[codexPrimaryLimitID] = primary
	}
	return quotaRateLimitSet{RateLimits: primary, RateLimitsByLimitID: byLimitID}, nil
}

// splitCodexSignal splits "Bengalfox-Primary-Used-Percent" into the limit
// name ("Bengalfox") and the field ("Primary-Used-Percent"). The default
// limit has an empty name. Headers that describe no window are skipped.
func splitCodexSignal(rest string) (limit, field string, ok bool) {
	for _, window := range codexWindowNames {
		if strings.HasPrefix(rest, window+"-") {
			return "", rest, true
		}
		if index := strings.Index(rest, "-"+window+"-"); index > 0 {
			return rest[:index], rest[index+1:], true
		}
	}
	switch {
	case rest == "Limit-Reached" || rest == "Allowed":
		return "", rest, true
	case strings.HasSuffix(rest, "-Limit-Reached"):
		return strings.TrimSuffix(rest, "-Limit-Reached"), "Limit-Reached", true
	case strings.HasSuffix(rest, "-Allowed"):
		return strings.TrimSuffix(rest, "-Allowed"), "Allowed", true
	}
	return "", "", false
}

// codexLimitID turns a header namespace into a limit id: "Code-Review" and
// "Additional-Code-Review" both become "code_review".
func codexLimitID(name string) string {
	name = strings.TrimPrefix(name, "Additional-")
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

// normalizeCodexSignalLimit builds one limit. present is false when the limit
// carries no window and no reached flag.
func normalizeCodexSignalLimit(limitID string, fields map[string]string, observedAt time.Time) (quotaRateLimit, bool, error) {
	view := quotaRateLimit{LimitID: limitID}
	present := false
	for _, window := range codexWindowNames {
		normalized, err := normalizeCodexSignalWindow(fields, window+"-", observedAt)
		if err != nil {
			return quotaRateLimit{}, false, err
		}
		if normalized == nil {
			continue
		}
		present = true
		if window == "Primary" {
			view.Primary = normalized
		} else {
			view.Secondary = normalized
		}
	}
	reached := strings.EqualFold(strings.TrimSpace(fields["Limit-Reached"]), "true") ||
		strings.EqualFold(strings.TrimSpace(fields["Allowed"]), "false")
	if reached {
		present = true
		view.RateLimitReachedType = reachedType("rate_limit_reached")
	}
	return view, present, nil
}

// normalizeCodexSignalWindow reads one window. A window without a used
// percentage is absent. A fractional percentage is rounded up, so the
// snapshot never reports less use than CPA observed.
func normalizeCodexSignalWindow(fields map[string]string, prefix string, observedAt time.Time) (*quotaWindow, error) {
	rawUsed, ok := fields[prefix+"Used-Percent"]
	if !ok {
		return nil, nil
	}
	used, err := strconv.ParseFloat(strings.TrimSpace(rawUsed), 64)
	if err != nil {
		return nil, quotaSchemaError("quota_field_type_invalid")
	}
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return nil, quotaSchemaError("quota_percent_out_of_range")
	}
	view := &quotaWindow{UsedPercent: int(math.Ceil(used))}
	if raw, ok := fields[prefix+"Window-Minutes"]; ok {
		minutes, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, quotaSchemaError("quota_field_type_invalid")
		}
		if minutes <= 0 || minutes > maxQuotaWindowSeconds/60 {
			return nil, quotaSchemaError("quota_window_invalid")
		}
		view.WindowDurationMins = &minutes
	}
	var reset int64
	if raw, ok := fields[prefix+"Reset-At"]; ok {
		if reset, err = strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err != nil {
			return nil, quotaSchemaError("quota_field_type_invalid")
		}
	} else if raw, ok := fields[prefix+"Reset-After-Seconds"]; ok {
		after, errAfter := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if errAfter != nil {
			return nil, quotaSchemaError("quota_field_type_invalid")
		}
		if after < 0 || after > maxQuotaWindowSeconds {
			return nil, quotaSchemaError("quota_reset_invalid")
		}
		reset = observedAt.Unix() + after
	}
	if reset != 0 {
		if reset < minQuotaResetUnix || reset > maxQuotaResetUnix {
			return nil, quotaSchemaError("quota_reset_invalid")
		}
		view.ResetsAt = &reset
	}
	return view, nil
}

func validQuotaLimitID(value string) bool {
	if value == "" || len(value) > maxQuotaLimitIDBytes || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func reachedType(value string) *string {
	value = strings.TrimSpace(value)
	switch value {
	case "rate_limit_reached", "workspace_owner_credits_depleted", "workspace_member_credits_depleted",
		"workspace_owner_usage_limit_reached", "workspace_member_usage_limit_reached":
		return &value
	}
	return nil
}

// ---- Antigravity ------------------------------------------------------------

// antigravityQuotaFacade answers the credential page's Gemini quota. CPA has
// no passive quota record for Antigravity and this plugin sends no provider
// request of its own, so the quota is reported as unavailable.
func (p *Plugin) antigravityQuotaFacade(query url.Values, body []byte) []byte {
	if !decodeFacadeRequest(body) {
		return facadeError(http.StatusBadRequest)
	}
	accountID, ok := quotaAccountID(query)
	if !ok {
		return facadeError(http.StatusNotFound)
	}
	if _, found, err := p.inventory.Find(ProviderAntigravity, accountID); err != nil || !found {
		return facadeError(http.StatusNotFound)
	}
	return managementJSON(http.StatusOK, map[string]any{"status": "unavailable", "quota": []facadeQuota{}})
}

// ---- Claude -----------------------------------------------------------------

// anthropicQuotaFacade projects CPA's passive observation of the unified rate
// limit headers. It issues no upstream request, guesses no plan and changes
// no cooldown.
func (p *Plugin) anthropicQuotaFacade(query url.Values, body []byte) []byte {
	if !decodeFacadeRequest(body) {
		return facadeError(http.StatusBadRequest)
	}
	accountID, ok := quotaAccountID(query)
	if !ok {
		return facadeError(http.StatusNotFound)
	}
	account, found, err := p.inventory.Find(ProviderClaude, accountID)
	if err != nil || !found {
		return facadeError(http.StatusNotFound)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observation, err := p.observeCredential(ctx, account)
	if err != nil {
		return facadeError(http.StatusServiceUnavailable)
	}
	snapshot := anthropicSnapshot(observation, p.now())
	response := map[string]any{"status": "ok", "quota": []facadeQuota{}, "windows": snapshot.Windows}
	if snapshot.ObservedAt != "" {
		response["observed_at"] = snapshot.ObservedAt
	}
	if snapshot.CooldownUntil != "" {
		response["cooldown_until"] = snapshot.CooldownUntil
	}
	return managementJSON(http.StatusOK, response)
}

// credentialObservation is what CPA reports about one credential's passive
// quota observations, read from its management listing.
type credentialObservation struct {
	Quota          signalSnapshot            `json:"quota"`
	ModelQuotas    map[string]signalSnapshot `json:"model_quotas"`
	NextRetryAfter time.Time                 `json:"next_retry_after"`
}

type signalSnapshot struct {
	ObservedAt time.Time         `json:"observed_at"`
	Signals    map[string]string `json:"signals"`
}

type anthropicQuotaSnapshot struct {
	ObservedAt    string
	CooldownUntil string
	Windows       []anthropicWindow
}

// anthropicSnapshot uses the newest observation of the credential or any of
// its models, as the source-patched image does.
func anthropicSnapshot(observation credentialObservation, now time.Time) anthropicQuotaSnapshot {
	out := anthropicQuotaSnapshot{Windows: []anthropicWindow{}}
	if observation.NextRetryAfter.After(now) {
		out.CooldownUntil = observation.NextRetryAfter.UTC().Format(time.RFC3339)
	}
	latest, found := latestSnapshot(observation, "")
	if !found {
		return out
	}
	out.ObservedAt = latest.ObservedAt.UTC().Format(time.RFC3339)
	signal := func(name string) string { return latest.Signals[http.CanonicalHeaderKey(anthropicSignalsPrefix+name)] }
	for _, pair := range [][2]string{{"5h", anthropicFiveHour}, {"7d", anthropicSevenDay}} {
		window := anthropicWindow{Window: pair[1], Status: "unknown"}
		switch status := signal(pair[0] + "-Status"); status {
		case "allowed", "allowed_warning", "rejected":
			window.Status = status
		}
		if value, err := strconv.ParseFloat(signal(pair[0]+"-Utilization"), 64); err == nil &&
			!math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1 {
			value *= 100
			window.UsedPercent = &value
		}
		if value, err := strconv.ParseInt(signal(pair[0]+"-Reset"), 10, 64); err == nil &&
			value >= minQuotaResetUnix && value <= maxQuotaResetUnix {
			window.ResetAt = time.Unix(value, 0).UTC().Format(time.RFC3339)
		}
		if window.Status != "unknown" || window.UsedPercent != nil || window.ResetAt != "" {
			out.Windows = append(out.Windows, window)
		}
	}
	return out
}

// loopbackObserveCredential reads one credential's passive observations from
// CPA's own management listing, filtered by its auth index.
func (p *Plugin) loopbackObserveCredential(ctx context.Context, account Account) (credentialObservation, error) {
	if p.cfg.ManagementKey == "" || account.AuthIndex == "" {
		return credentialObservation{}, ErrUnavailable
	}
	target := p.cfg.LoopbackURL + "/v0/management/auth-files?" + url.Values{"auth_index": {account.AuthIndex}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return credentialObservation{}, ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+p.cfg.ManagementKey)
	response, err := p.loopback.Do(request)
	if err != nil {
		return credentialObservation{}, ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAuthFileListBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || len(data) > maxAuthFileListBytes {
		return credentialObservation{}, ErrUnavailable
	}
	var listing struct {
		Files []struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
			credentialObservation
		} `json:"files"`
	}
	if json.Unmarshal(data, &listing) != nil {
		return credentialObservation{}, ErrUnavailable
	}
	for _, file := range listing.Files {
		if file.AuthIndex == account.AuthIndex && file.ID == account.AuthID {
			return file.credentialObservation, nil
		}
	}
	return credentialObservation{}, ErrUnavailable
}

// ---- shared -----------------------------------------------------------------

// latestSnapshot returns the newest observation of the credential or any of
// its models whose signals include a name with prefix (any signal when prefix
// is empty). Model names are visited in order, so ties resolve the same way
// every time.
func latestSnapshot(observation credentialObservation, prefix string) (signalSnapshot, bool) {
	relevant := func(snapshot signalSnapshot) bool {
		if snapshot.ObservedAt.IsZero() {
			return false
		}
		if prefix == "" {
			return true
		}
		for name := range snapshot.Signals {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), prefix) {
				return true
			}
		}
		return false
	}
	var latest signalSnapshot
	found := false
	if relevant(observation.Quota) {
		latest, found = observation.Quota, true
	}
	models := make([]string, 0, len(observation.ModelQuotas))
	for model := range observation.ModelQuotas {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		if candidate := observation.ModelQuotas[model]; relevant(candidate) && (!found || candidate.ObservedAt.After(latest.ObservedAt)) {
			latest, found = candidate, true
		}
	}
	return latest, found
}

// credentialDocument reads a credential's stored document. Only the fields a
// check needs are taken from it; nothing is retained.
func (p *Plugin) credentialDocument(account Account) (map[string]json.RawMessage, error) {
	raw, err := p.host.AuthJSON(account.AuthIndex)
	if err != nil {
		return nil, err
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return nil, ErrUnavailable
	}
	return document, nil
}
