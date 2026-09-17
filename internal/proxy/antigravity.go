package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewAntigravity creates an internal bridge client. The CLI may produce its
// final result only after five minutes, so the usual Codex header timeout is
// too short. Internal bridge traffic must not traverse an environment proxy.
func NewAntigravity(baseURL *url.URL, token string) *Client {
	client := New(baseURL, token)
	client.antigravity = true
	transport := client.http.Transport.(*http.Transport)
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 6 * time.Minute
	return client
}

func (c *Client) transportFailure(ctx context.Context, err error) *Failure {
	failure := transportFailure(ctx, err)
	if c.antigravity && failure.Code == "upstream_unavailable" {
		failure.Status = http.StatusServiceUnavailable
	}
	return failure
}

func (c *Client) sanitizeFailure(response *http.Response) *Failure {
	if !c.antigravity {
		return sanitizeUpstreamFailure(response)
	}
	// Never expose the CLI's stderr, provider messages, or credentials through
	// error bodies. The bridge's bounded machine codes select fixed messages.
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, maxInternalResponseBodyBytes))
	_ = json.Unmarshal(data, &body)
	code := safeIdentifier(body.Error.Code)
	switch response.StatusCode {
	case http.StatusBadRequest:
		if code == "antigravity_invalid_request" {
			return &Failure{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: code, Message: "Antigravity 请求格式无效"}
		}
		if strings.HasPrefix(code, "antigravity_") && strings.HasSuffix(code, "_unsupported") {
			return &Failure{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: code, Message: "Antigravity 不支持此请求参数或输入类型"}
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_reauthentication_required", Message: "Antigravity 登录已失效，需要管理员重新认证"}
	case http.StatusRequestEntityTooLarge:
		return &Failure{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "antigravity_request_too_large", Message: "Antigravity 请求体超过 1 MiB 限制"}
	case http.StatusTooManyRequests:
		if code == "upstream_concurrency_exceeded" {
			return &Failure{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: code, Message: "Antigravity 正在处理其他请求，请稍后重试", RetryAfter: max(retryAfter, 1)}
		}
		return &Failure{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "upstream_rate_limited", Message: "Antigravity 订阅已达到使用限制", RetryAfter: max(retryAfter, 60)}
	case http.StatusServiceUnavailable:
		if code == "upstream_reauthentication_required" {
			return &Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: code, Message: "Antigravity 登录已失效，需要管理员重新认证", RetryAfter: retryAfter}
		}
		return &Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_unavailable", Message: "Antigravity 尚未就绪", RetryAfter: retryAfter}
	case http.StatusGatewayTimeout:
		return &Failure{Status: http.StatusGatewayTimeout, Type: "upstream_error", Code: "upstream_timeout", Message: "Antigravity 请求超时"}
	case http.StatusBadGateway:
		if code == "upstream_process_error" {
			return &Failure{Status: http.StatusBadGateway, Type: "upstream_error", Code: code, Message: "Antigravity 进程异常"}
		}
		return &Failure{Status: http.StatusBadGateway, Type: "upstream_error", Code: "upstream_protocol_error", Message: "Antigravity 输出协议或进程异常"}
	}
	return &Failure{Status: http.StatusBadGateway, Type: "upstream_error", Code: "upstream_protocol_error", Message: "Antigravity 返回了无效错误响应"}
}
