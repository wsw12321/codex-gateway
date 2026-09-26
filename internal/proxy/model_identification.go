package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ModelIdentificationProtocol = "model_identification_direct_v1"
	// The sidecar stops a probe at 180 seconds; allow time for its terminal reply.
	ModelIdentificationProbeTimeout = 185 * time.Second
	modelIdentificationPath         = "/internal/model-identification"
	diagnosticRunHeader             = "X-Codex-Diagnostic-Run"
	diagnosticProbeHeader           = "X-Codex-Diagnostic-Probe"
)

// ModelIdentificationProbe contains trusted task metadata, not client headers.
// Only the Owner-only identification worker may construct these requests.
type ModelIdentificationProbe struct {
	RunID      string
	UserID     string
	AccountID  string
	Model      string
	Prompt     string
	ProbeIndex int
}

// ValidModelIdentificationModel validates a native catalog identifier without
// applying the data plane's pricing, aliases or routing restrictions.
func ValidModelIdentificationModel(model string) bool { return validSidecarModelID(model) }

// ModelIdentificationError retains safe diagnostic facts without retaining a
// sidecar or upstream response. Cause is available only for cancellation checks.
type ModelIdentificationError struct {
	Code           string
	Stage          string
	StatusCode     int
	UpstreamStatus int
	RetryAfter     int
	Cause          error `json:"-"`
}

func (e *ModelIdentificationError) Error() string { return e.SafeCode() }
func (e *ModelIdentificationError) Unwrap() error { return e.Cause }
func (e *ModelIdentificationError) SafeCode() string {
	if e != nil {
		if _, ok := diagnosticErrorStages[e.Code]; ok {
			return e.Code
		}
	}
	return "probe_unavailable"
}

// Restrict both the code and its source stage to values emitted by the pinned
// diagnostic protocol. Neither an arbitrary upstream message nor its code is safe.
var diagnosticErrorStages = map[string]string{
	"protocol_unsupported":         "preflight",
	"probe_request_invalid":        "preflight",
	"probe_actor_invalid":          "preflight",
	"probe_context_invalid":        "preflight",
	"probe_account_not_found":      "preflight",
	"probe_account_mismatch":       "preflight",
	"probe_credential_unavailable": "preflight",
	"probe_model_unavailable":      "preflight",
	"probe_unavailable":            "preflight",
	"probe_timeout":                "probing",
	"probe_canceled":               "probing",
	"probe_authentication_failed":  "probing",
	"probe_rate_limited":           "probing",
	"probe_model_unsupported":      "probing",
	"probe_upstream_unavailable":   "probing",
	"probe_upstream_rejected":      "probing",
	"probe_network_failed":         "probing",
	"probe_response_incomplete":    "validating",
	"probe_invalid_response":       "validating",
	"probe_response_too_large":     "validating",
	"probe_output_too_large":       "validating",
	"probe_output_tokens_exceeded": "validating",
	"probe_unexpected_tool":        "validating",
	"probe_empty_output":           "validating",
}

func diagnosticError(code string, status int) *ModelIdentificationError {
	return &ModelIdentificationError{Code: code, Stage: diagnosticErrorStages[code], StatusCode: status}
}

// The ordinary transport's 90-second header limit is suitable for streaming
// traffic, but a diagnostic returns headers only after the whole answer. Clone
// the transport rather than changing the shared data-plane client.
func newDiagnosticHTTPClient(client *http.Client) *http.Client {
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if concrete, ok := transport.(*http.Transport); ok {
		copyTransport := concrete.Clone()
		copyTransport.ResponseHeaderTimeout = 0
		copyClient.Transport = copyTransport
	}
	return &copyClient
}

func (c *Client) RequireModelIdentificationCapability(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response struct {
		Protocol string `json:"protocol"`
	}
	if err := c.diagnosticJSON(ctx, http.MethodGet, modelIdentificationPath+"/capabilities", nil, nil, &response); err != nil {
		return err
	}
	if response.Protocol != ModelIdentificationProtocol {
		return diagnosticError("protocol_unsupported", http.StatusServiceUnavailable)
	}
	return nil
}

