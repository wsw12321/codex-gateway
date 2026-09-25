package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// ListUpstreamAccountModels asks the compatibility sidecar for models available
// to one stable credential, rather than relying on the pooled /v1/models list.
func (c *Client) ListUpstreamAccountModels(ctx context.Context, accountID string) ([]string, error) {
	if !upstreamAccountPattern.MatchString(accountID) {
		return nil, &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "invalid_upstream_account"}
	}
	var response struct {
		AccountID string    `json:"account_id"`
		Models    *[]string `json:"models"`
	}
	if err := c.internalJSON(ctx, http.MethodGet, "/internal/upstream-accounts/"+accountID+"/models", &response); err != nil {
		return nil, err
	}
	if response.AccountID != accountID || response.Models == nil || len(*response.Models) > 256 {
		return nil, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	seen := make(map[string]struct{}, len(*response.Models))
	for _, model := range *response.Models {
		if !validSidecarModelID(model) {
			return nil, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		if _, duplicate := seen[model]; duplicate {
			return nil, &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
		}
		seen[model] = struct{}{}
	}
	return *response.Models, nil
}

func validSidecarModelID(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for _, value := range model {
		if (value < 'a' || value > 'z') && (value < 'A' || value > 'Z') &&
			(value < '0' || value > '9') && value != '-' && value != '_' &&
			value != '.' && value != ':' && value != '/' {
			return false
		}
	}
	return true
}

// ProbeUpstreamAccount sends exactly one text-only Responses request through a
// sidecar endpoint that pins every selection and retry to accountID. The
// response is kept in memory for scoring and must never be logged or stored.
func (c *Client) ProbeUpstreamAccount(ctx context.Context, accountID, model, prompt string) (string, error) {
	return c.ProbeUpstreamAccountAsUser(ctx, "", accountID, model, prompt)
}

// ProbeUpstreamAccountAsUser sends a pinned probe while preserving the
// authenticated Gateway user identity needed by the sidecar's account-access
// selector. The identity is a trusted value from the Gateway session, never a
// client-supplied header.
func (c *Client) ProbeUpstreamAccountAsUser(ctx context.Context, userID, accountID, model, prompt string) (string, error) {
	if !upstreamAccountPattern.MatchString(accountID) || !validCatalogModelID(model) ||
		prompt == "" || len(prompt) > 8<<10 || !utf8.ValidString(prompt) ||
		(userID != "" && !gatewayUserPattern.MatchString(userID)) {
		return "", &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "invalid_model_probe"}
	}
	body, err := json.Marshal(struct {
		Model           string `json:"model"`
		Input           string `json:"input"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}{Model: model, Input: prompt, MaxOutputTokens: 16384})
	if err != nil {
		return "", &InternalAPIError{StatusCode: http.StatusBadRequest, Code: "invalid_model_probe"}
	}
	var response struct {
		AccountID  string `json:"account_id"`
		OutputText string `json:"output_text"`
	}
	var headers http.Header
	if userID != "" {
		headers = http.Header{gatewayUserHeader: {userID}}
	}
	if err := c.internalJSONBodyWithHeaders(ctx, http.MethodPost, "/internal/upstream-accounts/"+accountID+"/probe", string(body), headers, &response); err != nil {
		return "", err
	}
	if response.AccountID != accountID {
		return "", &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "probe_account_mismatch"}
	}
	if strings.TrimSpace(response.OutputText) == "" ||
		len(response.OutputText) > 512<<10 || !utf8.ValidString(response.OutputText) {
		return "", &InternalAPIError{StatusCode: http.StatusBadGateway, Code: "sidecar_invalid_response"}
	}
	return response.OutputText, nil
}
