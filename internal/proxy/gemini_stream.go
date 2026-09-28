package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const geminiStreamHeader = "X-Codex-Gateway-Gemini-Stream"

// The incremental private protocol has text-only progress events and one full
// terminal result. Tools and usage are withheld until that result passes all
// validation and EOF proves that no trailing error/event invalidated it.
func (c *Client) forwardGeminiStream(ctx context.Context, w http.ResponseWriter, response *http.Response, model string, tools map[string]struct{}, initial Result) (result Result, failure *Failure) {
	result = initial
	var text strings.Builder
	var terminal []byte
	var usage Usage
	responseID := ""
	emit := func(data []byte, token bool) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if result.FirstByteAt.IsZero() {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			result.FirstByteAt = time.Now()
		}
		n, err := fmt.Fprintf(w, "data: %s\n\n", data)
		result.BytesOut += int64(n)
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		if err == nil && token && result.FirstTokenAt.IsZero() {
			result.FirstTokenAt = time.Now()
		}
		return err
	}
	defer func() {
		result.CompletedAt = time.Now()
		if failure != nil && !result.FirstByteAt.IsZero() && ctx.Err() == nil && failure.Code != "client_disconnected" {
			// AGY can ignore a JSON error event and treat a clean EOF as success.
			// The owner must abort HTTP after settlement, even though other Gemini
			// clients can consume the sanitized error event below.
			result.AbortStream = true
			status := failure.Status
			if status < 400 || status > 599 {
				status = http.StatusBadGateway
			}
			kind := "INTERNAL"
			switch status {
			case http.StatusTooManyRequests:
				kind = "RESOURCE_EXHAUSTED"
			case http.StatusServiceUnavailable:
				kind = "UNAVAILABLE"
			case http.StatusGatewayTimeout:
				kind = "DEADLINE_EXCEEDED"
			}
			data, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "status": kind, "message": failure.Message}})
			// A named SSE error also fails explicitly in AGY's parser, which
			// otherwise ignores a data-only error object. AbortStream prevents
			// consumers that ignore the event from observing a successful EOF.
			n, _ := io.WriteString(w, "event: error\n")
			result.BytesOut += int64(n)
			_ = emit(data, false)
		}
	}()
	var downstreamErr error
	err := readGeminiEvents(response.Body, func(data []byte) error {
		if len(terminal) != 0 {
			return errors.New("event after Gemini completion")
		}
		if !utf8.Valid(data) || validateModelCatalogJSON(data) != nil {
			return errors.New("invalid Gemini stream JSON")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return errors.New("invalid Gemini stream object")
		}
		if raw, exists := fields["error"]; exists {
			var detail struct {
				Status int    `json:"status"`
				Code   string `json:"code"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if len(fields) != 1 || decoder.Decode(&detail) != nil || detail.Status < 400 || detail.Status > 599 {
				return errors.New("invalid Gemini stream error")
			}
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": detail.Code}})
			failure = c.sanitizeFailure(&http.Response{StatusCode: detail.Status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))})
			return errors.New("Gemini upstream stream failed")
		}
		if _, exists := fields["usageMetadata"]; exists {
			var err error
			usage, err = validateGeminiResponse(data, model, tools)
			if err != nil {
				return err
			}
			var id string
			if json.Unmarshal(fields["responseId"], &id) != nil || (responseID != "" && id != responseID) {
				return errors.New("Gemini response identity changed")
			}
			terminal = bytes.Clone(data)
			return nil
		}
		var progress struct {
			Model      string `json:"modelVersion"`
			ResponseID string `json:"responseId"`
			Candidates []struct {
				Index   int `json:"index"`
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						Text *string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&progress) != nil || progress.Model != model || len(progress.Candidates) != 1 ||
			progress.ResponseID == "" || (responseID != "" && responseID != progress.ResponseID) {
			return errors.New("invalid Gemini progress metadata")
		}
		candidate := progress.Candidates[0]
		if candidate.Index != 0 || candidate.Content.Role != "model" || candidate.Content.Parts == nil || len(candidate.Content.Parts) > 1 {
			return errors.New("invalid Gemini progress candidate")
		}
		token := len(candidate.Content.Parts) != 0
		if token {
			part := candidate.Content.Parts[0]
			if part.Text == nil || *part.Text == "" || len(*part.Text) > maxGeminiResponseBytes-text.Len() {
				return errors.New("invalid Gemini text delta")
			}
			text.WriteString(*part.Text)
		}
		responseID = progress.ResponseID
		downstreamErr = emit(data, token)
		return downstreamErr
	})
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return result, c.transportFailure(ctx, err)
		}
		if downstreamErr != nil {
			return result, &Failure{Type: "request_error", Code: "client_disconnected", Message: "客户端已断开连接", Cause: downstreamErr}
		}
		if failure != nil {
			return result, failure
		}
		return result, geminiProtocolFailure(err)
	}
	if len(terminal) == 0 {
		return result, geminiProtocolFailure(errors.New("missing Gemini completion"))
	}
	// The full terminal object has already passed the same model, tool, usage,
	// duplicate-key and size checks as non-streaming responses.
	var final map[string]any
	decoder := json.NewDecoder(bytes.NewReader(terminal))
	decoder.UseNumber()
	_ = decoder.Decode(&final)
	parts := final["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if text.Len() > 0 {
		if len(parts) != 1 {
			return result, geminiProtocolFailure(errors.New("Gemini streamed text changed into tools"))
		}
		part := parts[0].(map[string]any)
		full, ok := part["text"].(string)
		if !ok || !strings.HasPrefix(full, text.String()) {
			return result, geminiProtocolFailure(errors.New("Gemini final text does not match deltas"))
		}
		part["text"] = strings.TrimPrefix(full, text.String())
		terminal, _ = json.Marshal(final)
	}
	if err := emit(terminal, true); err != nil {
		if ctx.Err() != nil {
			return result, c.transportFailure(ctx, err)
		}
		return result, &Failure{Type: "request_error", Code: "client_disconnected", Message: "客户端已断开连接", Cause: err}
	}
	result.Model, result.ServiceTier, result.Usage = model, "default", usage
	return result, nil
}

func readGeminiEvents(reader io.Reader, event func([]byte) error) error {
	limited := &io.LimitedReader{R: reader, N: maxGeminiResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), maxGeminiResponseBytes)
	var data bytes.Buffer
	pending := false
	for scanner.Scan() {
		line := scanner.Text()
		if limited.N == 0 {
			return errors.New("Gemini stream exceeds limit")
		}
		if line == "" {
			if pending {
				if err := event(data.Bytes()); err != nil {
					return err
				}
				data.Reset()
				pending = false
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			return errors.New("unexpected Gemini SSE field")
		}
		if pending {
			data.WriteByte('\n')
		}
		data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		pending = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if pending || limited.N == 0 {
		return errors.New("incomplete or oversized Gemini SSE event")
	}
	return nil
}
