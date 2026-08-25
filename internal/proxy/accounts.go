package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	safeMetadataValuePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	maskedEmailPattern       = regexp.MustCompile(`^[A-Za-z0-9]\*{3}@[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	rateLimitIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

const (
	AccountRateLimitsMethod    = "account/rateLimits/read"
	AccountRateLimitsRequestID = int64(6)

	accountRateLimitsRequestBody   = `{"method":"account/rateLimits/read","id":6}`
	maxAccountRateLimitBuckets     = 33
	maxRateLimitWindowDurationMins = int64((31 * 24 * time.Hour) / time.Minute)
	minRateLimitResetUnix          = int64(946684800)  // 2000-01-01T00:00:00Z
	maxRateLimitResetUnix          = int64(4102444800) // 2100-01-01T00:00:00Z
)

type UpstreamAccount struct {
	ID           string    `json:"id"`
	MaskedEmail  string    `json:"masked_email"`
	Plan         string    `json:"plan"`
	Status       string    `json:"status"`
	LastSyncedAt time.Time `json:"last_synced_at"`
}

type upstreamAccountWire struct {
	ID           *string    `json:"id"`
	MaskedEmail  *string    `json:"masked_email"`
	Plan         *string    `json:"plan"`
	Status       *string    `json:"status"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
}

type RateLimitWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
	ResetsAt           *int64 `json:"resetsAt"`
}

type RateLimitSnapshot struct {
	LimitID              string           `json:"limitId"`
	LimitName            *string          `json:"limitName"`
	Primary              *RateLimitWindow `json:"primary"`
	Secondary            *RateLimitWindow `json:"secondary"`
	RateLimitReachedType *string          `json:"rateLimitReachedType"`
}

type AccountRateLimitsResult struct {
	RateLimits            RateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID   map[string]RateLimitSnapshot `json:"rateLimitsByLimitId"`
	RateLimitResetCredits *struct{}                    `json:"rateLimitResetCredits"`
}

type UpstreamQuota struct {
	ID     int64                   `json:"id"`
	Result AccountRateLimitsResult `json:"result"`
}

