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

// Quota queries reproduce the source-patched image's /internal/*/quota
// contract byte for byte where the Gateway decodes it strictly: the request is
// a fixed RPC envelope, the answer is a normalized snapshot or one fixed error
// code, and nothing the provider wrote is ever passed through.
//
// Live queries (Codex, Antigravity) go out from this plugin, not through the
// host, because no host callback can both pin a proxy and refuse redirects.
// Each request uses the route the Gateway binds the account to, or CPA's
// global egress when the account has no binding; it never connects directly.
const (
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	antigravityQuotaURL  = "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
	antigravityUserAgent = "antigravity/hub/2.29.0 (aidev_client; os_type=darwin; arch=arm64; cl=1013060670)"

	quotaRPCMethod         = "account/rateLimits/read"
	quotaRPCID             = 6
	quotaTimeout           = 15 * time.Second
	maxQuotaRequestBytes   = 256
	maxQuotaResponseBytes  = 1 << 20
	maxFacadeRequestBytes  = 128
	maxQuotaExtraLimits    = 32
	maxQuotaLimitIDBytes   = 32
	maxQuotaWindowSeconds  = 31 * 24 * 60 * 60
	minQuotaResetUnix      = 946684800  // 2000-01-01T00:00:00Z
	maxQuotaResetUnix      = 4102444800 // 2100-01-01T00:00:00Z
	maxBearerTokenBytes    = 16 << 10
	maxAuthFileListBytes   = 1 << 20
	codexPrimaryLimitID    = "codex"
	anthropicFiveHour      = "five_hour"
	anthropicSevenDay      = "seven_day"
	anthropicSignalsPrefix = "Anthropic-Ratelimit-Unified-"
)

var (
	errQuotaRequestTooLarge = errors.New("quota request too large")
	errQuotaRequestInvalid  = errors.New("quota request invalid")
	errQuotaEgress          = errors.New("quota egress unavailable")
)

// quotaSchemaError names a failed schema check without retaining any value
// from the provider's response.
type quotaSchemaError string

func (e quotaSchemaError) Error() string { return string(e) }

func quotaSchemaCode(err error) string {
	var schema quotaSchemaError
	if errors.As(err, &schema) {
		switch schema {
		case "quota_json_invalid", "quota_field_type_invalid", "quota_rate_limit_missing",
			"quota_percent_missing", "quota_percent_out_of_range", "quota_percent_fractional",
			"quota_window_invalid", "quota_reset_invalid", "quota_limit_id_invalid",
			"quota_limit_id_duplicate", "quota_limits_excessive":
			return string(schema)
		}
	}
	return "quota_schema_changed"
}

// ---- wire types shared with the Gateway ------------------------------------

