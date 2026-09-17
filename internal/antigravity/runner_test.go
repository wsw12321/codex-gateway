package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeCLIConfig struct {
	Stream, Stderr, Capture, Models, Version string
	ExitCode, FragmentSize                   int
	Hang, Child                              bool
}

type processCapture struct {
	Args                                      []string
	Prompt                                    string
	Root                                      string
	PID, ChildPID                             int
	WorkspaceEmpty, SafePolicy, SecretsAbsent bool
}

// The executable shim selects this helper using only a fixture file argument.
// Request prompts still cross the real stdin pipe exercised by Runner.Run.
func TestAgyProcess(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--agy-test-helper" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	if len(os.Args) > marker+2 && os.Args[marker+2] == "--sleep-child" {
		for {
			time.Sleep(time.Hour)
		}
	}
	var fixture fakeCLIConfig
	encoded, err := os.ReadFile(os.Args[marker+1])
	if err != nil {
		os.Exit(81)
	}
	if json.Unmarshal(encoded, &fixture) != nil {
		os.Exit(82)
	}
	args := os.Args[marker+2:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(fixture.Version)
		os.Exit(0)
	}
	if len(args) == 1 && args[0] == "models" {
		fmt.Print(fixture.Models)
		os.Exit(0)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(83)
	}
	var event struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(input), &event) != nil || event.Event != "user" {
		os.Exit(84)
	}
	cwd, _ := os.Getwd()
	entries, _ := os.ReadDir(cwd)
	settings, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".gemini/antigravity-cli/settings.json"))
	root := filepath.Dir(cwd)
	capture := processCapture{Args: args, Prompt: event.Message.Content, Root: root, PID: os.Getpid(), WorkspaceEmpty: len(entries) == 0, SafePolicy: string(settings) == SafeSettings, SecretsAbsent: os.Getenv("ANTIGRAVITY_BRIDGE_API_KEY") == "" && os.Getenv("SIDECAR_API_KEY") == ""}
	if fixture.Child {
		child := exec.Command(os.Args[0], "-test.run=^TestAgyProcess$", "--", "--agy-test-helper", os.Args[marker+1], "--sleep-child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(85)
		}
		capture.ChildPID = child.Process.Pid
	}
	if fixture.Capture != "" {
		encoded, _ := json.Marshal(capture)
		if os.WriteFile(fixture.Capture, encoded, 0600) != nil {
			os.Exit(86)
		}
	}
	if fixture.Hang {
		for {
			time.Sleep(time.Hour)
		}
	}
	for remaining := fixture.Stream; len(remaining) > 0; {
		size := fixture.FragmentSize
		if size <= 0 || size > len(remaining) {
			size = len(remaining)
		}
		_, _ = io.WriteString(os.Stdout, remaining[:size])
		remaining = remaining[size:]
	}
	_, _ = io.WriteString(os.Stderr, fixture.Stderr)
	os.Exit(fixture.ExitCode)
}