// ListModelIdentificationModels returns native model candidates independently
// of ordinary routing availability, plan metadata, aliases and pricing rules.
func (c *Client) ListModelIdentificationModels(ctx context.Context, accountID string) ([]string, error) {
	if !upstreamAccountPattern.MatchString(accountID) {
		return nil, diagnosticError("probe_request_invalid", http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response struct {
		AccountID string    `json:"account_id"`
		Models    *[]string `json:"models"`
	}
	if err := c.diagnosticJSON(ctx, http.MethodGet, modelIdentificationPath+"/accounts/"+accountID+"/models", nil, nil, &response); err != nil {
		return nil, err
	}
	if response.AccountID != accountID || response.Models == nil || len(*response.Models) > 256 {
		return nil, diagnosticError("probe_invalid_response", http.StatusBadGateway)
	}
	seen := make(map[string]bool, len(*response.Models))
	for _, model := range *response.Models {
		if !validSidecarModelID(model) || seen[model] {
			return nil, diagnosticError("probe_invalid_response", http.StatusBadGateway)
		}
		seen[model] = true
	}
	return *response.Models, nil
}

func (c *Client) ProbeModelIdentification(ctx context.Context, probe ModelIdentificationProbe) (string, error) {
	if !gatewayUserPattern.MatchString(probe.UserID) {
		return "", diagnosticError("probe_actor_invalid", http.StatusBadRequest)
	}
	if !gatewayUserPattern.MatchString(probe.RunID) || probe.ProbeIndex < 1 || probe.ProbeIndex > 3 {
		return "", diagnosticError("probe_context_invalid", http.StatusBadRequest)
	}
	if !upstreamAccountPattern.MatchString(probe.AccountID) || !validSidecarModelID(probe.Model) || probe.Prompt == "" ||
		len(probe.Prompt) > 8<<10 || !utf8.ValidString(probe.Prompt) || strings.ContainsRune(probe.Prompt, 0) {
		return "", diagnosticError("probe_request_invalid", http.StatusBadRequest)
	}
	body, err := json.Marshal(struct {
		Model           string `json:"model"`
		Input           string `json:"input"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}{probe.Model, probe.Prompt, 16384})
	if err != nil {
		return "", diagnosticError("probe_request_invalid", http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, ModelIdentificationProbeTimeout)
	defer cancel()
	headers := http.Header{
		gatewayUserHeader:     {probe.UserID},
		diagnosticRunHeader:   {probe.RunID},
		diagnosticProbeHeader: {strconv.Itoa(probe.ProbeIndex)},
	}
	var response struct {
		AccountID  string `json:"account_id"`
		OutputText string `json:"output_text"`
	}
	if err := c.diagnosticJSON(ctx, http.MethodPost, modelIdentificationPath+"/accounts/"+probe.AccountID+"/probe", body, headers, &response); err != nil {
		return "", err
	}
	if response.AccountID != probe.AccountID {
		return "", diagnosticError("probe_account_mismatch", http.StatusBadGateway)
	}
	if !utf8.ValidString(response.OutputText) || len(response.OutputText) > 16<<10 {
		return "", diagnosticError("probe_output_too_large", http.StatusBadGateway)
	}
	if strings.TrimSpace(response.OutputText) == "" {
		return "", diagnosticError("probe_empty_output", http.StatusBadGateway)
	}
	return response.OutputText, nil
}

func (c *Client) diagnosticJSON(ctx context.Context, method, path string, body []byte, headers http.Header, destination any) error {
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawQuery, target.Fragment = "", ""
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return diagnosticError("probe_request_invalid", http.StatusBadRequest)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.diagnosticHTTP.Do(request)
	if err != nil {
		return diagnosticTransportError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return diagnosticStatusError(ctx, response)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return diagnosticError("probe_invalid_response", http.StatusBadGateway)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxInternalResponseBodyBytes+1))
	if err != nil {
		return diagnosticTransportError(ctx, err)
	}
	if len(data) > maxInternalResponseBodyBytes {
		return diagnosticError("probe_response_too_large", http.StatusBadGateway)
	}
	if !utf8.Valid(data) || rejectDuplicateJSONKeys(data) != nil || decodeStrictJSON(data, destination) != nil {
		return diagnosticError("probe_invalid_response", http.StatusBadGateway)
	}
	return nil
}

func diagnosticTransportError(ctx context.Context, cause error) error {
	code, status := "probe_network_failed", http.StatusBadGateway
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeout(cause) {
		code, status = "probe_timeout", http.StatusGatewayTimeout
	} else if errors.Is(ctx.Err(), context.Canceled) {
		code = "probe_canceled"
	}
	failure := diagnosticError(code, status)
	failure.Cause = cause
	return failure
}

func diagnosticStatusError(ctx context.Context, response *http.Response) error {
	failure := diagnosticError("probe_unavailable", response.StatusCode)
	switch response.StatusCode {
	case http.StatusNotFound:
		failure = diagnosticError("protocol_unsupported", response.StatusCode)
	case http.StatusUnauthorized, http.StatusForbidden:
		failure = diagnosticError("probe_actor_invalid", response.StatusCode)
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		failure = diagnosticError("probe_timeout", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxInternalErrorBodyBytes+1))
	if err != nil {
		return diagnosticTransportError(ctx, err)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if len(body) > maxInternalErrorBodyBytes || !utf8.Valid(body) || mediaErr != nil || mediaType != "application/json" || rejectDuplicateJSONKeys(body) != nil {
		return failure
	}
	// Pointer fields distinguish missing required facts from zero values. No
	// arbitrary message or unknown field is accepted or retained.
	var envelope struct {
		Error *struct {
			Code           *string `json:"code"`
			Stage          *string `json:"stage"`
			UpstreamStatus int     `json:"upstream_status,omitempty"`
			RetryAfter     int     `json:"retry_after,omitempty"`
		} `json:"error"`
	}
	if decodeStrictJSON(body, &envelope) != nil || envelope.Error == nil || envelope.Error.Code == nil || envelope.Error.Stage == nil {
		return failure
	}
	wire := envelope.Error
	stage, known := diagnosticErrorStages[*wire.Code]
	if !known || stage != *wire.Stage || (wire.UpstreamStatus != 0 && (wire.UpstreamStatus < 100 || wire.UpstreamStatus > 599)) || wire.RetryAfter < 0 || wire.RetryAfter > 3600 {
		return failure
	}
	return &ModelIdentificationError{Code: *wire.Code, Stage: stage, StatusCode: response.StatusCode, UpstreamStatus: wire.UpstreamStatus, RetryAfter: wire.RetryAfter}
}
