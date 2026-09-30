package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Bound both the legacy complete response and the negotiated incremental
// stream, allowing room for JSON escaping and protocol metadata. Final usage
// and tools are released only after terminal validation in either protocol.
const maxGeminiResponseBytes = 16 << 20

// ForwardGemini only supports configured bridge models. Native Gemini requests
// must never reach the primary Codex upstream, including during a bridge outage.
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
		if err := c.requireAccountAccessCapability(ctx); err != nil {
			return Result{}, &Failure{Status: http.StatusServiceUnavailable, Type: "server_error", Code: "upstream_access_protocol_unavailable", Message: "上游账号权限服务不可用", Cause: err}
		}
		outgoing.Header.Set(gatewayUserHeader, options.UserID)
	}
	if stream {
		outgoing.Header.Set("Accept", "text/event-stream")
		if !c.cpaNative {
			outgoing.Header.Set(geminiStreamHeader, "v1")
		}
	} else {
		outgoing.Header.Set("Accept", "application/json")
	}
	response, err := c.http.Do(outgoing)
	if err != nil {
		return Result{}, c.transportFailure(ctx, err)
	}
	defer response.Body.Close()
	result := Result{
		StatusCode:        response.StatusCode,
		ContentType:       response.Header.Get("Content-Type"),
		UpstreamAccountID: safeUpstreamAccountHeader(response.Header),
		ConversationHash:  safeConversationHashHeader(response.Header),
		UpstreamRequestID: firstHeader(response.Header, "X-Request-Id", "Openai-Request-Id"),
	}
	notifyUpstreamAccount(options, result.UpstreamAccountID)
	notifyConversation(options, result.ConversationHash)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := c.sanitizeFailure(response)
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
	if c.cpaNative {
		return c.forwardNativeGemini(ctx, w, response, model, declaredTools, stream, result)
	}
	if values := response.Header.Values(geminiStreamHeader); len(values) != 0 {
		if !stream || len(values) != 1 || values[0] != "v1" {
			return result, geminiProtocolFailure(errors.New("unsupported Gemini stream protocol"))
		}
		return c.forwardGeminiStream(ctx, w, response, model, declaredTools, result)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGeminiResponseBytes+1))
	result.CompletedAt = time.Now()
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return result, c.transportFailure(ctx, err)
		}
		return result, geminiProtocolFailure(err)
	}
	if len(data) > maxGeminiResponseBytes {
		return result, geminiProtocolFailure(errors.New("Gemini response exceeds limit"))
	}
	jsonData := data
	if stream {
		jsonData, err = geminiSSEObject(data)
		if err != nil {
			return result, geminiProtocolFailure(err)
		}
	}
	usage, err := validateGeminiResponse(jsonData, model, declaredTools)
	if err != nil {
		return result, geminiProtocolFailure(err)
	}
	if ctx.Err() != nil {
		return result, c.transportFailure(ctx, ctx.Err())
	}
	result.Model, result.Usage = model, usage
	// The bridge serves only Standard. Native Gemini has no Responses-style
	// service_tier field, so record the known tier instead of a missing-tier fallback.
	result.ServiceTier = "default"
	w.Header().Set("Content-Type", wantMediaType+"; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if stream {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(http.StatusOK)
	timed := &timedWriter{writer: w}
	_, err = timed.Write(data)
	result.BytesOut, result.FirstByteAt = timed.bytes.Load(), timed.firstByte
	result.FirstTokenAt, result.CompletedAt = result.FirstByteAt, time.Now()
	if err != nil {
		return result, &Failure{Status: 0, Type: "upstream_error", Code: "upstream_stream_error", Message: "上游响应流意外中断", Cause: err}
	}
	return result, nil
}

func geminiDeclaredTools(body []byte) (map[string]struct{}, error) {
	// Admission owns full request validation. This small projection verifies
	// returned calls against the original declarations without trusting the CLI.
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

// geminiSSEObject accepts exactly the bridge's completed data event, including
// fragmented/multiline data and CRLF. It requires the terminating blank line:
// EOF without dispatch is a partial event, and [DONE] is not Gemini JSON.
func geminiSSEObject(body []byte) ([]byte, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("invalid Gemini SSE UTF-8")
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), maxGeminiResponseBytes)
	var data bytes.Buffer
	complete, pending := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if pending {
				if complete {
					return nil, errors.New("multiple Gemini completion events")
				}
				complete, pending = true, false
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if complete || !strings.HasPrefix(line, "data:") {
			return nil, errors.New("unexpected Gemini SSE field or trailing event")
		}
		value := strings.TrimPrefix(line, "data:")
		value = strings.TrimPrefix(value, " ")
		if pending {
			data.WriteByte('\n')
		}
		data.WriteString(value)
		pending = true
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !complete || pending {
		return nil, errors.New("incomplete Gemini SSE event")
	}
	return data.Bytes(), nil
}

