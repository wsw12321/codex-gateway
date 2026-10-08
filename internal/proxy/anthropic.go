package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/cpaprotocol"
)

// NewCPAAnthropic shares CPA's internal authentication and network boundary.
// Native Messages routes select the Claude executor inside CPA; caller-supplied
// provider headers and API keys never cross this boundary.
func NewCPAAnthropic(baseURL *url.URL, token string) *Client {
	c := New(baseURL, token)
	c.anthropic = true
	return c
}

func ValidAnthropicQuery(raw string) bool {
	if raw == "" {
		return true
	}
	q, err := url.ParseQuery(raw)
	return err == nil && len(q) == 1 && len(q["beta"]) == 1 && q.Get("beta") == "true"
}

func (c *Client) ForwardMessages(ctx context.Context, w http.ResponseWriter, incoming *http.Request, model, path string, options ForwardOptions) (Result, *Failure) {
	if c == nil || !c.anthropic || incoming.Method != http.MethodPost || (path != "/v1/messages" && path != "/v1/messages/count_tokens") || !ValidAnthropicQuery(incoming.URL.RawQuery) {
		return Result{}, protocolFailure(errors.New("invalid Anthropic route"))
	}
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawPath, target.RawQuery, target.Fragment = "", incoming.URL.RawQuery, ""
	outgoing, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), incoming.Body)
	if err != nil {
		return Result{}, protocolFailure(err)
	}
	outgoing.ContentLength = incoming.ContentLength
	c.copyAllowedHeaders(outgoing.Header, incoming.Header)
	for name, values := range incoming.Header {
		if strings.EqualFold(name, "Anthropic-Version") || strings.EqualFold(name, "Anthropic-Beta") {
			for _, value := range values {
				outgoing.Header.Add(name, value)
			}
		}
	}
	outgoing.Header.Set("Authorization", "Bearer "+c.token)
	outgoing.Header.Set("Cache-Control", "no-store")
	if options.AffinityScope != "" {
		if !affinityScopePattern.MatchString(options.AffinityScope) {
			return Result{}, protocolFailure(errors.New("invalid affinity"))
		}
		outgoing.Header.Set(affinityHeader, options.AffinityScope)
	}
	if options.UserID != "" {
		if !gatewayUserPattern.MatchString(options.UserID) {
			return Result{}, protocolFailure(errors.New("invalid user"))
		}
		if err := c.requireAccountAccessCapability(ctx); err != nil {
			return Result{}, &Failure{Status: 503, Type: "api_error", Code: "upstream_access_protocol_unavailable", Message: "Claude 账号权限服务不可用", Cause: err}
		}
		outgoing.Header.Set(gatewayUserHeader, options.UserID)
	}
	response, err := c.http.Do(outgoing)
	if err != nil {
		return Result{}, c.transportFailure(ctx, err)
	}
	defer response.Body.Close()
	result := Result{StatusCode: response.StatusCode, ContentType: response.Header.Get("Content-Type"),
		Model: model, UpstreamAccountID: safeUpstreamAccountHeader(response.Header), ConversationHash: safeConversationHashHeader(response.Header),
		UpstreamRequestID: firstHeader(response.Header, "Request-Id", "X-Request-Id")}
	notifyUpstreamAccount(options, result.UpstreamAccountID)
	notifyConversation(options, result.ConversationHash)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Native error wording drives Claude Code's capability recovery. Only a
		// validated error envelope from the fixed CPA service is forwarded.
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxInternalResponseBodyBytes+1))
		var envelope struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if readErr != nil || len(data) > maxInternalResponseBodyBytes || json.Unmarshal(data, &envelope) != nil || envelope.Type != "error" || envelope.Error.Type == "" || envelope.Error.Message == "" {
			return result, &Failure{Status: 502, Type: "api_error", Code: "upstream_protocol_error", Message: "Claude 返回无效错误响应"}
		}
		if _, err := cpaprotocol.Object(data); err != nil {
			return result, protocolFailure(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if retry := response.Header.Get("Retry-After"); retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(response.StatusCode)
		result.FirstByteAt = time.Now()
		n, writeErr := w.Write(data)
		result.BytesOut, result.CompletedAt = int64(n), time.Now()
		return result, &Failure{Type: envelope.Error.Type, Code: "anthropic_upstream_error", Message: "Claude 请求失败", Cause: writeErr}
	}
	return c.forwardAnthropicResponse(ctx, w, response, path == "/v1/messages/count_tokens", result)
}