func fakeRunner(t *testing.T, fixture fakeCLIConfig) (Runner, string) {
	t.Helper()
	directory := t.TempDir()
	if fixture.Version == "" {
		fixture.Version = CLIVersion
	}
	if fixture.Models == "" {
		fixture.Models = CLIModel + "  Gemini 3.1 Pro (High)\n"
	}
	fixture.Capture = filepath.Join(directory, "capture.json")
	configPath := filepath.Join(directory, "fixture.json")
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nset -eu\nexport GORACE=atexit_sleep_ms=0\nexec " + quote(executable) + " -test.run='^TestAgyProcess$' -- --agy-test-helper " + quote(configPath) + " \"$@\"\n"
	binary := filepath.Join(directory, "agy")
	if err := os.WriteFile(binary, []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(directory, "requests")
	if err := os.Mkdir(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	return Runner{Binary: binary, TempDir: workRoot, Timeout: 5 * time.Second}, fixture.Capture
}

func readCapture(t *testing.T, path string) processCapture {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var capture processCapture
		data, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(data, &capture) == nil {
			return capture
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fake CLI never started")
	return processCapture{}
}

func assertWorkspacesClean(t *testing.T, runner Runner) {
	t.Helper()
	entries, err := os.ReadDir(runner.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("request data retained: entries=%v error=%v", entries, err)
	}
}

func TestRunnerStdinIsolationAndCleanup(t *testing.T) {
	t.Setenv("ANTIGRAVITY_BRIDGE_API_KEY", "sensitive-bridge-secret")
	t.Setenv("SIDECAR_API_KEY", "sensitive-codex-secret")
	const prompt = "private prompt marker /logout 世界"
	var logs bytes.Buffer
	runner, capturePath := fakeRunner(t, fakeCLIConfig{Stream: initEvent + textStep + resultEvent, Stderr: prompt + " sensitive-token", FragmentSize: 1})
	runner.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	result, failure := runner.Run(context.Background(), prompt)
	if failure != nil || result.Response != "Hello 世界" {
		t.Fatalf("result=%+v failure=%+v", result, failure)
	}
	capture := readCapture(t, capturePath)
	if capture.Prompt != prompt || !capture.WorkspaceEmpty || !capture.SafePolicy || !capture.SecretsAbsent {
		t.Fatalf("isolation failed: %+v", capture)
	}
	if strings.Contains(strings.Join(capture.Args, " "), prompt) || strings.Contains(logs.String(), prompt) || strings.Contains(logs.String(), "sensitive-token") {
		t.Fatal("prompt or diagnostic leaked into argv/logs")
	}
	args := strings.Join(capture.Args, " ")
	for _, required := range []string{"--input-format stream-json", "--output-format stream-json", "--model " + CLIModel, "--print-timeout 5m", "--disable-slash-commands"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing fixed CLI option %q: %s", required, args)
		}
	}
	assertWorkspacesClean(t, runner)
}

func TestRunnerFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture fakeCLIConfig
		status  int
		code    string
	}{
		{"authentication stderr", fakeCLIConfig{Stderr: "authentication required: sensitive token", ExitCode: 1}, 503, "upstream_unavailable"},
		{"quota result", fakeCLIConfig{Stream: `{"event":"result","result":{"status":"ERROR","error":"RESOURCE_EXHAUSTED secret"}}` + "\n", ExitCode: 1}, 429, "upstream_rate_limited"},
		{"timeout result", fakeCLIConfig{Stream: `{"event":"result","result":{"status":"ERROR","error":"DEADLINE_EXCEEDED"}}` + "\n", ExitCode: 1}, 504, "upstream_timeout"},
		{"quota stderr", fakeCLIConfig{Stderr: "too many requests: sensitive provider detail", ExitCode: 1}, 429, "upstream_rate_limited"},
		{"malformed output", fakeCLIConfig{Stream: initEvent + "{\n"}, 502, "upstream_protocol_error"},
		{"malformed output with auth diagnostic", fakeCLIConfig{Stream: initEvent + "{\n", Stderr: "authentication required"}, 502, "upstream_protocol_error"},
		{"duplicate result", fakeCLIConfig{Stream: initEvent + resultEvent + resultEvent}, 502, "upstream_protocol_error"},
		{"tool event", fakeCLIConfig{Stream: initEvent + `{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"command"}}` + "\n"}, 502, "upstream_protocol_error"},
		{"process failed after result", fakeCLIConfig{Stream: initEvent + resultEvent, Stderr: "secret process detail", ExitCode: 1}, 502, "upstream_process_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := fakeRunner(t, test.fixture)
			_, failure := runner.Run(context.Background(), "private prompt")
			if failure == nil || failure.Status != test.status || failure.Code != test.code {
				t.Fatalf("failure=%+v, want%d %s", failure, test.status, test.code)
			}
			if strings.Contains(failure.Message, "secret") || strings.Contains(failure.Message, "private prompt") {
				t.Fatalf("sensitive diagnostic exposed: %+v", failure)
			}
			assertWorkspacesClean(t, runner)
		})
	}
}

func TestRunnerTimeoutAndCancellationKillProcessGroup(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			runner, path := fakeRunner(t, fakeCLIConfig{Hang: true, Child: true})
			if timeout {
				runner.Timeout = 300 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *Failure, 1)
			go func() { _, failure := runner.Run(ctx, "private prompt"); done <- failure }()
			capture := readCapture(t, path)
			if capture.ChildPID <= 0 {
				t.Fatal("no child process spawned")
			}
			if !timeout {
				cancel()
			}
			select {
			case failure := <-done:
				want := 499
				if timeout {
					want = 504
				}
				if failure == nil || failure.Status != want {
					t.Fatalf("failure=%+v want%d", failure, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not stop request")
			}
			for _, pid := range []int{capture.PID, capture.ChildPID} {
				deadline := time.Now().Add(time.Second)
				for processAlive(pid) && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if processAlive(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("process%d survived group cancellation", pid)
				}
			}
			assertWorkspacesClean(t, runner)
		})
	}
}

func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	_, rest, ok := strings.Cut(string(data), ") ")
	return !ok || !strings.HasPrefix(rest, "Z ")
}

func TestRunnerReadinessRequiresExactModelAndVersion(t *testing.T) {
	for _, test := range []struct {
		name, models, version string
		wantOK                bool
	}{
		{"target present", CLIModel + "  Gemini Pro\n", CLIVersion, true},
		{"prefix rejected", CLIModel + "-next Gemini Pro\n", CLIVersion, false},
		{"description rejected", "other-model " + CLIModel + "\n", CLIVersion, false},
		{"wrong version", CLIModel + " Gemini Pro\n", "1.2.5", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := fakeRunner(t, fakeCLIConfig{Models: test.models, Version: test.version})
			err := runner.Check(context.Background())
			if (err == nil) != test.wantOK {
				t.Fatalf("Check error=%v wantOK%v", err, test.wantOK)
			}
			assertWorkspacesClean(t, runner)
		})
	}
}
