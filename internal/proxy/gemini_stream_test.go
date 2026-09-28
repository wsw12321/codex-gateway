package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/antigravity"
)

type liveBridgeExecutor struct {
	bridgeHTTPExecutor
	stream func(context.Context, string, func(string) error) (antigravity.Result, *antigravity.Failure)
}

func (e liveBridgeExecutor) RunStream(ctx context.Context, _, prompt string, emit func(string) error) (antigravity.Result, *antigravity.Failure) {
	return e.stream(ctx, prompt, emit)
}

func TestGeminiHTTPStreamsAcrossBothLayersBeforeCompletionAndCancels(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRequest), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release, stopped := make(chan struct{}), make(chan struct{})
			bridge, _ := newBridgeHTTPClient(t, liveBridgeExecutor{stream: func(ctx context.Context, _ string, emit func(string) error) (antigravity.Result, *antigravity.Failure) {
				defer close(stopped)
				if emit("") != nil || emit("Hello ") != nil {
					return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled"}
				}
				select {
				case <-ctx.Done():
					return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled"}
				case <-release:
					return bridgeHTTPResult(), nil
				}
			}})
			router := NewRouter(nil, bridge, map[string]string{nativeGeminiModel: nativeGeminiModel})
			done := make(chan *Failure, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, failure := router.ForwardGemini(r.Context(), w, r, nativeGeminiModel, r.URL.Path, ForwardOptions{})
				done <- failure
			}))
			defer gateway.Close()
			path := "/v1beta/models/" + nativeGeminiModel + ":streamGenerateContent"
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+path, strings.NewReader(nativeGeminiRequest))
			response, err := gateway.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			for _, want := range []string{`"parts":[]`, `"text":"Hello "`} {
				line, err := reader.ReadString('\n')
				if err != nil || !strings.Contains(line, want) {
					t.Fatalf("did not stream before result: %q %v", line, err)
				}
				_, _ = reader.ReadString('\n')
			}
			select {
			case <-stopped:
				t.Fatal("executor completed before early text was read")
			default:
			}
			if cancelRequest {
				cancel()
			} else {
				close(release)
				tail, err := io.ReadAll(reader)
				if err != nil || !strings.Contains(string(tail), `"text":"from Antigravity"`) || strings.Contains(string(tail), `"text":"Hello from Antigravity"`) {
					t.Fatalf("invalid terminal suffix: %s %v", tail, err)
				}
			}
			select {
			case failure := <-done:
				if (failure != nil) != cancelRequest || failure != nil && failure.Code != "client_disconnected" {
					t.Fatalf("failure=%v", failure)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("forwarding did not terminate")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream survived cancellation")
			}
		})
	}
}

