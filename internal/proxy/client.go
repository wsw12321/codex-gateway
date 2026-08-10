// Package proxy implements the narrow Codex Responses data plane. It is not a
// general reverse proxy: callers choose one of three fixed upstream paths and
// only an explicit header allowlist can cross the trust boundary.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const maxSSEEventBytes = 4 << 20

var allowedRequestHeaders = map[string]struct{}{
	"Accept":                      {},
	"Content-Type":                {},
	"Openai-Beta":                 {},
	"Originator":                  {},
	"Traceparent":                 {},
	"Tracestate":                  {},
	"User-Agent":                  {},
	"X-Codex-Beta-Features":       {},
	"X-Codex-Turn-Metadata":       {},
	"X-Stainless-Arch":            {},
	"X-Stainless-Lang":            {},
	"X-Stainless-Os":              {},
	"X-Stainless-Package-Version": {},
	"X-Stainless-Retry-Count":     {},
	"X-Stainless-Runtime":         {},
	"X-Stainless-Runtime-Version": {},
}

type Usage struct {
	InputTokens     int64
	CachedTokens    int64
	OutputTokens    int64
	ReasoningTokens int64
}

func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

type Result struct {
	StatusCode        int
	ContentType       string
	Model             string
	UpstreamRequestID string
	Usage             Usage
	BytesOut          int64
	FirstByteAt       time.Time
	FirstTokenAt      time.Time
	CompletedAt       time.Time
}

type Failure struct {
	Status     int
	Type       string
	Code       string
	Message    string
	RetryAfter int
	Cause      error
}

func (f *Failure) Error() string {
	if f.Cause != nil {
		return f.Code + ": " + f.Cause.Error()
	}
	return f.Code
}

func (f *Failure) Unwrap() error { return f.Cause }

type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

func New(baseURL *url.URL, token string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 90 * time.Second
	transport.ExpectContinueTimeout = time.Second
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 16
	return NewWithHTTPClient(baseURL, token, &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("upstream redirects are disabled")
		},
	})
}

func NewWithHTTPClient(baseURL *url.URL, token string, client *http.Client) *Client {
	copyURL := *baseURL
	copyURL.Path = strings.TrimRight(copyURL.Path, "/")
	return &Client{baseURL: &copyURL, token: token, http: client}
}

func (c *Client) Forward(ctx context.Context, w http.ResponseWriter, incoming *http.Request, upstreamPath string) (Result, *Failure) {
	if !allowedPath(incoming.Method, upstreamPath) {
		return Result{}, &Failure{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "unsupported_endpoint", Message: "不支持的接口"}
	}
	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + upstreamPath
	target.RawQuery = ""
	target.Fragment = ""

	outgoing, err := http.NewRequestWithContext(ctx, incoming.Method, target.String(), incoming.Body)
	if err != nil {
		return Result{}, protocolFailure(err)
	}
	outgoing.ContentLength = incoming.ContentLength
	copyAllowedHeaders(outgoing.Header, incoming.Header)
	outgoing.Header.Set("Authorization", "Bearer "+c.token)
	outgoing.Header.Set("Cache-Control", "no-store")

	response, err := c.http.Do(outgoing)
	if err != nil {
		return Result{}, transportFailure(ctx, err)
	}
	defer response.Body.Close()

	result := Result{
		StatusCode:        response.StatusCode,
		ContentType:       response.Header.Get("Content-Type"),
		UpstreamRequestID: firstHeader(response.Header, "X-Request-Id", "Openai-Request-Id"),
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := sanitizeUpstreamFailure(response)
		result.CompletedAt = time.Now()
		return result, failure
	}

	w.Header().Set("Content-Type", safeContentType(result.ContentType))
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, _ := mime.ParseMediaType(result.ContentType)
	if mediaType == "text/event-stream" {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(response.StatusCode)
	timed := &timedWriter{writer: w}

	if mediaType == "text/event-stream" {
		result.Model, result.Usage, result.FirstTokenAt, err = streamSSE(timed, response.Body)
	} else {
		result.Model, result.Usage, err = streamJSON(timed, response.Body)
	}
	result.BytesOut = timed.bytes.Load()
	result.FirstByteAt = timed.firstByte
	if mediaType != "text/event-stream" {
		result.FirstTokenAt = result.FirstByteAt
	}
	result.CompletedAt = time.Now()
	if err != nil {
		// The response has already begun. Returning the error lets the caller
		// record it, but it must not attempt to append a JSON error body.
		return result, &Failure{Status: 0, Type: "upstream_error", Code: "upstream_stream_error", Message: "上游响应流意外中断", Cause: err}
	}
	return result, nil
}

func allowedPath(method, path string) bool {
	if method == http.MethodGet && path == "/v1/models" {
		return true
	}
	if method == http.MethodPost && (path == "/v1/responses" || path == "/v1/responses/compact") {
		return true
	}
	return false
}

func copyAllowedHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, ok := allowedRequestHeaders[canonical]; !ok {
			continue
		}
		for _, value := range values {
			if !strings.ContainsAny(value, "\r\n") {
				dst.Add(canonical, value)
			}
		}
	}
}

