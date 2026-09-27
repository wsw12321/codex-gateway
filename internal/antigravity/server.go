package antigravity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type Executor interface {
	Run(context.Context, string) (Result, *Failure)
	Check(context.Context) error
}

type Server struct {
	runner Executor
	token  [32]byte
	ready  atomic.Bool
	gate   chan struct{}
}

func NewServer(runner Executor, token string) *Server {
	return &Server{runner: runner, token: sha256.Sum256([]byte(token)), gate: make(chan struct{}, 1)}
}

// Refresh is nonblocking if a request already owns the one process slot.
// Readiness probes never spawn concurrent agy processes or consume model quota.
func (s *Server) Refresh(ctx context.Context) {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		return
	}
	s.ready.Store(s.runner.Check(ctx) == nil)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/readyz" && r.Method == http.MethodGet {
		if !s.ready.Load() {
			writeFailure(w, &Failure{503, "upstream_unavailable", "Antigravity is not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ready": true})
		return
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		writeFailure(w, &Failure{401, "invalid_api_key", "Invalid bridge credentials"})
		return
	}
	digest := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
	if subtle.ConstantTimeCompare(digest[:], s.token[:]) != 1 {
		writeFailure(w, &Failure{401, "invalid_api_key", "Invalid bridge credentials"})
		return
	}
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		if !s.ready.Load() {
			writeFailure(w, &Failure{503, "upstream_unavailable", "Antigravity is not ready"})
			return
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": PublicModel, "object": "model", "created": 0, "owned_by": "antigravity"}}})
		return
	}
	if r.URL.Path == "/v1/responses/compact" && r.Method == http.MethodPost {
		writeFailure(w, &Failure{501, "antigravity_compact_unsupported", "Antigravity does not support compact"})
		return
	}
	native, nativeStream := false, false
	if strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
		model, method, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":")
		native = found && (model == PublicModel || model == "gemini-3.1-pro-preview-customtools") && (method == "generateContent" || method == "streamGenerateContent")
		nativeStream = method == "streamGenerateContent"
		query, queryErr := url.ParseQuery(r.URL.RawQuery)
		if native && (queryErr != nil || r.URL.RawPath != "" || len(query) > 1 || len(query["alt"]) > 1 || (len(query) != 0 && (query.Get("alt") != "sse" || !nativeStream))) {
			writeFailure(w, unsupported("query"))
			return
		}
	}
	if (r.URL.Path != "/v1/responses" && !native) || r.Method != http.MethodPost {
		writeFailure(w, &Failure{404, "unsupported_endpoint", "Unsupported endpoint"})
		return
	}
	// Acquire before reading/spooling bodies. Busy callers do not form a
	// waiting queue or allocate an unbounded number of request buffers.
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		w.Header().Set("Retry-After", "1")
		writeFailure(w, &Failure{429, "upstream_concurrency_exceeded", "Antigravity is busy"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeFailure(w, &Failure{413, "antigravity_request_too_large", "Antigravity request exceeds 1 MiB"})
		return
	}
	var request Request
	var failure *Failure
	if native {
		request, failure = DecodeGeminiRequest(body)
		request.Stream = nativeStream
	} else {
		request, failure = DecodeRequest(body)
	}
	if failure != nil {
		writeFailure(w, failure)
		return
	}
	if !s.ready.Load() {
		writeFailure(w, &Failure{503, "upstream_unavailable", "Antigravity is not ready"})
		return
	}
	result, failure := s.runner.Run(r.Context(), request.Prompt)
	if failure != nil {
		if failure.Status == 503 {
			s.ready.Store(false)
		}
		if r.Context().Err() == nil {
			writeFailure(w, failure)
		}
		return
	}
	if r.Context().Err() != nil {
		return
	}
	call, failure := decodeClientFunctionCall(result.Response, request.toolNames)
	if failure != nil {
		writeFailure(w, failure)
		return
	}
	if native {
		response := geminiResponseObject(result, call)
		if request.Stream {
			writeGeminiSSE(w, response)
		} else {
			writeJSON(w, 200, response)
		}
		return
	}
	response := responseObject(result, call)
	if request.Stream {
		writeSSE(w, response)
		return
	}
	writeJSON(w, 200, response)
}