// The real client must surface a delta before the executor is allowed to
// finish. This catches buffering in either HTTP layer or an incompatible SSE
// shape; a second case ensures stream errors cannot become successful answers.
func TestNativeGeminiRealAGYIncrementalTextAndStreamError(t *testing.T) {
	binary := os.Getenv("AGY_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("AGY_CLI_TEST_BINARY is not set")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			release := make(chan struct{})
			bridge, _ := newBridgeHTTPClient(t, liveBridgeExecutor{stream: func(ctx context.Context, prompt string, emit func(string) error) (antigravity.Result, *antigravity.Failure) {
				result := bridgeHTTPResult()
				if !strings.Contains(prompt, "Available client tool declarations") {
					result.Response = "Title"
					return result, nil
				}
				if emit("") != nil || emit("stream-prefix ") != nil {
					return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled"}
				}
				text := "stream-prefix "
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
			streaming:
				for {
					select {
					case <-release:
						break streaming
					case <-ctx.Done():
						return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled"}
					case <-ticker.C:
						text += "chunk "
						if emit("chunk ") != nil {
							return antigravity.Result{}, &antigravity.Failure{Status: 499, Code: "request_canceled"}
						}
					}
				}
				if fail {
					return antigravity.Result{}, &antigravity.Failure{Status: 502, Code: "upstream_protocol_error", Message: "private failure marker"}
				}
				result.Response = text + "finished"
				return result, nil
			}})
			router := NewRouter(nil, bridge, map[string]string{nativeGeminiModel: nativeGeminiModel})
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "flash-lite") {
					// Title generation is concurrent in the real CLI; this fixture
					// reserves the single-process bridge for the blocking main call.
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", nativeGeminiResponse)
					return
				}
				path := "/v1beta/models/" + nativeGeminiModel + ":streamGenerateContent"
				result, _ := router.ForwardGemini(r.Context(), w, r, nativeGeminiModel, path, ForwardOptions{})
				if result.AbortStream {
					panic(http.ErrAbortHandler)
				}
			}))
			defer gateway.Close()
			root := t.TempDir()
			home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
			settings := filepath.Join(home, ".gemini", "antigravity-cli")
			for _, dir := range []string{settings, project} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			policy := strings.Replace(antigravity.SafeSettings, "{", `{"modelProvider":"gemini",`, 1)
			if err := os.WriteFile(filepath.Join(settings, "settings.json"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, binary, "--print", "Reply with a short greeting. Do not use tools.", "--model", nativeGeminiModel,
				"--output-format", "stream-json", "--print-timeout", "8s", "--disable-slash-commands", "--log-file", filepath.Join(root, "cli.log"))
			cmd.Dir = project
			cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "TMPDIR=" + root, "TERM=dumb", "AGY_CLI_DISABLE_AUTO_UPDATE=true",
				"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "XDG_DATA_HOME=" + filepath.Join(root, "data"),
				"GEMINI_API_KEY=synthetic-stream-test", "GOOGLE_GEMINI_BASE_URL=" + gateway.URL}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			sawDelta, status, answer := false, "", ""
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 64<<10), 1<<20)
			for scanner.Scan() {
				var event struct {
					Step struct {
						Text string `json:"text_delta"`
					} `json:"step_update"`
					Result struct {
						Status   string `json:"status"`
						Response string `json:"response"`
					} `json:"result"`
				}
				if json.Unmarshal(scanner.Bytes(), &event) != nil {
					t.Errorf("invalid client NDJSON: %s", scanner.Bytes())
				}
				if !sawDelta && event.Step.Text != "" {
					sawDelta = true
					close(release)
				}
				if event.Result.Status != "" {
					status, answer = event.Result.Status, event.Result.Response
				}
			}
			err = cmd.Wait()
			if !sawDelta || scanner.Err() != nil || strings.Contains(stderr.String(), "private failure marker") {
				t.Fatalf("real client did not stream safely: delta=%t status=%s answer=%q scan=%v stderr=%s", sawDelta, status, answer, scanner.Err(), &stderr)
			}
			if fail {
				if err == nil || status == "SUCCESS" {
					t.Fatalf("upstream failure became success: status=%s answer=%s stderr=%s", status, answer, &stderr)
				}
			} else if err != nil || status != "SUCCESS" || !strings.HasPrefix(answer, "stream-prefix ") || !strings.HasSuffix(strings.TrimSpace(answer), "finished") || strings.Count(answer, "stream-prefix") != 1 {
				t.Fatalf("streamed answer failed or duplicated: err=%v status=%s answer=%s stderr=%s", err, status, answer, &stderr)
			}
		})
	}
}

func geminiProgressFixture(text string) string {
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]string{"text": text})
	}
	data, _ := json.Marshal(map[string]any{
		"modelVersion": nativeGeminiModel, "responseId": "response-1",
		"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}}},
	})
	return "data: " + string(data) + "\n\n"
}

func streamingGeminiRouter(t *testing.T, body string) *Router {
	t.Helper()
	return nativeGeminiRouter(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(geminiStreamHeader) != "v1" {
			t.Error("stream protocol was not negotiated")
		}
		response := routerTestResponse(200, body)
		response.Header.Set("Content-Type", "text/event-stream")
		response.Header.Set(geminiStreamHeader, "v1")
		return response, nil
	})
}

func forwardStreamFixture(t *testing.T, wire string) (Result, *Failure, *httptest.ResponseRecorder) {
	t.Helper()
	router := streamingGeminiRouter(t, wire)
	path := "/v1beta/models/" + nativeGeminiModel + ":streamGenerateContent"
	recorder := httptest.NewRecorder()
	result, failure := router.ForwardGemini(context.Background(), recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(nativeGeminiRequest)), nativeGeminiModel, path, ForwardOptions{})
	return result, failure, recorder
}