func firstHeader(header http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(header.Get(name)); value != "" && len(value) <= 256 {
			return value
		}
	}
	return ""
}

func safeContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "application/json; charset=utf-8"
	}
	if mediaType == "text/event-stream" {
		return "text/event-stream; charset=utf-8"
	}
	return "application/json; charset=utf-8"
}

type timedWriter struct {
	writer    http.ResponseWriter
	bytes     atomic.Int64
	firstByte time.Time
}

func (w *timedWriter) Write(p []byte) (int, error) {
	if w.firstByte.IsZero() && len(p) > 0 {
		w.firstByte = time.Now()
	}
	n, err := w.writer.Write(p)
	w.bytes.Add(int64(n))
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

type usageFields struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	InputDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type responseEnvelope struct {
	Type     string      `json:"type"`
	ID       string      `json:"id"`
	Model    string      `json:"model"`
	Usage    usageFields `json:"usage"`
	Response *struct {
		ID    string      `json:"id"`
		Model string      `json:"model"`
		Usage usageFields `json:"usage"`
	} `json:"response"`
}

func streamJSON(dst io.Writer, src io.Reader) (string, Usage, error) {
	envelope := responseEnvelope{}
	decoder := json.NewDecoder(io.TeeReader(src, dst))
	if err := decoder.Decode(&envelope); err != nil {
		return "", Usage{}, err
	}
	// Decode may stop immediately after the JSON value. Forward any bytes the
	// decoder has not read yet (typically trailing whitespace) without ever
	// buffering the response body.
	if _, err := io.Copy(dst, src); err != nil {
		return envelope.Model, convertUsage(envelope.Usage), err
	}
	if envelope.Response != nil {
		if envelope.Model == "" {
			envelope.Model = envelope.Response.Model
		}
		if envelope.Usage.InputTokens == 0 && envelope.Usage.OutputTokens == 0 {
			envelope.Usage = envelope.Response.Usage
		}
	}
	return envelope.Model, convertUsage(envelope.Usage), nil
}

func streamSSE(dst io.Writer, src io.Reader) (string, Usage, time.Time, error) {
	reader := bufio.NewReaderSize(src, 64<<10)
	var event bytes.Buffer
	var model string
	var usage Usage
	var firstToken time.Time
	discardEvent := false
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := dst.Write(line); writeErr != nil {
				return model, usage, firstToken, writeErr
			}
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(trimmed) == 0 {
				if !discardEvent {
					if parsedModel, parsedUsage, tokenDelta, ok := parseSSEData(event.Bytes()); ok {
						if parsedModel != "" {
							model = parsedModel
						}
						if parsedUsage.InputTokens != 0 || parsedUsage.OutputTokens != 0 {
							usage = parsedUsage
						}
						if tokenDelta && firstToken.IsZero() {
							firstToken = time.Now()
						}
					}
				}
				event.Reset()
				discardEvent = false
			} else if bytes.HasPrefix(trimmed, []byte("data:")) && !discardEvent {
				data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				if event.Len()+len(data)+1 > maxSSEEventBytes {
					event.Reset()
					discardEvent = true
				} else {
					if event.Len() > 0 {
						event.WriteByte('\n')
					}
					event.Write(data)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if event.Len() > 0 && !discardEvent {
					if parsedModel, parsedUsage, tokenDelta, ok := parseSSEData(event.Bytes()); ok {
						if parsedModel != "" {
							model = parsedModel
						}
						if parsedUsage.InputTokens != 0 || parsedUsage.OutputTokens != 0 {
							usage = parsedUsage
						}
						if tokenDelta && firstToken.IsZero() {
							firstToken = time.Now()
						}
					}
				}
				return model, usage, firstToken, nil
			}
			return model, usage, firstToken, err
		}
	}
}