type quotaRPCResponse struct {
	ID     int               `json:"id"`
	Result quotaRateLimitSet `json:"result"`
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

// codexQuota serves the Owner's live Codex quota query.
func (p *Plugin) codexQuota(query url.Values, body []byte) []byte {
	result, status, code := p.queryCodexQuota(query, body)
	if code != "" {
		return managementError(status, code)
	}
	return managementJSON(http.StatusOK, result)
}

// codexQuotaFacade serves the credential page: the same live query, reduced to
// the remaining share of each limit's primary window.
func (p *Plugin) codexQuotaFacade(query url.Values, body []byte) []byte {
	if !decodeFacadeRequest(body) {
		return facadeError(http.StatusBadRequest)
	}
	rpc, _ := json.Marshal(map[string]any{"method": quotaRPCMethod, "id": quotaRPCID})
	result, status, code := p.queryCodexQuota(query, rpc)
	if code != "" {
		if status == http.StatusNotFound {
			return facadeError(http.StatusNotFound)
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
	return managementJSON(http.StatusOK, map[string]any{"status": "ok", "quota": quota})
}

func facadeError(status int) []byte {
	return managementJSON(status, map[string]string{"status": "error"})
}

// queryCodexQuota returns the normalized snapshot, or an HTTP status with the
// fixed error code the Gateway maps.
func (p *Plugin) queryCodexQuota(query url.Values, body []byte) (quotaRPCResponse, int, string) {
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
	document, err := p.credentialDocument(account)
	if err != nil {
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_credential_unavailable"
	}
	// The header must name the same ChatGPT account the stable id was derived
	// from, so a credential replaced since the last inventory refresh cannot
	// answer for another account.
	chatgptAccount, ok := codexIdentity(document)
	if !ok || chatgptAccount == "" || CodexAccountIndex(chatgptAccount) != account.ID {
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_account_identity_unavailable"
	}
	token := stringField(document, "access_token")
	if !safeBearerToken(token) {
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_credential_unavailable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_request_failed"
	}
	request.Header = http.Header{
		"Accept":             {"application/json"},
		"Authorization":      {"Bearer " + token},
		"Chatgpt-Account-Id": {chatgptAccount},
	}
	status, data, err := p.quotaExchange(ctx, ProviderCodex, account, request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return quotaRPCResponse{}, http.StatusGatewayTimeout, "quota_upstream_timeout"
		}
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_upstream_unavailable"
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return quotaRPCResponse{}, status, "quota_reauthentication_required"
	case status == http.StatusTooManyRequests:
		return quotaRPCResponse{}, status, "quota_upstream_rate_limited"
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return quotaRPCResponse{}, http.StatusGatewayTimeout, "quota_upstream_timeout"
	case status < http.StatusOK || status >= http.StatusMultipleChoices:
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_upstream_error"
	case data == nil:
		return quotaRPCResponse{}, http.StatusBadGateway, "quota_response_invalid"
	}
	result, err := normalizeCodexUsage(data)
	if err != nil {
		return quotaRPCResponse{}, http.StatusBadGateway, quotaSchemaCode(err)
	}
	return quotaRPCResponse{ID: id, Result: result}, 0, ""
}

type codexUsagePayload struct {
	RateLimit            json.RawMessage        `json:"rate_limit"`
	AdditionalRateLimits []codexAdditionalLimit `json:"additional_rate_limits"`
	RateLimitReachedType *struct {
		Type string `json:"type"`
	} `json:"rate_limit_reached_type"`
}

type codexAdditionalLimit struct {
	LimitName      string          `json:"limit_name"`
	MeteredFeature string          `json:"metered_feature"`
	RateLimit      *codexRateLimit `json:"rate_limit"`
}

type codexRateLimit struct {
	PrimaryWindow   *codexUsageWindow `json:"primary_window"`
	SecondaryWindow *codexUsageWindow `json:"secondary_window"`
}

type codexUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

// normalizeCodexUsage converts ChatGPT's usage document into the app-server
// rateLimits shape. It accepts exactly what the source-patched image accepts
// and fails with the same codes.
func normalizeCodexUsage(body []byte) (quotaRateLimitSet, error) {
	var payload codexUsagePayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return quotaRateLimitSet{}, quotaSchemaError("quota_field_type_invalid")
		}
		return quotaRateLimitSet{}, quotaSchemaError("quota_json_invalid")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return quotaRateLimitSet{}, quotaSchemaError("quota_json_invalid")
	}
	// Plan metadata does not determine usage: require a quota field, keeping
	// explicit null windows without accepting unrelated JSON as usage.
	if len(payload.RateLimit) == 0 && payload.AdditionalRateLimits == nil {
		return quotaRateLimitSet{}, quotaSchemaError("quota_rate_limit_missing")
	}
	var rateLimit *codexRateLimit
	if len(payload.RateLimit) != 0 {
		if err := json.Unmarshal(payload.RateLimit, &rateLimit); err != nil {
			return quotaRateLimitSet{}, quotaSchemaError("quota_field_type_invalid")
		}
	}
	var reached *string
	if payload.RateLimitReachedType != nil {
		reached = reachedType(payload.RateLimitReachedType.Type)
	}
	primary, err := normalizeCodexLimit(codexPrimaryLimitID, rateLimit, reached)
	if err != nil {
		return quotaRateLimitSet{}, err
	}
	byLimitID := map[string]quotaRateLimit{codexPrimaryLimitID: primary}
	if len(payload.AdditionalRateLimits) > maxQuotaExtraLimits {
		return quotaRateLimitSet{}, quotaSchemaError("quota_limits_excessive")
	}
	for _, additional := range payload.AdditionalRateLimits {
		limitID := strings.TrimSpace(additional.MeteredFeature)
		if !validQuotaLimitID(limitID) {
			return quotaRateLimitSet{}, quotaSchemaError("quota_limit_id_invalid")
		}
		if _, exists := byLimitID[limitID]; exists {
			return quotaRateLimitSet{}, quotaSchemaError("quota_limit_id_duplicate")
		}
		normalized, errLimit := normalizeCodexLimit(limitID, additional.RateLimit, nil)
		if errLimit != nil {
			return quotaRateLimitSet{}, errLimit
		}
		byLimitID[limitID] = normalized
	}
	return quotaRateLimitSet{RateLimits: primary, RateLimitsByLimitID: byLimitID}, nil
}

