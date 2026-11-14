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
	"strings"
	"time"
)

// Bound a complete response and a stream alike, allowing room for JSON
// escaping and protocol metadata.
const maxGeminiResponseBytes = 16 << 20

// ForwardGemini only supports configured Antigravity models. Native Gemini
// requests must never reach the primary Codex client.
func (r *Router) ForwardGemini(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, upstreamPath string, options ForwardOptions) (Result, *Failure) {
	if !r.IsAntigravityModel(model) {
		return Result{}, &Failure{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "model_not_found", Message: "未配置此 Gemini 模型"}
	}
	if r.antigravity == nil {
		return Result{}, &Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_unavailable", Message: "Antigravity 尚未就绪"}
	}
	if incoming.Method != http.MethodPost || !validCatalogModelID(model) ||
		(upstreamPath != "/v1beta/models/"+model+":generateContent" && upstreamPath != "/v1beta/models/"+model+":streamGenerateContent") {
		return Result{}, &Failure{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "unsupported_endpoint", Message: "不支持的接口"}
	}
	return r.antigravity.forwardGemini(ctx, w, incoming, model, upstreamPath, options)
}

func (c *Client) forwardGemini(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, upstreamPath string, options ForwardOptions) (Result, *Failure) {
	body, err := io.ReadAll(io.LimitReader(incoming.Body, maxInternalResponseBodyBytes+1))
	if err != nil {
		return Result{}, c.transportFailure(ctx, err)
	}
	if len(body) > maxInternalResponseBodyBytes {
		return Result{}, &Failure{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "antigravity_request_too_large", Message: "Antigravity 请求体超过 1 MiB 限制"}
	}
	declaredTools, err := geminiDeclaredTools(body)
	if err != nil {
		return Result{}, &Failure{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "antigravity_invalid_request", Message: "Antigravity 请求格式无效", Cause: err}
	}
	stream := strings.HasSuffix(upstreamPath, ":streamGenerateContent")
	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + upstreamPath
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	if stream {
		target.RawQuery = "alt=sse"
	}
	outgoing, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, protocolFailure(err)
	}
	// Construct internal headers from scratch: caller API keys, cookies, account
	// affinity, and arbitrary query credentials have no role in this protocol.
	outgoing.Header.Set("Authorization", "Bearer "+c.token)
	outgoing.Header.Set("Content-Type", "application/json")
	outgoing.Header.Set("Cache-Control", "no-store")
	if options.AffinityScope != "" {
		if !affinityScopePattern.MatchString(options.AffinityScope) {
			return Result{}, protocolFailure(errors.New("invalid upstream affinity scope"))
		}
		outgoing.Header.Set(affinityHeader, options.AffinityScope)
	}
	if options.UserID != "" {
		if !gatewayUserPattern.MatchString(options.UserID) {
			return Result{}, protocolFailure(errors.New("invalid upstream user identity"))
		}
		if err := c.prepareAccountScope(ctx, &options); err != nil {
			return Result{}, &Failure{Status: http.StatusServiceUnavailable, Type: "server_error", Code: "upstream_access_protocol_unavailable", Message: "上游账号权限服务不可用", Cause: err}
		}
		outgoing.Header.Set(gatewayUserHeader, options.UserID)
	}
	if err := setGroupAccountScope(outgoing.Header, options); err != nil {
		return Result{}, protocolFailure(err)
	}
	if stream {
		outgoing.Header.Set("Accept", "text/event-stream")
	} else {
		outgoing.Header.Set("Accept", "application/json")
	}
	opened, err := c.openDispatch(outgoing.Header, options)
	if err != nil {
		return Result{}, protocolFailure(err)
	}
	defer opened.close()
	response, err := c.http.Do(outgoing)
	if err != nil {
		return Result{}, c.transportFailure(ctx, err)
	}
	defer response.Body.Close()
	result := Result{
		StatusCode:        response.StatusCode,
		ContentType:       response.Header.Get("Content-Type"),
		UpstreamRequestID: firstHeader(response.Header, "X-Request-Id", "Openai-Request-Id"),
	}
	opened.apply(&result)
	notifyUpstreamAccount(options, result.UpstreamAccountID)
	notifyConversation(options, result.ConversationHash)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := opened.failure(sanitizeUpstreamFailure(response), 0)
		if retryAfter := parseRetryAfter(response.Header.Get("Retry-After")); retryAfter > 0 {
			failure.RetryAfter = retryAfter
		}
		result.CompletedAt = time.Now()
		return result, failure
	}
	mediaType, _, mediaErr := mime.ParseMediaType(result.ContentType)
	wantMediaType := "application/json"
	if stream {
		wantMediaType = "text/event-stream"
	}
	if response.StatusCode != http.StatusOK || mediaErr != nil || mediaType != wantMediaType {
		result.CompletedAt = time.Now()
		return result, geminiProtocolFailure(errors.New("unexpected Gemini response status or content type"))
	}
	return c.forwardNativeGemini(ctx, w, response, model, declaredTools, stream, result)
}

func geminiDeclaredTools(body []byte) (map[string]struct{}, error) {
	// Admission owns full request validation. This small projection verifies
	// returned calls against the original declarations.
	var request struct {
		Tools []struct {
			Declarations []struct {
				Name string `json:"name"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	tools := make(map[string]struct{})
	for _, tool := range request.Tools {
		for _, declaration := range tool.Declarations {
			tools[declaration.Name] = struct{}{}
		}
	}
	return tools, nil
}

func geminiProtocolFailure(err error) *Failure {
	return &Failure{Status: http.StatusBadGateway, Type: "upstream_error", Code: "upstream_protocol_error", Message: "Antigravity 输出协议或进程异常", Cause: fmt.Errorf("validate Gemini response: %w", err)}
}
