package antigravity

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseStreamUpdatesArriveBeforeResult(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	updates := make(chan string, 4)
	done := make(chan *Failure, 1)
	go func() {
		_, failure := parseStreamUpdates(CLIModel, reader, func(text string) error { updates <- text; return nil })
		done <- failure
	}()
	go func() {
		_, _ = io.WriteString(writer, initEvent+strings.Replace(textStep, "untrusted partial text", "Hello ", 1))
	}()
	for _, want := range []string{"", "Hello "} {
		select {
		case got := <-updates:
			if got != want {
				t.Fatalf("update %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("update was buffered until completion")
		}
	}
	select {
	case <-done:
		t.Fatal("completed without terminal result")
	default:
	}
	_, _ = io.WriteString(writer, resultEvent)
	_ = writer.Close()
	if failure := <-done; failure != nil {
		t.Fatal(failure)
	}
}

func TestParseStreamUpdatesRejectUnsafeTailAndChangedText(t *testing.T) {
	for _, source := range []string{
		initEvent + textStep + resultEvent,
		initEvent + `{"event":"step_update","step_update":{"step_type":"agent_response","state":"ACTIVE","tool_name":"read_file","text_delta":"secret"}}` + "\n",
		initEvent + resultEvent + textStep,
	} {
		var text strings.Builder
		_, failure := parseStreamUpdates(CLIModel, strings.NewReader(source), func(delta string) error { text.WriteString(delta); return nil })
		if failure == nil || failure.Status != 502 || strings.Contains(text.String(), "secret") {
			t.Fatalf("failure=%v text=%q", failure, text.String())
		}
	}
}

func TestRunnerStreamWriteFailureKillsProcessAndCleansWorkspace(t *testing.T) {
	runner, capture := fakeRunner(t, fakeCLIConfig{Stream: initEvent + resultEvent, Child: true})
	_, failure := runner.RunStream(context.Background(), CLIModel, "test", func(string) error { return errors.New("client disconnected") })
	if failure == nil || failure.Status != 499 {
		t.Fatalf("failure=%v", failure)
	}
	_ = readCapture(t, capture)
	assertWorkspacesClean(t, runner)
}

type streamingTestExecutor struct {
	run func(context.Context, func(string) error) (Result, *Failure)
}

func (e streamingTestExecutor) Check(context.Context) ([]string, error) {
	return []string{CLIModel}, nil
}
func (e streamingTestExecutor) Run(ctx context.Context, _, _ string) (Result, *Failure) {
	return e.run(ctx, func(string) error { return nil })
}
func (e streamingTestExecutor) RunStream(ctx context.Context, _, _ string, emit func(string) error) (Result, *Failure) {
	return e.run(ctx, emit)
}

func TestGeminiStreamKeepsLongToolAliveWithoutExposingArguments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	call := `{"type":"function_call","name":"write_file","arguments":{"content":"private generated file"}}`
	executor := streamingTestExecutor{run: func(ctx context.Context, emit func(string) error) (Result, *Failure) {
		if emit("") != nil || emit("```json\n"+call) != nil {
			return Result{}, &Failure{499, "request_canceled", "canceled"}
		}
		select {
		case <-release:
			return Result{Response: "```json\n" + call + "\n```", Usage: &Usage{}}, nil
		case <-ctx.Done():
			return Result{}, &Failure{499, "request_canceled", "canceled"}
		}
	}}
	bridge := NewServer(executor, testBridgeToken)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selected := ""
		bridge.serveGeminiStreamInterval(w, r, Request{Model: CLIModel, toolNames: map[string]struct{}{"write_file": {}}}, executor, &selected, 10*time.Millisecond)
	}))
	defer upstream.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
	response, err := upstream.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for i := 0; i < 3; i++ {
		line, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(line, `"parts":[]`) || strings.Contains(line, "private") || strings.HasPrefix(line, ":") {
			t.Fatalf("invalid heartbeat: %q %v", line, err)
		}
		_, _ = reader.ReadString('\n')
	}
	close(release)
	tail, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(tail), `"functionCall"`) || !strings.Contains(string(tail), "private generated file") {
		t.Fatalf("tool not completed: %s %v", tail, err)
	}
}

func TestPotentialClientCallAllowsOrdinaryCodeToStream(t *testing.T) {
	for _, text := range []string{"", " ", "{", "\u3000{", "\v\f{", "```", "```j", "```json\n{", "```\n{"} {
		if !potentialClientCall(text) {
			t.Fatalf("tool prefix released: %q", text)
		}
	}
	for _, text := range []string{"Hello", "<html>", "```html\n<svg>", "```go\npackage main", "```\n<html>"} {
		if potentialClientCall(text) {
			t.Fatalf("ordinary text buffered: %q", text)
		}
	}
}

func TestAccountStreamRetriesOnlyBeforeOutput(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "before output", true: "after output"}[start], func(t *testing.T) {
			_, selector := newConversationSelector(t, map[string]int64{accountID("default"): 1, accountID("work"): 1})
			primary := streamingTestExecutor{run: func(_ context.Context, emit func(string) error) (Result, *Failure) {
				if start {
					_ = emit("")
				}
				return Result{}, &Failure{503, "upstream_unavailable", "unavailable"}
			}}
			backup := &accountExecutor{}
			server := accountServer(t, selector.URL, primary, backup)
			selected := ""
			var release func()
			ctx := context.WithValue(context.Background(), accountRequestKey{}, accountRequest{userID: accountUserID, selected: &selected, release: &release})
			_, failure := server.manager.RunStream(ctx, CLIModel, "hello", func(string) error { return nil })
			if release != nil {
				release()
			}
			if start && (failure == nil || backup.runs.Load() != 0 || selected != accountID("default")) {
				t.Fatalf("retried after stream started: failure=%v backup=%d selected=%s", failure, backup.runs.Load(), selected)
			}
			if !start && (failure != nil || backup.runs.Load() != 1 || selected != accountID("work")) {
				t.Fatalf("failed to retry before output: failure=%v backup=%d selected=%s", failure, backup.runs.Load(), selected)
			}
			assertAccountConversationUsage(t, server, "default", 0, 0)
			assertAccountConversationUsage(t, server, "work", 0, 0)
		})
	}
}