func normalizeCodexLimit(limitID string, limit *codexRateLimit, reached *string) (quotaRateLimit, error) {
	view := quotaRateLimit{LimitID: limitID, RateLimitReachedType: reached}
	if limit == nil {
		return view, nil
	}
	var err error
	if view.Primary, err = normalizeCodexWindow(limit.PrimaryWindow); err != nil {
		return quotaRateLimit{}, err
	}
	if view.Secondary, err = normalizeCodexWindow(limit.SecondaryWindow); err != nil {
		return quotaRateLimit{}, err
	}
	return view, nil
}

func normalizeCodexWindow(window *codexUsageWindow) (*quotaWindow, error) {
	if window == nil {
		return nil, nil
	}
	if window.UsedPercent == nil {
		return nil, quotaSchemaError("quota_percent_missing")
	}
	used := *window.UsedPercent
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return nil, quotaSchemaError("quota_percent_out_of_range")
	}
	if math.Trunc(used) != used {
		return nil, quotaSchemaError("quota_percent_fractional")
	}
	view := &quotaWindow{UsedPercent: int(used)}
	if window.LimitWindowSeconds != nil {
		seconds := *window.LimitWindowSeconds
		if seconds <= 0 || seconds > maxQuotaWindowSeconds {
			return nil, quotaSchemaError("quota_window_invalid")
		}
		minutes := (seconds + 59) / 60
		view.WindowDurationMins = &minutes
	}
	if window.ResetAt != nil {
		if *window.ResetAt < minQuotaResetUnix || *window.ResetAt > maxQuotaResetUnix {
			return nil, quotaSchemaError("quota_reset_invalid")
		}
		reset := *window.ResetAt
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

// antigravityQuotaFacade serves the credential page's live Gemini quota.
func (p *Plugin) antigravityQuotaFacade(query url.Values, body []byte) []byte {
	if !decodeFacadeRequest(body) {
		return facadeError(http.StatusBadRequest)
	}
	accountID, ok := quotaAccountID(query)
	if !ok {
		return facadeError(http.StatusNotFound)
	}
	account, found, err := p.inventory.Find(ProviderAntigravity, accountID)
	if err != nil || !found {
		return facadeError(http.StatusNotFound)
	}
	document, err := p.credentialDocument(account)
	if err != nil {
		return facadeError(http.StatusBadGateway)
	}
	token, project := stringField(document, "access_token"), stringField(document, "project_id")
	if !safeBearerToken(token) || project == "" || len(project) > 256 {
		return facadeError(http.StatusBadGateway)
	}
	payload, _ := json.Marshal(map[string]string{"project": project})
	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityQuotaURL, bytes.NewReader(payload))
	if err != nil {
		return facadeError(http.StatusBadGateway)
	}
	request.Header = http.Header{
		"Authorization": {"Bearer " + token},
		"Content-Type":  {"application/json"},
		"User-Agent":    {antigravityUserAgent},
	}
	status, data, err := p.quotaExchange(ctx, ProviderAntigravity, account, request)
	if err != nil || status != http.StatusOK || data == nil {
		return facadeError(http.StatusBadGateway)
	}
	quota, ok := normalizeAntigravityQuota(data)
	if !ok {
		return facadeError(http.StatusBadGateway)
	}
	return managementJSON(http.StatusOK, map[string]any{"status": "ok", "quota": quota})
}