func parseSSEData(data []byte) (string, Usage, bool, bool) {
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return "", Usage{}, false, false
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", Usage{}, false, false
	}
	tokenDelta := strings.HasSuffix(envelope.Type, ".delta") &&
		(strings.HasPrefix(envelope.Type, "response.output_") || strings.HasPrefix(envelope.Type, "response.reasoning_"))
	if envelope.Response != nil {
		return envelope.Response.Model, convertUsage(envelope.Response.Usage), tokenDelta, true
	}
	if envelope.Usage.InputTokens != 0 || envelope.Usage.OutputTokens != 0 {
		return envelope.Model, convertUsage(envelope.Usage), tokenDelta, true
	}
	return "", Usage{}, tokenDelta, tokenDelta
}

func convertUsage(value usageFields) Usage {
	return Usage{
		InputTokens: value.InputTokens, CachedTokens: value.InputDetails.CachedTokens,
		OutputTokens: value.OutputTokens, ReasoningTokens: value.OutputDetails.ReasoningTokens,
	}
}

func sanitizeUpstreamFailure(response *http.Response) *Failure {
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return &Failure{Status: http.StatusServiceUnavailable, Type: "upstream_error", Code: "upstream_reauthentication_required", Message: "上游 Codex 登录已失效，需要管理员重新认证", RetryAfter: retryAfter}
	case http.StatusTooManyRequests:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return &Failure{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "upstream_rate_limited", Message: "上游当前已达到使用限制", RetryAfter: max(retryAfter, 60)}
	}

	var value struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err == nil {
		_ = json.Unmarshal(body, &value)
	}
	message := safeUpstreamMessage(value.Error.Message)
	if message == "" {
		message = "上游拒绝了请求"
	}
	code := safeIdentifier(value.Error.Code)
	if code == "" {
		code = "upstream_request_failed"
	}
	typ := safeIdentifier(value.Error.Type)
	if typ == "" {
		typ = "upstream_error"
	}
	status := response.StatusCode
	if status < 400 || status > 499 {
		status = http.StatusBadGateway
	}
	return &Failure{Status: status, Type: typ, Code: code, Message: message, RetryAfter: retryAfter}
}

func safeIdentifier(value string) string {
	if value == "" || len(value) > 80 {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return ""
		}
	}
	return value
}

func safeUpstreamMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 512 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	lower := strings.ToLower(value)
	for _, sensitive := range []string{"authorization", "bearer ", "cookie", "access_token", "refresh_token", "api key", "api_key"} {
		if strings.Contains(lower, sensitive) {
			return ""
		}
	}
	return value
}

func parseRetryAfter(value string) int {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err == nil && seconds > 0 && seconds <= 86400 {
		return seconds
	}
	return 0
}

func transportFailure(ctx context.Context, err error) *Failure {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return &Failure{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "request_too_large", Message: "请求体过大", Cause: err}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return &Failure{Status: 0, Type: "request_error", Code: "client_disconnected", Message: "客户端已断开连接", Cause: err}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeout(err) {
		return &Failure{Status: http.StatusGatewayTimeout, Type: "upstream_error", Code: "upstream_timeout", Message: "连接上游超时", Cause: err}
	}
	return protocolFailure(err)
}

func protocolFailure(err error) *Failure {
	return &Failure{Status: http.StatusBadGateway, Type: "upstream_error", Code: "upstream_unavailable", Message: "暂时无法连接上游服务", Cause: err}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (u Usage) String() string {
	return fmt.Sprintf("input=%d cached=%d output=%d reasoning=%d", u.InputTokens, u.CachedTokens, u.OutputTokens, u.ReasoningTokens)
}
