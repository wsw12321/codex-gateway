package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testBridgeToken = "test-bridge-token-at-least-32-bytes-long"

func bridgeRequest(server http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testBridgeToken)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}

func TestBridgeJSONAndSSEFromFakeCLI(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[stream], func(t *testing.T) {
			runner, _ := fakeRunner(t, fakeCLIConfig{Stream: initEvent + textStep + resultEvent, FragmentSize: 3})
			server := NewServer(runner, testBridgeToken)
			server.Refresh(context.Background())
			body := `{"model":"` + PublicModel + `","input":"hello","stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
			response := bridgeRequest(server, http.MethodPost, "/v1/responses", body)
			if response.Code != 200 {
				t.Fatalf("response=%d %s", response.Code, response.Body)
			}
			if strings.Contains(response.Body.String(), "untrusted partial") || strings.Contains(response.Body.String(), "run_command") || strings.Contains(response.Body.String(), "tool_info") {
				t.Fatal("internal agent events leaked")
			}
			var completed map[string]any
			if !stream {
				if err := json.Unmarshal(response.Body.Bytes(), &completed); err != nil {
					t.Fatal(err)
				}
			} else {
				if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") || !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
					t.Fatalf("invalid SSE framing: %s", response.Body)
				}
				var kinds []string
				for _, frame := range strings.Split(response.Body.String(), "\n\n") {
					if frame == "" || frame == "data: [DONE]" {
						continue
					}
					lines := strings.Split(frame, "\n")
					if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
						t.Fatalf("invalid frame: %q", frame)
					}
					var event map[string]any
					if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &event); err != nil {
						t.Fatal(err)
					}
					kind := strings.TrimPrefix(lines[0], "event: ")
					if event["type"] != kind || event["sequence_number"] != float64(len(kinds)) {
						t.Fatalf("SSE sequence mismatch: %+v", event)
					}
					kinds = append(kinds, kind)
					if kind == "response.completed" {
						completed = event["response"].(map[string]any)
					}
				}
				want := "response.created,response.in_progress,response.output_item.added,response.content_part.added,response.output_text.delta,response.output_text.done,response.content_part.done,response.output_item.done,response.completed"
				if strings.Join(kinds, ",") != want {
					t.Fatalf("events=%v", kinds)
				}
			}
			if completed["model"] != PublicModel || completed["status"] != "completed" || completed["store"] != false {
				t.Fatalf("response metadata=%+v", completed)
			}
			usage := completed["usage"].(map[string]any)
			if usage["input_tokens"] != float64(10415) || usage["output_tokens"] != float64(657) || usage["total_tokens"] != float64(11072) || usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(616) || usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(8113) {
				t.Fatalf("usage double-counted thinking or lost cache: %+v", usage)
			}
			output := completed["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			if output["text"] != "Hello 世界" {
				t.Fatalf("final text=%+v", output)
			}
			assertWorkspacesClean(t, runner)
		})
	}
}

type blockingExecutor struct {
	entered chan struct{}
	release chan struct{}
	checks  atomic.Int64
}

func (b *blockingExecutor) Check(context.Context) error { b.checks.Add(1); return nil }
func (b *blockingExecutor) Run(ctx context.Context, _ string) (Result, *Failure) {
	close(b.entered)
	select {
	case <-ctx.Done():
		return Result{}, &Failure{499, "request_canceled", "Canceled"}
	case <-b.release:
	}
	return Result{Status: "SUCCESS", Response: "ok", NumTurns: 1, Usage: &Usage{}}, nil
}

type failOnRead struct{ read atomic.Bool }

func (f *failOnRead) Read([]byte) (int, error) { f.read.Store(true); return 0, io.EOF }
func (*failOnRead) Close() error               { return nil }

func TestBridgeRejectsConcurrencyBeforeReadingBody(t *testing.T) {
	executor := &blockingExecutor{entered: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(executor, testBridgeToken)
	server.Refresh(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- bridgeRequest(server, "POST", "/v1/responses", `{"model":"`+PublicModel+`","input":"hello"}`)
	}()
	select {
	case <-executor.entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter")
	}
	defer close(executor.release)
	body := &failOnRead{}
	r := httptest.NewRequest("POST", "/v1/responses", body)
	r.Header.Set("Authorization", "Bearer "+testBridgeToken)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "upstream_concurrency_exceeded") || w.Header().Get("Retry-After") != "1" || body.read.Load() {
		t.Fatalf("busy response=%d %s bodyRead=%v", w.Code, w.Body, body.read.Load())
	}
	server.Refresh(context.Background())
	if executor.checks.Load() != 1 {
		t.Fatal("readiness spawned concurrent CLI process")
	}
}

func TestBridgeReadinessAuthenticationAndUnsupportedInput(t *testing.T) {
	runner, _ := fakeRunner(t, fakeCLIConfig{Stderr: "Please sign in to view available models", ExitCode: 1})
	server := NewServer(runner, testBridgeToken)
	if response := bridgeRequest(server, "GET", "/v1/models", ""); response.Code != 503 {
		t.Fatalf("unready models=%d", response.Code)
	}
	server.Refresh(context.Background())
	if response := bridgeRequest(server, "GET", "/v1/models", ""); response.Code != 200 || !strings.Contains(response.Body.String(), PublicModel) {
		t.Fatalf("ready models=%d %s", response.Code, response.Body)
	}
	for _, auth := range []string{"", "Bearer wrong", "bearer " + testBridgeToken} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("accepted authorization %q", auth)
		}
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Add("Authorization", "Bearer "+testBridgeToken)
	r.Header.Add("Authorization", "Bearer "+testBridgeToken)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("duplicate credentials accepted")
	}
	if response := bridgeRequest(server, "POST", "/v1/responses", `{"model":"`+PublicModel+`","input":"hello","tools":[]}`); response.Code != 400 || !strings.Contains(response.Body.String(), "antigravity_tools_unsupported") {
		t.Fatalf("unsupported body=%d %s", response.Code, response.Body)
	}
	if response := bridgeRequest(server, "POST", "/v1/responses/compact", `{}`); response.Code != 501 {
		t.Fatalf("compact=%d", response.Code)
	}
	if response := bridgeRequest(server, "POST", "/v1/responses", `{"model":"`+PublicModel+`","input":"hello"}`); response.Code != 503 {
		t.Fatalf("expired auth=%d %s", response.Code, response.Body)
	}
	if response := bridgeRequest(server, "GET", "/v1/models", ""); response.Code != 503 {
		t.Fatalf("expired auth retained model=%d", response.Code)
	}
}

func TestBridgeProtocolFailureDoesNotLeakSuccessfulSSE(t *testing.T) {
	runner, _ := fakeRunner(t, fakeCLIConfig{Stream: initEvent + resultEvent + resultEvent})
	server := NewServer(runner, testBridgeToken)
	server.Refresh(context.Background())
	response := bridgeRequest(server, "POST", "/v1/responses", `{"model":"`+PublicModel+`","input":"hello","stream":true}`)
	if response.Code != 502 || strings.Contains(response.Body.String(), "Hello 世界") || strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("bad stream leaked partial success: %d %s", response.Code, response.Body)
	}
}