// Anthropic input_tokens excludes both cache categories. Partial SSE usage
// updates are cumulative snapshots, never increments. Missing fields retain
// the last value, while an explicit zero replaces it.
func anthropicUsage(data []byte, prior Usage) (Usage, error) {
	fields, err := cpaprotocol.Object(data)
	if err != nil {
		return Usage{}, err
	}
	u := prior
	plain := u.InputTokens - u.CachedTokens - u.CacheWriteTokens
	for key, target := range map[string]*int64{"input_tokens": &plain, "cache_read_input_tokens": &u.CachedTokens, "cache_creation_input_tokens": &u.CacheWriteTokens, "output_tokens": &u.OutputTokens} {
		if raw, ok := fields[key]; ok {
			var count *int64
			if json.Unmarshal(raw, &count) != nil || count == nil || *count < 0 {
				return Usage{}, errors.New("invalid Anthropic usage count")
			}
			*target = *count
		}
	}
	if _, ok := fields["cache_creation_input_tokens"]; ok {
		u.CacheWriteTokensPresent = true
		// A newer total without TTL detail cannot inherit stale allocation.
		u.CacheWriteTTLPresent = false
		u.CacheWrite5mTokens, u.CacheWrite1hTokens = 0, 0
	}
	if raw, ok := fields["cache_creation"]; ok {
		// Explicit null is a missing breakdown, unlike an omitted field in a
		// partial cumulative update. Never retain stale TTL quantities for it.
		u.CacheWriteTTLPresent = false
		u.CacheWrite5mTokens, u.CacheWrite1hTokens = 0, 0
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			ttl, err := cpaprotocol.Object(raw)
			if err != nil {
				return Usage{}, err
			}
			var short, long *int64
			if err := nativeUsageCounts(raw, "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"); err != nil {
				return Usage{}, err
			}
			_ = json.Unmarshal(ttl["ephemeral_5m_input_tokens"], &short)
			_ = json.Unmarshal(ttl["ephemeral_1h_input_tokens"], &long)
			if short != nil && long != nil {
				if !u.CacheWriteTokensPresent || *short > math.MaxInt64-*long || *short+*long != u.CacheWriteTokens {
					return Usage{}, errors.New("inconsistent Anthropic cache TTL usage")
				}
				u.CacheWrite5mTokens, u.CacheWrite1hTokens, u.CacheWriteTTLPresent = *short, *long, true
			}
		}
	}
	if plain < 0 || plain > math.MaxInt64-u.CachedTokens || plain+u.CachedTokens > math.MaxInt64-u.CacheWriteTokens {
		return Usage{}, errors.New("Anthropic input overflow")
	}
	u.InputTokens = plain + u.CachedTokens + u.CacheWriteTokens
	if u.InputTokens > math.MaxInt64-u.OutputTokens {
		return Usage{}, errors.New("Anthropic total overflow")
	}
	return u, nil
}