type rateLimitWindowWire struct {
	UsedPercent        *int   `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
	ResetsAt           *int64 `json:"resetsAt"`
}

type rateLimitSnapshotWire struct {
	LimitID              *string         `json:"limitId"`
	LimitName            json.RawMessage `json:"limitName"`
	Primary              json.RawMessage `json:"primary"`
	Secondary            json.RawMessage `json:"secondary"`
	RateLimitReachedType json.RawMessage `json:"rateLimitReachedType"`
}

type accountRateLimitsResultWire struct {
	RateLimits            *rateLimitSnapshotWire            `json:"rateLimits"`
	RateLimitsByLimitID   *map[string]rateLimitSnapshotWire `json:"rateLimitsByLimitId"`
	RateLimitResetCredits json.RawMessage                   `json:"rateLimitResetCredits"`
}

type upstreamQuotaWire struct {
	ID     *int64                       `json:"id"`
	Result *accountRateLimitsResultWire `json:"result"`
}

type InternalAPIError struct {
	StatusCode int    `json:"status_code"`
	Code       string `json:"code"`
	RetryAfter int    `json:"retry_after,omitempty"`
	Cause      error  `json:"-"`
}

func (e *InternalAPIError) Error() string {
	if e == nil {
		return ""
	}
	// Cause may contain a malformed value copied from the sidecar response
	// (for example time.ParseError includes the original JSON string). Keep it
	// available to errors.Is/errors.As without ever exposing it through normal
	// logging or an HTTP error response.
	return e.SafeCode()
}

func (e *InternalAPIError) Unwrap() error { return e.Cause }

func (c *Client) ListUpstreamAccounts(ctx context.Context) ([]UpstreamAccount, error) {
	var response struct {
		Accounts *[]upstreamAccountWire `json:"accounts"`
	}
	if err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts", &response); err != nil {
		return nil, err
	}
	if response.Accounts == nil {
		return nil, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	accounts := make([]UpstreamAccount, 0, len(*response.Accounts))
	seen := make(map[string]struct{}, len(*response.Accounts))
	for _, wire := range *response.Accounts {
		if wire.ID == nil || wire.MaskedEmail == nil || wire.Plan == nil || wire.Status == nil ||
			wire.LastSyncedAt == nil {
			return nil, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		account := UpstreamAccount{
			ID: *wire.ID, MaskedEmail: *wire.MaskedEmail, Plan: *wire.Plan,
			Status: *wire.Status, LastSyncedAt: *wire.LastSyncedAt,
		}
		if !upstreamAccountPattern.MatchString(account.ID) ||
			!safeMaskedEmail(account.MaskedEmail) ||
			!validUpstreamPlan(account.Plan) ||
			!validAccountStatus(account.Status) || account.LastSyncedAt.IsZero() {
			return nil, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		if _, ok := seen[account.ID]; ok {
			return nil, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		seen[account.ID] = struct{}{}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func (c *Client) QueryUpstreamAccountQuota(ctx context.Context, accountID string) (UpstreamQuota, error) {
	if !upstreamAccountPattern.MatchString(accountID) {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "invalid_upstream_account"}
	}
	var wire upstreamQuotaWire
	if err := c.internalJSONBody(ctx, http.MethodPost, "/internal/upstream-accounts/"+accountID+"/quota", accountRateLimitsRequestBody, &wire); err != nil {
		return UpstreamQuota{}, err
	}
	if wire.ID == nil || *wire.ID != AccountRateLimitsRequestID || wire.Result == nil ||
		wire.Result.RateLimits == nil || wire.Result.RateLimitsByLimitID == nil ||
		!isJSONNull(wire.Result.RateLimitResetCredits) {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	rateLimits, ok := normalizedRateLimitSnapshot(*wire.Result.RateLimits, "codex")
	if !ok {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	byLimitIDWire := *wire.Result.RateLimitsByLimitID
	if len(byLimitIDWire) == 0 || len(byLimitIDWire) > maxAccountRateLimitBuckets {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	byLimitID := make(map[string]RateLimitSnapshot, len(byLimitIDWire))
	for limitID, snapshotWire := range byLimitIDWire {
		if !rateLimitIDPattern.MatchString(limitID) {
			return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		snapshot, valid := normalizedRateLimitSnapshot(snapshotWire, limitID)
		if !valid {
			return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		byLimitID[limitID] = snapshot
	}
	if codex, exists := byLimitID["codex"]; !exists || !equalRateLimitSnapshot(rateLimits, codex) {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	return UpstreamQuota{
		ID: AccountRateLimitsRequestID,
		Result: AccountRateLimitsResult{
			RateLimits: rateLimits, RateLimitsByLimitID: byLimitID, RateLimitResetCredits: nil,
		},
	}, nil
}

func (c *Client) internalJSON(ctx context.Context, method, path string, destination any) error {
	return c.internalJSONBody(ctx, method, path, "", destination)
}

func (c *Client) internalJSONBody(ctx context.Context, method, path, body string, destination any) error {
	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	target.RawQuery = ""
	target.Fragment = ""
	var requestBody io.Reader
	if body != "" {
		requestBody = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), requestBody)
	if err != nil {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_unavailable", Cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		code := "sidecar_unavailable"
		status := http.StatusBadGateway
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeout(err) {
			code = "sidecar_timeout"
			status = http.StatusGatewayTimeout
		}
		return &InternalAPIError{StatusCode: status, Code: code, Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return internalStatusError(response)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	limitedBody, err := io.ReadAll(io.LimitReader(response.Body, maxInternalResponseBodyBytes+1))
	if err != nil || len(limitedBody) > maxInternalResponseBodyBytes {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response", Cause: err}
	}
	if err := rejectDuplicateJSONKeys(limitedBody); err != nil {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response", Cause: err}
	}
	decoder := json.NewDecoder(bytes.NewReader(limitedBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response", Cause: err}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	return nil
}

func internalStatusError(response *http.Response) error {
	code := "sidecar_request_failed"
	switch response.StatusCode {
	case http.StatusNotFound:
		code = "invalid_upstream_account"
	case http.StatusUnauthorized, http.StatusForbidden:
		code = "upstream_reauthentication_required"
	case http.StatusTooManyRequests:
		code = "upstream_quota_rate_limited"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		code = "sidecar_timeout"
	default:
		if response.StatusCode >= http.StatusInternalServerError {
			code = "sidecar_unavailable"
		}
	}
	retryAfter := 0
	if parsed, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); err == nil && parsed > 0 && parsed <= 3600 {
		retryAfter = parsed
	}
	// Drain only a bounded amount so the connection can be reused without ever
	// parsing, returning, or logging a potentially sensitive upstream body.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
	return &InternalAPIError{StatusCode: response.StatusCode, Code: code, RetryAfter: retryAfter}
}

func safeMaskedEmail(value string) bool {
	trimmed := strings.TrimSpace(value)
	return value == trimmed && len(value) >= 7 && len(value) <= 254 && maskedEmailPattern.MatchString(value)
}

func validAccountStatus(value string) bool {
	switch value {
	case "active", "unavailable", "disabled", "error":
		return true
	default:
		return false
	}
}

func validUpstreamPlan(value string) bool {
	switch value {
	case "plus", "pro", "unknown":
		return true
	default:
		return false
	}
}

func isJSONNull(value json.RawMessage) bool {
	return len(value) > 0 && bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func normalizedRateLimitSnapshot(wire rateLimitSnapshotWire, expectedLimitID string) (RateLimitSnapshot, bool) {
	if wire.LimitID == nil || *wire.LimitID != expectedLimitID || !rateLimitIDPattern.MatchString(*wire.LimitID) || !isJSONNull(wire.LimitName) {
		return RateLimitSnapshot{}, false
	}
	primary, ok := normalizedRateLimitWindowJSON(wire.Primary)
	if !ok {
		return RateLimitSnapshot{}, false
	}
	secondary, ok := normalizedRateLimitWindowJSON(wire.Secondary)
	if !ok {
		return RateLimitSnapshot{}, false
	}
	reachedType, ok := normalizedRateLimitReachedType(wire.RateLimitReachedType)
	if !ok {
		return RateLimitSnapshot{}, false
	}
	return RateLimitSnapshot{
		LimitID: *wire.LimitID, LimitName: nil, Primary: primary, Secondary: secondary,
		RateLimitReachedType: reachedType,
	}, true
}

func normalizedRateLimitWindowJSON(raw json.RawMessage) (*RateLimitWindow, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if isJSONNull(raw) {
		return nil, true
	}
	var wire rateLimitWindowWire
	if err := decodeStrictJSON(raw, &wire); err != nil {
		return nil, false
	}
	if wire.UsedPercent == nil || *wire.UsedPercent < 0 || *wire.UsedPercent > 100 {
		return nil, false
	}
	if wire.WindowDurationMins != nil && (*wire.WindowDurationMins <= 0 || *wire.WindowDurationMins > maxRateLimitWindowDurationMins) {
		return nil, false
	}
	if wire.ResetsAt != nil && (*wire.ResetsAt < minRateLimitResetUnix || *wire.ResetsAt > maxRateLimitResetUnix) {
		return nil, false
	}
	return &RateLimitWindow{
		UsedPercent: *wire.UsedPercent, WindowDurationMins: wire.WindowDurationMins, ResetsAt: wire.ResetsAt,
	}, true
}

func normalizedRateLimitReachedType(raw json.RawMessage) (*string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if isJSONNull(raw) {
		return nil, true
	}
	var value string
	if err := decodeStrictJSON(raw, &value); err != nil {
		return nil, false
	}
	switch value {
	case "rate_limit_reached",
		"workspace_owner_credits_depleted",
		"workspace_member_credits_depleted",
		"workspace_owner_usage_limit_reached",
		"workspace_member_usage_limit_reached":
		return &value, true
	default:
		return nil, false
	}
}

func decodeStrictJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func equalRateLimitSnapshot(left, right RateLimitSnapshot) bool {
	return left.LimitID == right.LimitID && left.LimitName == nil && right.LimitName == nil &&
		equalOptionalString(left.RateLimitReachedType, right.RateLimitReachedType) &&
		equalRateLimitWindow(left.Primary, right.Primary) && equalRateLimitWindow(left.Secondary, right.Secondary)
}

func equalOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalRateLimitWindow(left, right *RateLimitWindow) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.UsedPercent == right.UsedPercent &&
		equalOptionalInt64(left.WindowDurationMins, right.WindowDurationMins) &&
		equalOptionalInt64(left.ResetsAt, right.ResetsAt)
}

func equalOptionalInt64(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, exists := seen[key]; exists {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if valueErr := consumeJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return closeErr
		}
		if closing != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if valueErr := consumeJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return closeErr
		}
		if closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func (e *InternalAPIError) SafeCode() string {
	if e == nil || !safeMetadataValuePattern.MatchString(e.Code) {
		return "sidecar_request_failed"
	}
	return e.Code
}

func (e *InternalAPIError) String() string {
	if e == nil {
		return "proxy.InternalAPIError{}"
	}
	return fmt.Sprintf("proxy.InternalAPIError{status=%d, code=%q}", e.StatusCode, e.SafeCode())
}

func (e *InternalAPIError) GoString() string { return e.String() }