func validateGeminiResponse(body []byte, model string, tools map[string]struct{}) (Usage, error) {
	if !utf8.Valid(body) {
		return Usage{}, errors.New("invalid Gemini response UTF-8")
	}
	if err := validateModelCatalogJSON(body); err != nil {
		return Usage{}, err
	}
	var response struct {
		ModelVersion string `json:"modelVersion"`
		ResponseID   string `json:"responseId"`
		Candidates   []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text         *string `json:"text,omitempty"`
					FunctionCall *struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
						ID   string          `json:"id,omitempty"`
					} `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
			Index        int    `json:"index"`
		} `json:"candidates"`
		UsageMetadata *struct {
			Prompt     *int64 `json:"promptTokenCount"`
			Candidates *int64 `json:"candidatesTokenCount"`
			Thoughts   *int64 `json:"thoughtsTokenCount"`
			Cached     *int64 `json:"cachedContentTokenCount"`
			Total      *int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return Usage{}, err
	}
	if response.ModelVersion != model || len(response.Candidates) != 1 {
		return Usage{}, errors.New("unexpected Gemini response model or candidates")
	}
	candidate := response.Candidates[0]
	if candidate.Content.Role != "model" || len(candidate.Content.Parts) == 0 || candidate.Index != 0 ||
		(candidate.FinishReason != "STOP" && candidate.FinishReason != "MAX_TOKENS") {
		return Usage{}, errors.New("incomplete Gemini candidate")
	}
	callIDs := make(map[string]struct{})
	for _, part := range candidate.Content.Parts {
		if (part.Text == nil) == (part.FunctionCall == nil) {
			return Usage{}, errors.New("invalid Gemini response part")
		}
		if part.Text != nil {
			if *part.Text == "" {
				return Usage{}, errors.New("empty Gemini response text")
			}
			continue
		}
		call := part.FunctionCall
		if _, declared := tools[call.Name]; !declared || call.Name == "" {
			return Usage{}, errors.New("Gemini returned an undeclared tool")
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal(call.Args, &args); err != nil || args == nil {
			return Usage{}, errors.New("Gemini tool arguments are not an object")
		}
		if call.ID != "" {
			if strings.IndexFunc(call.ID, unicode.IsControl) >= 0 || strings.TrimSpace(call.ID) != call.ID {
				return Usage{}, errors.New("invalid Gemini tool call ID")
			}
			if _, duplicate := callIDs[call.ID]; duplicate {
				return Usage{}, errors.New("duplicate Gemini tool call ID")
			}
			callIDs[call.ID] = struct{}{}
		}
	}
	u := response.UsageMetadata
	if u == nil || u.Prompt == nil || u.Candidates == nil || u.Thoughts == nil || u.Cached == nil || u.Total == nil ||
		*u.Prompt < 0 || *u.Candidates < 0 || *u.Thoughts < 0 || *u.Cached < 0 || *u.Cached > *u.Prompt || *u.Candidates > math.MaxInt64-*u.Thoughts {
		return Usage{}, errors.New("invalid Gemini usage")
	}
	output := *u.Candidates + *u.Thoughts
	if *u.Prompt > math.MaxInt64-output || *u.Total != *u.Prompt+output {
		return Usage{}, errors.New("inconsistent or overflowing Gemini usage")
	}
	return Usage{InputTokens: *u.Prompt, CachedTokens: *u.Cached, OutputTokens: output, ReasoningTokens: *u.Thoughts}, nil
}

func geminiProtocolFailure(err error) *Failure {
	return &Failure{Status: http.StatusBadGateway, Type: "upstream_error", Code: "upstream_protocol_error", Message: "Antigravity 输出协议或进程异常", Cause: fmt.Errorf("validate Gemini response: %w", err)}
}
