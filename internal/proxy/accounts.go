package proxy

import (
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

type QuotaWindow struct {
	UsedRatio      float64    `json:"used_ratio"`
	RemainingRatio float64    `json:"remaining_ratio"`
	ResetAt        *time.Time `json:"reset_at"`
	WindowSeconds  *int64     `json:"window_seconds,omitempty"`
}

type AdditionalQuotaWindow struct {
	Name string `json:"name"`
	QuotaWindow
}

type UpstreamQuota struct {
	QueriedAt         time.Time               `json:"queried_at"`
	Plan              string                  `json:"plan"`
	FiveHour          QuotaWindow             `json:"five_hour"`
	SevenDay          QuotaWindow             `json:"seven_day"`
	AdditionalWindows []AdditionalQuotaWindow `json:"additional_windows"`
}

type quotaWindowWire struct {
	UsedRatio      *float64   `json:"used_ratio"`
	RemainingRatio *float64   `json:"remaining_ratio"`
	ResetAt        *time.Time `json:"reset_at"`
	WindowSeconds  *int64     `json:"window_seconds,omitempty"`
}

type additionalQuotaWindowWire struct {
	Name           *string    `json:"name"`
	UsedRatio      *float64   `json:"used_ratio"`
	RemainingRatio *float64   `json:"remaining_ratio"`
	ResetAt        *time.Time `json:"reset_at"`
	WindowSeconds  *int64     `json:"window_seconds,omitempty"`
}

type upstreamQuotaWire struct {
	QueriedAt         *time.Time                  `json:"queried_at"`
	Plan              *string                     `json:"plan"`
	FiveHour          *quotaWindowWire            `json:"five_hour"`
	SevenDay          *quotaWindowWire            `json:"seven_day"`
	AdditionalWindows []additionalQuotaWindowWire `json:"additional_windows"`
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
	if err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts/"+accountID+"/quota", &wire); err != nil {
		return UpstreamQuota{}, err
	}
	if wire.QueriedAt == nil || wire.Plan == nil || wire.FiveHour == nil || wire.SevenDay == nil {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	fiveHour, ok := normalizedQuotaWindow(*wire.FiveHour, false)
	if !ok {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	sevenDay, ok := normalizedQuotaWindow(*wire.SevenDay, false)
	if !ok || wire.QueriedAt.IsZero() || !validQuotaPlan(*wire.Plan) {
		return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	quota := UpstreamQuota{
		QueriedAt: *wire.QueriedAt, Plan: *wire.Plan, FiveHour: fiveHour, SevenDay: sevenDay,
		AdditionalWindows: make([]AdditionalQuotaWindow, 0, len(wire.AdditionalWindows)),
	}
	for index, additionalWire := range wire.AdditionalWindows {
		window, ok := normalizedQuotaWindow(quotaWindowWire{
			UsedRatio: additionalWire.UsedRatio, RemainingRatio: additionalWire.RemainingRatio,
			ResetAt: additionalWire.ResetAt, WindowSeconds: additionalWire.WindowSeconds,
		}, true)
		if !ok || additionalWire.Name == nil || strings.TrimSpace(*additionalWire.Name) == "" {
			return UpstreamQuota{}, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		// An upstream-controlled label is not needed to render the normalized
		// window and could otherwise smuggle secret fragments through a
		// syntactically safe string. Replace it with a gateway-generated name.
		quota.AdditionalWindows = append(quota.AdditionalWindows, AdditionalQuotaWindow{
			Name: fmt.Sprintf("additional_%d", index+1), QuotaWindow: window,
		})
	}
	return quota, nil
}

func (c *Client) internalJSON(ctx context.Context, method, path string, destination any) error {
	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	target.RawQuery = ""
	target.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_unavailable", Cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
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
	limited := &io.LimitedReader{R: response.Body, N: maxInternalResponseBodyBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response", Cause: err}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	if limited.N == 0 {
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

func validQuotaPlan(value string) bool {
	return value == "plus" || value == "pro"
}

func normalizedQuotaWindow(wire quotaWindowWire, allowSeconds bool) (QuotaWindow, bool) {
	if wire.UsedRatio == nil || wire.RemainingRatio == nil || wire.ResetAt == nil {
		return QuotaWindow{}, false
	}
	window := QuotaWindow{
		UsedRatio: *wire.UsedRatio, RemainingRatio: *wire.RemainingRatio,
		ResetAt: wire.ResetAt, WindowSeconds: wire.WindowSeconds,
	}
	return window, validQuotaWindow(window, allowSeconds)
}

func validQuotaWindow(window QuotaWindow, allowSeconds bool) bool {
	if window.UsedRatio < 0 || window.UsedRatio > 1 || window.RemainingRatio < 0 || window.RemainingRatio > 1 {
		return false
	}
	if difference := window.UsedRatio + window.RemainingRatio - 1; difference < -0.000001 || difference > 0.000001 {
		return false
	}
	if window.ResetAt == nil || window.ResetAt.IsZero() {
		return false
	}
	if window.WindowSeconds != nil && (!allowSeconds || *window.WindowSeconds <= 0 || *window.WindowSeconds > int64((31*24*time.Hour)/time.Second)) {
		return false
	}
	return true
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
