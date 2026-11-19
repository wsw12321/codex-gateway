package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wsw/codex-gateway/internal/cpaprotocol"
)

func ForwardSupplier(ctx context.Context, w http.ResponseWriter, outgoing *http.Request, client *http.Client, provider, path, model, account, conversation string) (Result, *Failure) {
	parser := &Client{}
	var tools map[string]struct{}
	if provider == "antigravity" {
		data, err := io.ReadAll(io.LimitReader(outgoing.Body, maxInternalResponseBodyBytes+1))
		if err != nil || len(data) > maxInternalResponseBodyBytes {
			return Result{}, &Failure{Status: 400, Code: "invalid_gemini_request", Type: "invalid_request_error", Message: "Gemini 请求体无效"}
		}
		tools, err = geminiDeclaredTools(data)
		if err != nil {
			return Result{}, &Failure{Status: 400, Code: "invalid_gemini_request", Type: "invalid_request_error", Message: "Gemini 请求工具声明无效"}
		}
		outgoing.Body = io.NopCloser(bytes.NewReader(data))
	}
	result := Result{UpstreamAccountID: account, ConversationHash: conversation}
	var connected atomic.Bool
	outgoing = outgoing.WithContext(httptrace.WithClientTrace(outgoing.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}))
	response, err := client.Do(outgoing)
	if err != nil {
		failure := parser.transportFailure(ctx, errors.New("supplier connection unavailable"))
		if ctx.Err() == nil && !connected.Load() {
			failure.Code = "supplier_egress_unavailable"
		}
		return result, failure
	}
	defer response.Body.Close()
	result.StatusCode = response.StatusCode
	result.ContentType = response.Header.Get("Content-Type")
	result.UpstreamRequestID = firstHeader(response.Header, "Request-Id", "X-Request-Id", "Openai-Request-Id")
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.CompletedAt = time.Now()
		return result, sanitizeSupplierFailure(response)
	}
	media, _, err := mime.ParseMediaType(result.ContentType)
	if response.StatusCode != http.StatusOK || err != nil || (media != "application/json" && media != "text/event-stream") {
		return result, protocolFailure(errors.New("invalid supplier response content type or status"))
	}
	switch provider {
	case "anthropic":
		result.Model = model
		return parser.forwardAnthropicResponse(ctx, w, response, path == "/v1/messages/count_tokens", result)
	case "antigravity":
		stream := strings.HasSuffix(path, ":streamGenerateContent")
		if stream != (media == "text/event-stream") {
			return result, protocolFailure(errors.New("unexpected Gemini response format"))
		}
		return parser.forwardNativeGemini(ctx, w, response, model, tools, stream, result)
	default:
		return forwardSupplierResponses(ctx, w, response, media, result)
	}
}

func sanitizeSupplierFailure(response *http.Response) *Failure {
	status := response.StatusCode
	if status == 429 {
		return &Failure{Status: status, Code: "supplier_rate_limited", Type: "rate_limit_error", Message: "供应商暂时限流", RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"))}
	}
	if status == 400 || status == 404 || status == 413 || status == 422 {
		return &Failure{Status: status, Code: "supplier_request_rejected", Type: "invalid_request_error", Message: "供应商拒绝请求，请检查模型和接口支持"}
	}
	code := "supplier_unavailable"
	if status == 401 || status == 403 {
		code = "supplier_credentials_rejected"
	}
	return &Failure{Status: 502, Code: code, Type: "upstream_error", Message: "供应商请求未成功"}
}

func supplierResponsesUsage(data []byte) (string, string, Usage, error) {
	fields, err := cpaprotocol.Object(data)
	if err != nil || fields["error"] != nil && !bytes.Equal(fields["error"], []byte("null")) {
		return "", "", Usage{}, errors.New("invalid supplier Responses object")
	}
	raw := fields["usage"]
	counts, err := cpaprotocol.Object(raw)
	if err != nil || counts["input_tokens"] == nil || counts["output_tokens"] == nil || nativeUsageCounts(raw, "input_tokens", "output_tokens", "total_tokens") != nil {
		return "", "", Usage{}, errors.New("missing or invalid supplier Responses usage")
	}
	var envelope responseEnvelope
	if json.Unmarshal(data, &envelope) != nil {
		return "", "", Usage{}, errors.New("invalid supplier Responses usage fields")
	}
	usage := convertUsage(envelope.Usage)
	if usage.CachedTokens < 0 || usage.CachedTokens > usage.InputTokens || usage.CacheWriteTokens < 0 || usage.ReasoningTokens < 0 || usage.ReasoningTokens > usage.OutputTokens {
		return "", "", Usage{}, errors.New("inconsistent supplier Responses usage")
	}
	return envelope.Model, envelope.ServiceTier, usage, nil
}

func forwardSupplierResponses(ctx context.Context, w http.ResponseWriter, response *http.Response, media string, result Result) (Result, *Failure) {
	writer := &timedWriter{writer: w}
	var err error
	if media == "application/json" {
		const limit = 16 << 20
		var data []byte
		data, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || len(data) > limit {
			return result, protocolFailure(errors.New("invalid supplier Responses JSON"))
		}
		result.Model, result.ServiceTier, result.Usage, err = supplierResponsesUsage(data)
		if err != nil {
			return result, protocolFailure(errors.New("invalid supplier Responses usage"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, err = writer.Write(data)
		result.FirstTokenAt = writer.firstByte
	} else {
		terminal := false
		err = readAnthropicEvents(response.Body, func(frame, data []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(data) != 0 && !bytes.Equal(data, []byte("[DONE]")) {
				fields, err := cpaprotocol.Object(data)
				var kind string
				if err != nil || json.Unmarshal(fields["type"], &kind) != nil || kind == "error" || kind == "response.failed" || terminal {
					return errors.New("invalid supplier Responses event")
				}
				if kind == "response.completed" || kind == "response.incomplete" {
					result.Model, result.ServiceTier, result.Usage, err = supplierResponsesUsage(fields["response"])
					if err != nil {
						return err
					}
					terminal = true
				}
				if result.FirstTokenAt.IsZero() && strings.HasSuffix(kind, ".delta") && (strings.HasPrefix(kind, "response.output_") || strings.HasPrefix(kind, "response.reasoning_")) {
					result.FirstTokenAt = time.Now()
				}
			}
			if writer.firstByte.IsZero() {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Accel-Buffering", "no")
			}
			if _, err := writer.Write(frame); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		})
		if err == nil && !terminal {
			err = errors.New("missing supplier Responses stream usage")
		}
	}
	result.BytesOut, result.FirstByteAt, result.CompletedAt = writer.bytes.Load(), writer.firstByte, time.Now()
	if err != nil {
		if result.BytesOut > 0 {
			result.AbortStream = true
			return result, &Failure{Code: "upstream_stream_error", Type: "upstream_error", Message: "供应商响应流意外中断"}
		}
		return result, protocolFailure(err)
	}
	return result, nil
}