func TestGeminiStreamAssemblesTextOnceAndSettlesTerminalUsage(t *testing.T) {
	result, failure, recorder := forwardStreamFixture(t, geminiProgressFixture("")+geminiProgressFixture("hel")+"data: "+nativeGeminiResponse+"\n\n")
	if failure != nil || result.Usage.Total() != 22 || result.Model != nativeGeminiModel || result.ServiceTier != "default" || result.FirstTokenAt.IsZero() || !result.FirstTokenAt.After(result.FirstByteAt) {
		t.Fatalf("result=%+v failure=%v", result, failure)
	}
	if strings.Contains(recorder.Body.String(), `"text":"hello"`) || !strings.Contains(recorder.Body.String(), `"text":"lo"`) || recorder.Header().Get(geminiStreamHeader) != "" || result.BytesOut != int64(recorder.Body.Len()) {
		t.Fatalf("duplicated text or internal metadata: %s", recorder.Body)
	}
}

func TestGeminiStreamFailureNeverReleasesTerminalToolOrUsage(t *testing.T) {
	tool := strings.Replace(nativeGeminiResponse, `{"text":"hello"}`, `{"functionCall":{"name":"read_file","args":{"path":"private"},"id":"read-1"}}`, 1)
	for _, test := range []struct{ name, wire string }{
		{"missing completion", geminiProgressFixture("hel")},
		{"partial event", geminiProgressFixture("") + "data: {"},
		{"invalid usage", geminiProgressFixture("hel") + "data: " + strings.Replace(nativeGeminiResponse, `"totalTokenCount":22`, `"totalTokenCount":23`, 1) + "\n\n"},
		{"mismatched text", geminiProgressFixture("other") + "data: " + nativeGeminiResponse + "\n\n"},
		{"text changed to tool", geminiProgressFixture("hel") + "data: " + tool + "\n\n"},
		{"tail after tool", geminiProgressFixture("") + "data: " + tool + "\n\ndata: {}\n\n"},
		{"tool in progress event", geminiProgressFixture("") + strings.Replace(geminiProgressFixture("hel"), `{"text":"hel"}`, `{"functionCall":{"name":"shell","args":{}}}`, 1)},
		{"upstream error", geminiProgressFixture("") + `data: {"error":{"status":503,"code":"upstream_unavailable"}}` + "\n\n"},
		{"private error details", geminiProgressFixture("") + `data: {"error":{"status":502,"code":"upstream_protocol_error","message":"private"}}` + "\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, failure, recorder := forwardStreamFixture(t, test.wire)
			if failure == nil || result.Usage != (Usage{}) || strings.Contains(recorder.Body.String(), "functionCall") || strings.Contains(recorder.Body.String(), "private") || strings.Contains(recorder.Body.String(), "usageMetadata") || !strings.Contains(recorder.Body.String(), `"error":`) {
				t.Fatalf("result=%+v failure=%v body=%s", result, failure, recorder.Body)
			}
		})
	}
}

func TestGeminiStreamHeartbeatDoesNotBecomeFirstToken(t *testing.T) {
	result, failure, _ := forwardStreamFixture(t, geminiProgressFixture(""))
	if failure == nil || result.FirstByteAt.IsZero() || !result.FirstTokenAt.IsZero() || result.Usage.Total() != 0 {
		t.Fatalf("heartbeat counted as output: %+v %v", result, failure)
	}
}

func TestReadGeminiEventsRejectsUnboundedOrIncompleteStreams(t *testing.T) {
	for _, wire := range []string{"data: {}", "event: test\n\n", strings.Repeat(": padding\n", maxGeminiResponseBytes/10+1)} {
		if err := readGeminiEvents(strings.NewReader(wire), func([]byte) error { return nil }); err == nil {
			t.Fatal("accepted invalid stream")
		}
	}
}

func TestGeminiStreamPreservesLargeIntegerUsage(t *testing.T) {
	const count = int64(9007199254740993)
	response := strings.Replace(nativeGeminiResponse, `"promptTokenCount":10`, fmt.Sprintf(`"promptTokenCount":%d`, count), 1)
	response = strings.Replace(response, `"totalTokenCount":22`, fmt.Sprintf(`"totalTokenCount":%d`, count+12), 1)
	result, failure, recorder := forwardStreamFixture(t, geminiProgressFixture("hel")+"data: "+response+"\n\n")
	if failure != nil || result.Usage.InputTokens != count || !strings.Contains(recorder.Body.String(), fmt.Sprintf(`"promptTokenCount":%d`, count)) {
		t.Fatalf("usage rounded: %+v %v %s", result, failure, recorder.Body)
	}
}