func writeFailure(w http.ResponseWriter, failure *Failure) {
	kind := "upstream_error"
	if failure.Status >= 400 && failure.Status < 500 {
		kind = "invalid_request_error"
	}
	if failure.Status == 429 {
		kind = "rate_limit_error"
	}
	writeJSON(w, failure.Status, map[string]any{"error": map[string]string{"type": kind, "code": failure.Code, "message": failure.Message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func newID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("random source unavailable")
	}
	return prefix + hex.EncodeToString(raw[:])
}

type clientFunctionCall struct {
	Name      string
	Arguments json.RawMessage
	ID        string
}

func decodeClientFunctionCall(text string, names map[string]struct{}) (*clientFunctionCall, *Failure) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 3 && (lines[0] == "```json" || lines[0] == "```") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			trimmed = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}
	fields, ok := object([]byte(trimmed))
	if !ok || (!jsonEqual(fields["type"], `"function_call"`) && !jsonEqual(fields["type"], `"function"`)) {
		return nil, nil
	}
	name, ok := stringValue(fields["name"])
	if !ok || !validToolName(name) || uniqueJSON([]byte(trimmed)) != nil || !onlyKeys(fields, "type", "name", "arguments", "id", "call_id") {
		return nil, protocolFailure()
	}
	if _, declared := names[name]; !declared {
		return nil, protocolFailure()
	}
	args := fields["arguments"]
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if _, ok := object(args); !ok {
		return nil, protocolFailure()
	}
	id := ""
	for _, key := range []string{"id", "call_id"} {
		if raw, exists := fields[key]; exists {
			if id != "" {
				return nil, protocolFailure()
			}
			id, ok = stringValue(raw)
			if !ok || !validCallID(id) {
				return nil, protocolFailure()
			}
		}
	}
	if id == "" {
		id = newID("call_")
	}
	return &clientFunctionCall{Name: name, Arguments: args, ID: id}, nil
}

func responseObject(result Result, call *clientFunctionCall) map[string]any {
	var item map[string]any
	if call != nil {
		item = map[string]any{
			"id":        newID("fc_"),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   call.ID,
			"name":      call.Name,
			"arguments": string(call.Arguments),
		}
	} else {
		item = map[string]any{
			"id":      newID("msg_"),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": result.Response, "annotations": []any{}, "logprobs": []any{}}},
		}
	}
	return map[string]any{
		"id": newID("resp_"), "object": "response", "created_at": time.Now().Unix(), "status": "completed",
		"model": PublicModel, "service_tier": "default", "store": false, "error": nil, "incomplete_details": nil,
		"output": []any{item},
		"usage": map[string]any{
			"input_tokens": result.Usage.InputTokens, "output_tokens": result.Usage.OutputTokens, "total_tokens": result.Usage.TotalTokens,
			"input_tokens_details":  map[string]int64{"cached_tokens": result.Usage.CacheReadTokens},
			"output_tokens_details": map[string]int64{"reasoning_tokens": result.Usage.ThinkingTokens},
		},
	}
}

func writeSSE(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	sequence := 0
	emit := func(kind string, value map[string]any) bool {
		value["type"], value["sequence_number"] = kind, sequence
		sequence++
		encoded, _ := json.Marshal(value)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, encoded)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return err == nil
	}
	initial := make(map[string]any, len(response))
	for key, value := range response {
		initial[key] = value
	}
	initial["status"], initial["output"], initial["usage"] = "in_progress", []any{}, nil
	if !emit("response.created", map[string]any{"response": initial}) || !emit("response.in_progress", map[string]any{"response": initial}) {
		return
	}
	item := response["output"].([]any)[0].(map[string]any)
	if item["type"] == "function_call" {
		itemID := item["id"].(string)
		name := item["name"].(string)
		callID := item["call_id"].(string)
		arguments := item["arguments"].(string)
		if !emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": itemID, "type": "function_call", "status": "in_progress", "name": name, "call_id": callID, "arguments": ""}}) {
			return
		}
		if !emit("response.function_call_arguments.delta", map[string]any{"item_id": itemID, "output_index": 0, "delta": arguments}) {
			return
		}
		if !emit("response.function_call_arguments.done", map[string]any{"item_id": itemID, "output_index": 0, "arguments": arguments}) {
			return
		}
		if !emit("response.output_item.done", map[string]any{"output_index": 0, "item": item}) {
			return
		}
		if !emit("response.completed", map[string]any{"response": response}) {
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	part := item["content"].([]any)[0].(map[string]any)
	if !emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": item["id"], "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}) {
		return
	}
	if !emit("response.content_part.added", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}}) {
		return
	}
	if !emit("response.output_text.delta", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "delta": part["text"], "logprobs": []any{}}) {
		return
	}
	if !emit("response.output_text.done", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "text": part["text"], "logprobs": []any{}}) {
		return
	}
	if !emit("response.content_part.done", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "part": part}) {
		return
	}
	if !emit("response.output_item.done", map[string]any{"output_index": 0, "item": item}) {
		return
	}
	if !emit("response.completed", map[string]any{"response": response}) {
		return
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