// normalizeAntigravityQuota keeps Gemini models with a valid remaining share.
// A reset time is kept only when it is a well-formed timestamp in range.
func normalizeAntigravityQuota(body []byte) ([]facadeQuota, bool) {
	var payload struct {
		Models map[string]struct {
			QuotaInfo *struct {
				RemainingFraction *float64 `json:"remainingFraction"`
				ResetTime         string   `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Models == nil {
		return nil, false
	}
	quota := make([]facadeQuota, 0, len(payload.Models))
	for model, value := range payload.Models {
		info := value.QuotaInfo
		if info == nil || info.RemainingFraction == nil || !strings.HasPrefix(model, "gemini-") || !validModelName(model) {
			continue
		}
		fraction := *info.RemainingFraction
		if math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction < 0 || fraction > 1 {
			continue
		}
		item := facadeQuota{Model: model, RemainingFraction: fraction}
		if reset, err := time.Parse(time.RFC3339, info.ResetTime); err == nil &&
			reset.Unix() >= minQuotaResetUnix && reset.Unix() <= maxQuotaResetUnix {
			item.ResetTime = reset.UTC().Format(time.RFC3339)
		}
		quota = append(quota, item)
	}
	if len(quota) == 0 {
		return nil, false
	}
	sort.Slice(quota, func(i, j int) bool { return quota[i].Model < quota[j].Model })
	return quota, true
}

func validModelName(model string) bool {
	if len(model) > 64 {
		return false
	}
	for i := 0; i < len(model); i++ {
		c := model[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return false
		}
	}
	return true
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
	latest := observation.Quota
	models := make([]string, 0, len(observation.ModelQuotas))
	for model := range observation.ModelQuotas {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		if candidate := observation.ModelQuotas[model]; candidate.ObservedAt.After(latest.ObservedAt) {
			latest = candidate
		}
	}
	if latest.ObservedAt.IsZero() {
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

// credentialDocument reads a credential's stored document. Only the fields a
// query needs are taken from it; nothing is retained.
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

// quotaRoute is the proxy a query for this account must use: the account's
// own binding, otherwise CPA's global egress. There is no direct path.
func (p *Plugin) quotaRoute(ctx context.Context, provider string, account Account) (string, error) {
	route, err := p.alloc.ResolveEgress(ctx, provider, account.ID)
	if err != nil {
		return "", errQuotaEgress
	}
	if route.ProxyURL != "" {
		return route.ProxyURL, nil
	}
	if p.cfg.GlobalProxyURL == "" {
		return "", errQuotaEgress
	}
	return p.cfg.GlobalProxyURL, nil
}

// quotaExchange sends one provider request through the account's route and
// returns the status and, for a 2xx answer, a body of bounded size. Error
// bodies are drained and discarded. A nil body with a 2xx status means the
// body was too large or unreadable.
func (p *Plugin) quotaExchange(ctx context.Context, provider string, account Account, request *http.Request) (int, []byte, error) {
	proxyURL, err := p.quotaRoute(ctx, provider, account)
	if err != nil {
		logf("quota route of account %s is unavailable", account.ID)
		return 0, nil, err
	}
	response, err := p.quotaHTTP(ctx, proxyURL, request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return response.StatusCode, nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxQuotaResponseBytes+1))
	if err != nil || len(data) > maxQuotaResponseBytes {
		return response.StatusCode, nil, nil
	}
	return response.StatusCode, data, nil
}

// directQuotaHTTP sends request through proxyURL with a fresh transport: no
// connection is shared between accounts, redirects are returned rather than
// followed, and the environment's proxy settings are ignored.
func directQuotaHTTP(ctx context.Context, proxyURL string, request *http.Request) (*http.Response, error) {
	proxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return nil, errQuotaEgress
	}
	transport := &http.Transport{
		Proxy:                  http.ProxyURL(proxy),
		ForceAttemptHTTP2:      true,
		DisableKeepAlives:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  quotaTimeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client.Do(request.WithContext(ctx))
}

// parseProxyURL accepts the proxy forms CPA itself supports for its global
// egress. A path, query or fragment means the value is not a proxy origin.
func parseProxyURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, errQuotaEgress
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return parsed, nil
	}
	return nil, errQuotaEgress
}

func safeBearerToken(value string) bool {
	if value == "" || len(value) > maxBearerTokenBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= 0x20 || value[i] >= 0x7f {
			return false
		}
	}
	return true
}