func (c *Client) forwardAnthropicResponse(ctx context.Context, w http.ResponseWriter, response *http.Response, countTokens bool, initial Result) (result Result, failure *Failure) {
	result = initial
	defer func() {
		result.CompletedAt = time.Now()
		if failure != nil && !result.FirstByteAt.IsZero() {
			failure.Status = 0
			result.AbortStream = true
		}
	}()
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (media != "application/json" && media != "text/event-stream") || countTokens && media != "application/json" {
		return result, protocolFailure(errors.New("invalid Anthropic media type"))
	}
	write := func(data []byte, token bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if result.FirstByteAt.IsZero() {
			w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
			w.Header().Set("Cache-Control", "no-store")
			if media == "text/event-stream" {
				w.Header().Set("X-Accel-Buffering", "no")
			}
			if result.UpstreamRequestID != "" {
				w.Header().Set("Request-Id", result.UpstreamRequestID)
			}
			w.WriteHeader(response.StatusCode)
			result.FirstByteAt = time.Now()
		}
		n, err := w.Write(data)
		result.BytesOut += int64(n)
		if token && result.FirstTokenAt.IsZero() {
			result.FirstTokenAt = time.Now()
		}
		if err == nil && media == "text/event-stream" {
			err = http.NewResponseController(w).Flush()
		}
		return err
	}
	if media == "application/json" {
		data, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
		if err != nil {
			return result, c.transportFailure(ctx, err)
		}
		if len(data) > 64<<20 {
			return result, protocolFailure(errors.New("Anthropic response too large"))
		}
		fields, err := cpaprotocol.Object(data)
		if err != nil {
			return result, protocolFailure(err)
		}
		if countTokens {
			var count *int64
			if json.Unmarshal(fields["input_tokens"], &count) != nil || count == nil || *count < 0 {
				return result, protocolFailure(errors.New("invalid token count"))
			}
		} else {
			var message struct {
				Type       string  `json:"type"`
				Model      string  `json:"model"`
				StopReason *string `json:"stop_reason"`
			}
			if json.Unmarshal(data, &message) != nil || message.Type != "message" || message.StopReason == nil {
				return result, protocolFailure(errors.New("incomplete Anthropic message"))
			}
			u, err := anthropicUsage(fields["usage"], Usage{})
			if err != nil {
				return result, protocolFailure(err)
			}
			usageFields, _ := cpaprotocol.Object(fields["usage"])
			if usageFields["input_tokens"] == nil || usageFields["output_tokens"] == nil {
				return result, protocolFailure(errors.New("missing Anthropic usage"))
			}
			result.Usage, result.ServiceTier = u, anthropicServiceTier(usageFields)
			if message.Model != "" {
				result.Model = message.Model
			}
		}
		if err := write(data, !countTokens); err != nil {
			return result, c.transportFailure(ctx, err)
		}
		return result, nil
	}
	started, finalUsage, stopped := false, false, false
	var usage Usage
	streamComplete := errors.New("Anthropic message complete")
	err = readAnthropicEvents(response.Body, func(raw, data []byte) error {
		if len(data) == 0 {
			return write(raw, false)
		}
		fields, err := cpaprotocol.Object(data)
		if err != nil {
			return err
		}
		var kind string
		if json.Unmarshal(fields["type"], &kind) != nil {
			return errors.New("invalid Anthropic event type")
		}
		if stopped {
			return errors.New("Anthropic event after message_stop")
		}
		switch kind {
		case "message_start":
			if started {
				return errors.New("duplicate Anthropic message_start")
			}
			message, err := cpaprotocol.Object(fields["message"])
			if err != nil {
				return err
			}
			u, err := anthropicUsage(message["usage"], Usage{})
			if err != nil {
				return err
			}
			uf, _ := cpaprotocol.Object(message["usage"])
			if uf["input_tokens"] == nil || uf["output_tokens"] == nil {
				return errors.New("missing Anthropic initial usage")
			}
			usage, started, result.ServiceTier = u, true, anthropicServiceTier(uf)
			result.Usage = usage
			var model string
			if json.Unmarshal(message["model"], &model) == nil && model != "" {
				result.Model = model
			}
		case "message_delta":
			if !started {
				return errors.New("Anthropic delta before message_start")
			}
			uf, err := cpaprotocol.Object(fields["usage"])
			if err != nil || uf["output_tokens"] == nil {
				return errors.New("missing Anthropic terminal usage")
			}
			u, err := anthropicUsage(fields["usage"], usage)
			if err != nil {
				return err
			}
			if u.OutputTokens < usage.OutputTokens {
				return errors.New("Anthropic usage regressed")
			}
			usage, finalUsage = u, true
			// Preserve the last validated cumulative snapshot even if the peer
			// disconnects before message_stop. Failed/cancelled requests still
			// settle the observed tokens through the ordinary ledger path.
			result.Usage = usage
			if uf["service_tier"] != nil {
				result.ServiceTier = anthropicServiceTier(uf)
			}
		case "message_stop":
			if !started || !finalUsage {
				return errors.New("incomplete Anthropic stream")
			}
			stopped = true
		case "error":
			if err := write(raw, false); err != nil {
				return err
			}
			return errors.New("Anthropic error event")
		}
		if err := write(raw, kind == "content_block_delta" || kind == "content_block_start"); err != nil {
			return err
		}
		if stopped {
			// message_stop ends the protocol. Do not keep the request and its
			// concurrency leases alive waiting for a transport-level EOF.
			return streamComplete
		}
		return nil
	})
	if errors.Is(err, streamComplete) {
		err = nil
	}
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return result, c.transportFailure(ctx, err)
		}
		return result, protocolFailure(err)
	}
	if !stopped {
		return result, protocolFailure(errors.New("Anthropic stream missing message_stop"))
	}
	result.Usage = usage
	return result, nil
}

func anthropicServiceTier(fields map[string]json.RawMessage) string {
	var tier string
	_ = json.Unmarshal(fields["service_tier"], &tier)
	if tier == "" {
		return "standard"
	}
	return tier
}

// Preserve bytes (including event names, comments, ping and unknown events)
// while examining only the minimal data needed for settlement.
func readAnthropicEvents(r io.Reader, emit func(raw, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Split(scanAnthropicLine)
	scanner.Buffer(make([]byte, 4096), maxSSEEventBytes)
	var frame, data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		if frame.Len()+len(line) > maxSSEEventBytes {
			return errors.New("Anthropic event too large")
		}
		frame.Write(line)
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			if err := emit(frame.Bytes(), bytes.TrimSuffix(data.Bytes(), []byte("\n"))); err != nil {
				return err
			}
			frame.Reset()
			data.Reset()
		} else if bytes.HasPrefix(trimmed, []byte("data:")) {
			value := bytes.TrimPrefix(trimmed, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			data.Write(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if frame.Len() != 0 {
		return errors.New("truncated Anthropic event")
	}
	return nil
}

func scanAnthropicLine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
