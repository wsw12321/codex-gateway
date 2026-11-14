package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Model identification with the CPA plugin. A probe is an ordinary Responses
// request: CPA builds the upstream request exactly as it does for a client.
// The Gateway pins it to one account with a one-time probe identity: its
// allocation callbacks answer with that account alone (see ProbeAccount), so
// CPA can neither pick nor retry another account. The probe is not admitted,
// billed or recorded as usage.
const (
	maxProbeOutputTokens = 16384
	maxProbeOutputBytes  = 16 << 10
)

// probeResponse is the part of a Responses answer a probe reads.
type probeResponse struct {
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (c *Client) probeThroughCPA(ctx context.Context, probe ModelIdentificationProbe) (string, error) {
	if c.requireEgress {
		if err := c.RequireAccountAccessCapability(ctx); err != nil {
			return "", err
		}
	}
	body, err := json.Marshal(struct {
		Model           string `json:"model"`
		Input           string `json:"input"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Stream          bool   `json:"stream"`
	}{probe.Model, probe.Prompt, maxProbeOutputTokens, false})
	if err != nil {
		return "", diagnosticError("probe_request_invalid", http.StatusBadRequest)
	}
	// A fresh caller scope per probe: no session state is shared with any
	// client or with another probe.
	var scope [32]byte
	if _, err = rand.Read(scope[:]); err != nil {
		return "", diagnosticError("probe_unavailable", http.StatusServiceUnavailable)
	}
	identity, release := c.ledger.PinProbe(probe.AccountID)
	defer release()

	ctx, cancel := context.WithTimeout(ctx, ModelIdentificationProbeTimeout)
	defer cancel()
	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + "/v1/responses"
	target.RawQuery, target.Fragment = "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return "", diagnosticError("probe_request_invalid", http.StatusBadRequest)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	request.Header.Set(affinityHeader, base64.RawURLEncoding.EncodeToString(scope[:]))
	request.Header.Set(gatewayUserHeader, identity)
	opened, err := c.openDispatch(request.Header, ForwardOptions{UserID: identity})
	if err != nil || opened == nil {
		return "", diagnosticError("probe_unavailable", http.StatusServiceUnavailable)
	}
	defer opened.close()

	response, err := c.diagnosticHTTP.Do(request)
	if err != nil {
		return "", diagnosticTransportError(ctx, err)
	}
	defer response.Body.Close()
	entry := opened.snapshot()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// A probe cancelled or timed out while its error body was still
		// arriving ended for that reason, not for the status it began with.
		if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxInternalErrorBodyBytes)); err != nil && ctx.Err() != nil {
			return "", diagnosticTransportError(ctx, err)
		}
		return "", probeStatusError(response, entry.rejected)
	}
	if entry.accountID != probe.AccountID {
		return "", diagnosticError("probe_account_mismatch", http.StatusBadGateway)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", diagnosticError("probe_invalid_response", http.StatusBadGateway)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxInternalResponseBodyBytes+1))
	if err != nil {
		return "", diagnosticTransportError(ctx, err)
	}
	if len(data) > maxInternalResponseBodyBytes {
		return "", diagnosticError("probe_response_too_large", http.StatusBadGateway)
	}
	return probeOutputText(data)
}

// probeStatusError maps a refused probe to the fixed diagnostic codes. An
// allocation refusal reported by the plugin takes precedence over the status
// CPA rendered for it.
func probeStatusError(response *http.Response, rejected string) error {
	switch rejected {
	case allocationRejectedConcurrency:
		return diagnosticError("probe_rate_limited", http.StatusTooManyRequests)
	case allocationRejectedUnavailable:
		return diagnosticError("probe_unavailable", http.StatusServiceUnavailable)
	}
	status := response.StatusCode
	var failure *ModelIdentificationError
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		failure = diagnosticError("probe_authentication_failed", status)
	case status == http.StatusTooManyRequests:
		failure = diagnosticError("probe_rate_limited", status)
		if seconds, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); err == nil && seconds > 0 && seconds <= 3600 {
			failure.RetryAfter = seconds
		}
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		failure = diagnosticError("probe_timeout", status)
	case status >= 500:
		failure = diagnosticError("probe_upstream_unavailable", status)
	default:
		failure = diagnosticError("probe_upstream_rejected", status)
	}
	failure.UpstreamStatus = status
	return failure
}

// probeOutputText reads the answer text of a completed, text-only response.
func probeOutputText(data []byte) (string, error) {
	var answer probeResponse
	if !utf8.Valid(data) || rejectDuplicateJSONKeys(data) != nil || json.Unmarshal(data, &answer) != nil {
		return "", diagnosticError("probe_invalid_response", http.StatusBadGateway)
	}
	switch answer.Status {
	case "completed":
	case "incomplete":
		return "", diagnosticError("probe_response_incomplete", http.StatusBadGateway)
	default:
		return "", diagnosticError("probe_invalid_response", http.StatusBadGateway)
	}
	if answer.Usage != nil && (answer.Usage.OutputTokens < 0 || answer.Usage.OutputTokens > maxProbeOutputTokens) {
		return "", diagnosticError("probe_output_tokens_exceeded", http.StatusBadGateway)
	}
	var text strings.Builder
	for _, item := range answer.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				}
				if text.Len() > maxProbeOutputBytes {
					return "", diagnosticError("probe_output_too_large", http.StatusBadGateway)
				}
			}
		case "reasoning":
		default:
			return "", diagnosticError("probe_unexpected_tool", http.StatusBadGateway)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", diagnosticError("probe_empty_output", http.StatusBadGateway)
	}
	return text.String(), nil
}
