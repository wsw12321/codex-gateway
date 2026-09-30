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

	"github.com/wsw/codex-gateway/internal/cpaprotocol"
)

func nativeResponsesUsage(data []byte) (Usage, error) {
	if err := nativeUsageCounts(data, "input_tokens", "output_tokens", "total_tokens"); err != nil {
		return Usage{}, err
	}
	fields, _ := cpaprotocol.Object(data)
	for key, field := range map[string]string{"input_tokens_details": "cached_tokens", "output_tokens_details": "reasoning_tokens"} {
		if raw, ok := fields[key]; ok && !bytes.Equal(raw, []byte("null")) {
			if err := nativeUsageCounts(raw, field); err != nil {
				return Usage{}, err
			}
		}
	}
	var u struct {
		Input        *int64 `json:"input_tokens"`
		Output       *int64 `json:"output_tokens"`
		Total        *int64 `json:"total_tokens"`
		InputDetails struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if json.Unmarshal(data, &u) != nil || u.Input == nil || u.Output == nil || *u.Input < 0 || *u.Output < 0 || u.InputDetails.Cached < 0 || u.InputDetails.Cached > *u.Input || u.OutputDetails.Reasoning < 0 || u.OutputDetails.Reasoning > *u.Output || *u.Input > math.MaxInt64-*u.Output || *u.Input+*u.Output == 0 || u.Total != nil && *u.Total != *u.Input+*u.Output {
		return Usage{}, errors.New("invalid CPA Responses usage")
	}
	// Responses output_tokens already includes reasoning_tokens.
	return Usage{InputTokens: *u.Input, CachedTokens: u.InputDetails.Cached, OutputTokens: *u.Output, ReasoningTokens: u.OutputDetails.Reasoning}, nil
}

func validateNativeResponse(data []byte, model string, tools map[string]struct{}) (Usage, error) {
	o, err := cpaprotocol.Object(data)
	if err != nil {
		return Usage{}, err
	}
	var r struct {
		Model  string            `json:"model"`
		Status string            `json:"status"`
		Tier   string            `json:"service_tier"`
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(data, &r) != nil || r.Status != "completed" || r.Model != "" && r.Model != model || r.Tier != "" && r.Tier != "default" && r.Tier != "auto" {
		return Usage{}, errors.New("invalid CPA Responses completion")
	}
	if raw := o["error"]; raw != nil && !bytes.Equal(raw, []byte("null")) {
		return Usage{}, errors.New("CPA Responses error in completion")
	}
	for _, item := range r.Output {
		if err := validateNativeResponseItem(item, tools); err != nil {
			return Usage{}, err
		}
	}
	return nativeResponsesUsage(o["usage"])
}

func validateNativeResponseItem(data []byte, tools map[string]struct{}) error {
	var item struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if json.Unmarshal(data, &item) != nil {
		return errors.New("invalid CPA output item")
	}
	switch item.Type {
	case "message":
		for _, part := range item.Content {
			if part.Type != "output_text" && part.Type != "refusal" {
				return errors.New("unsupported CPA output part")
			}
		}
	case "reasoning":
	case "function_call":
		if _, ok := tools[item.Name]; !ok {
			return errors.New("undeclared CPA function call")
		}
	default:
		return errors.New("unsupported CPA output item")
	}
	return nil
}

func (c *Client) forwardNativeResponses(ctx context.Context, w http.ResponseWriter, response *http.Response, model string, tools map[string]struct{}, initial Result) (result Result, failure *Failure) {
	result = initial
	defer func() {
		result.CompletedAt = time.Now()
		if failure != nil && result.BytesOut > 0 {
			result.AbortStream = true
			failure.Status = 0
		}
	}()
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || response.StatusCode != http.StatusOK || media != "application/json" && media != "text/event-stream" {
		return result, geminiProtocolFailure(errors.New("invalid CPA Responses content type"))
	}
	write := func(data []byte, token bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if result.FirstByteAt.IsZero() {
			w.Header().Set("Content-Type", media+"; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if media == "text/event-stream" {
				w.Header().Set("X-Accel-Buffering", "no")
			}
			w.WriteHeader(http.StatusOK)
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
		data, err := io.ReadAll(io.LimitReader(response.Body, maxGeminiResponseBytes+1))
		if err != nil {
			return result, c.transportFailure(ctx, err)
		}
		if len(data) > maxGeminiResponseBytes {
			return result, geminiProtocolFailure(errors.New("CPA Responses body too large"))
		}
		u, err := validateNativeResponse(data, model, tools)
		if err != nil {
			return result, geminiProtocolFailure(err)
		}
		if err := write(data, true); err != nil {
			return result, c.transportFailure(ctx, err)
		}
		result.Model, result.ServiceTier, result.Usage = model, "default", u
		return result, nil
	}
	limited := &io.LimitedReader{R: response.Body, N: maxGeminiResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), maxSSEEventBytes)
	var data bytes.Buffer
	completed, done := false, false
	var usage Usage
	consume := func() error {
		if data.Len() == 0 {
			return nil
		}
		event := bytes.Clone(data.Bytes())
		data.Reset()
		if bytes.Equal(event, []byte("[DONE]")) {
			if !completed || done {
				return errors.New("unexpected CPA Responses done")
			}
			done = true
			return write([]byte("data: [DONE]\n\n"), false)
		}
		if completed || done {
			return errors.New("event after CPA Responses completion")
		}
		o, err := cpaprotocol.Object(event)
		if err != nil {
			return err
		}
		var typ string
		if json.Unmarshal(o["type"], &typ) != nil || !nativeResponseEventType(typ) {
			return errors.New("invalid CPA Responses event")
		}
		if typ == "response.failed" || typ == "response.incomplete" || o["error"] != nil {
			return errors.New("CPA Responses failed")
		}
		if raw := o["item"]; raw != nil {
			if err := validateNativeResponseItem(raw, tools); err != nil {
				return err
			}
		}
		if raw := o["part"]; raw != nil {
			var part struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &part) != nil {
				return errors.New("invalid CPA Responses part")
			}
			if strings.HasPrefix(typ, "response.reasoning_summary_part.") {
				if part.Type != "summary_text" {
					return errors.New("invalid CPA reasoning summary")
				}
			} else if part.Type != "output_text" && part.Type != "refusal" {
				return errors.New("unsupported CPA Responses part")
			}
		}
		if strings.HasSuffix(typ, ".delta") {
			var delta string
			if raw := o["delta"]; len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &delta) != nil {
				return errors.New("invalid CPA Responses delta")
			}
		}
		if typ == "response.completed" {
			usage, err = validateNativeResponse(o["response"], model, tools)
			if err != nil {
				return err
			}
			completed = true
		}
		return write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", typ, event)), strings.HasSuffix(typ, ".delta"))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if limited.N == 0 {
			return result, geminiProtocolFailure(errors.New("CPA Responses stream too large"))
		}
		if line == "" {
			if err := consume(); err != nil {
				return result, geminiProtocolFailure(err)
			}
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			return result, geminiProtocolFailure(errors.New("invalid CPA Responses SSE field"))
		}
		if data.Len() > 0 {
			data.WriteByte('\n')
		}
		data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		if data.Len() > maxSSEEventBytes {
			return result, geminiProtocolFailure(errors.New("CPA Responses event too large"))
		}
	}
	if err := scanner.Err(); err != nil {
		return result, c.transportFailure(ctx, err)
	}
	if data.Len() != 0 || !completed {
		return result, geminiProtocolFailure(errors.New("CPA Responses missing terminal event"))
	}
	result.Model, result.ServiceTier, result.Usage = model, "default", usage
	return result, nil
}

func nativeResponseEventType(typ string) bool {
	switch typ {
	case "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete",
		"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.output_text.annotation.added",
		"response.refusal.delta", "response.refusal.done", "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done":
		return true
	default:
		return false
	}
}
