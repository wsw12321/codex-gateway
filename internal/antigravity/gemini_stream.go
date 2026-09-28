package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// This private version marker distinguishes incremental bridge responses from
// the original single-event protocol. It is never forwarded to API clients.
const geminiStreamHeader = "X-Codex-Gateway-Gemini-Stream"

const geminiHeartbeatInterval = 10 * time.Second

// potentialClientCall holds JSON tool envelopes until they can be validated as
// a complete call. Other text, including HTML/code fences, can stream normally.
func potentialClientCall(text string) bool {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	if text == "" || strings.HasPrefix(text, "{") {
		return true
	}
	for _, fence := range []string{"```json\n", "```json\r\n", "```\n", "```\r\n"} {
		if strings.HasPrefix(fence, text) {
			return true
		}
		if strings.HasPrefix(text, fence) {
			body := strings.TrimSpace(strings.TrimPrefix(text, fence))
			return body == "" || strings.HasPrefix(body, "{")
		}
	}
	return false
}

func geminiProgress(model, id, text string) map[string]any {
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]string{"text": text})
	}
	return map[string]any{
		"modelVersion": model, "responseId": id,
		"candidates": []any{map[string]any{
			"index": 0, "content": map[string]any{"role": "model", "parts": parts},
		}},
	}
}

func (s *Server) serveGeminiStream(w http.ResponseWriter, r *http.Request, request Request, runner StreamingExecutor, selected *string) {
	s.serveGeminiStreamInterval(w, r, request, runner, selected, geminiHeartbeatInterval)
}

func (s *Server) serveGeminiStreamInterval(w http.ResponseWriter, r *http.Request, request Request, runner StreamingExecutor, selected *string, interval time.Duration) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	type update struct {
		text, account string
		ack           chan error
	}
	type completion struct {
		result  Result
		failure *Failure
		account string
	}
	updates := make(chan update)
	done := make(chan completion, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- completion{failure: protocolFailure(), account: *selected}
			}
		}()
		result, failure := runner.RunStream(ctx, request.Model, request.Prompt, func(text string) error {
			ack := make(chan error, 1)
			select {
			case updates <- update{text: text, account: *selected, ack: ack}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case err := <-ack:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- completion{result, failure, *selected}
	}()
	// All ResponseWriter access belongs to this goroutine. Cancellation waits
	// for the process and credential cleanup before the account slot is released.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	id, account := newID("resp_"), ""
	started, plainText := false, false
	var pending strings.Builder
	emit := func(value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set(geminiStreamHeader, "v1")
			if account != "" {
				w.Header().Set("X-Codex-Upstream-Account", account)
			}
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	stop := func() {
		cancel()
		<-done
	}
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case update := <-updates:
			account = update.account
			text := update.text
			var err error
			if text == "" {
				err = emit(geminiProgress(request.Model, id, ""))
			} else {
				if !plainText {
					if len(text) > maxOutputBytes-pending.Len() {
						update.ack <- fmt.Errorf("stream output exceeds limit")
						stop()
						return
					}
					pending.WriteString(text)
					if !potentialClientCall(pending.String()) {
						plainText, text = true, pending.String()
						pending.Reset()
					} else {
						text = ""
					}
				}
				if text != "" {
					err = emit(geminiProgress(request.Model, id, text))
				}
			}
			update.ack <- err
			if err != nil {
				stop()
				return
			}
		case <-ticker.C:
			if started {
				// AGY 1.2.12 rejects SSE comments. An empty Gemini candidate is
				// accepted and carries neither tokens nor an executable tool.
				if emit(geminiProgress(request.Model, id, "")) != nil {
					stop()
					return
				}
			}
		case final := <-done:
			if ctx.Err() != nil {
				return
			}
			account = final.account
			failure := final.failure
			var call *clientFunctionCall
			if failure == nil {
				call, failure = decodeClientFunctionCall(final.result.Response, request.toolNames)
				if final.result.Usage == nil || (plainText && call != nil) {
					failure = protocolFailure()
				}
			}
			if failure != nil {
				if failure.Status == http.StatusServiceUnavailable {
					s.ready.Store(false)
				}
				if !started {
					if failure.Status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "1")
					}
					if account != "" {
						w.Header().Set("X-Codex-Upstream-Account", account)
					}
					writeFailure(w, failure)
				} else {
					_ = emit(map[string]any{"error": map[string]any{"status": failure.Status, "code": failure.Code}})
				}
				return
			}
			// Keep the full validated result on the private wire. Gateway checks
			// its prefix against streamed text and emits only the remaining text.
			response := geminiResponseObject(request.Model, final.result, call)
			response["responseId"] = id
			_ = emit(response)
			return
		}
	}
}
