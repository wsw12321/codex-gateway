package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeCLIConfig struct {
	Stream, Stderr, Capture, Models, Version string
	WantCredential, UpdatedCredential, Usage string
	ExitCode, FragmentSize                   int
	Hang, Child                              bool
}

type processCapture struct {
	Credential                                                string
	Args                                                      []string
	Prompt                                                    string
	Root                                                      string
	DBusSessionBusAddress, GNOMEKeyringControl, XDGRuntimeDir string
	PID, ChildPID                                             int
	WorkspaceEmpty, SafePolicy, SecretsAbsent                 bool
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
	credentialPath := filepath.Join(os.Getenv("HOME"), ".gemini/antigravity-cli/antigravity-oauth-token")
	credential, _ := os.ReadFile(credentialPath)
	if fixture.WantCredential != "" && string(credential) != fixture.WantCredential {
		os.Exit(87)
	}
	if fixture.UpdatedCredential != "" {
		if os.WriteFile(credentialPath, []byte(fixture.UpdatedCredential), 0600) != nil {
			os.Exit(88)
		}
	}
	if len(args) == 0 {
		os.Exit(fixture.ExitCode)
	}
	if len(args) > 1 && args[0] == "--print" && args[1] == "/usage" {
		fmt.Print(fixture.Usage)
		os.Exit(fixture.ExitCode)
	}
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
	capture := processCapture{
		Args: args, Prompt: event.Message.Content, Root: root, PID: os.Getpid(), Credential: string(credential),
		DBusSessionBusAddress: os.Getenv("DBUS_SESSION_BUS_ADDRESS"),
		GNOMEKeyringControl:   os.Getenv("GNOME_KEYRING_CONTROL"),
		XDGRuntimeDir:         os.Getenv("XDG_RUNTIME_DIR"),
		WorkspaceEmpty:        len(entries) == 0, SafePolicy: string(settings) == SafeSettings,
		SecretsAbsent: os.Getenv("ANTIGRAVITY_BRIDGE_API_KEY") == "" && os.Getenv("SIDECAR_API_KEY") == "",
	}
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
	return Runner{Binary: binary, TempDir: workRoot, Timeout: 5 * time.Second, Credentials: &runnerCredentials{data: testCredential}}, fixture.Capture
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
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/test-dbus")
	t.Setenv("GNOME_KEYRING_CONTROL", "/run/antigravity/keyring")
	t.Setenv("XDG_RUNTIME_DIR", "/run/antigravity")
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
	if capture.Prompt != prompt || !capture.WorkspaceEmpty || !capture.SafePolicy || !capture.SecretsAbsent ||
		capture.DBusSessionBusAddress != "unix:path=/tmp/test-dbus" ||
		capture.GNOMEKeyringControl != "/run/antigravity/keyring" ||
		capture.XDGRuntimeDir != "/run/antigravity" {
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

// This fake models only the persistence boundary; credentials_test exercises
// the production file validation and Secret Service protocol separately.
const testCredential = `{"token":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"},"project_id":"synthetic-project","region":"synthetic-region","user_tier":"synthetic-tier"}`
const refreshedCredential = `{"token":{"access_token":"refreshed-access","refresh_token":"refreshed-refresh","token_type":"Bearer","expiry":"2099-02-01T00:00:00Z"},"project_id":"synthetic-project","region":"synthetic-region","user_tier":"synthetic-tier"}`

type runnerCredentials struct {
	mu                  sync.Mutex
	data                string
	restoreErr, saveErr error
	restores, saves     int
	saveContextErr      error
	saveDeadline        time.Duration
}

func (c *runnerCredentials) Restore(ctx context.Context, home string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restores++
	if c.restoreErr != nil {
		return c.restoreErr
	}
	return os.WriteFile(filepath.Join(home, ".gemini/antigravity-cli/antigravity-oauth-token"), []byte(c.data), 0600)
}
func (c *runnerCredentials) Save(ctx context.Context, home string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves++
	c.saveContextErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		c.saveDeadline = time.Until(deadline)
	}
	if c.saveErr != nil {
		return c.saveErr
	}
	data, err := os.ReadFile(filepath.Join(home, ".gemini/antigravity-cli/antigravity-oauth-token"))
	if err == nil {
		c.data = string(data)
	}
	return err
}

func TestRunnerCredentialRefreshPersistsForNextInvocation(t *testing.T) {
	runner, capture := fakeRunner(t, fakeCLIConfig{Stream: initEvent + resultEvent, UpdatedCredential: refreshedCredential})
	for i, expected := range []string{testCredential, refreshedCredential} {
		result, failure := runner.Run(context.Background(), "private prompt")
		if failure != nil || result.Response == "" {
			t.Fatalf("invocation %d failure=%v", i, failure)
		}
		if got := readCapture(t, capture).Credential; got != expected {
			t.Fatalf("invocation %d did not restore expected credential", i)
		}
	}
	c := runner.Credentials.(*runnerCredentials)
	if c.data != refreshedCredential || c.restores != 2 || c.saves != 2 {
		t.Fatal("credential was not refreshed and restored")
	}
	assertWorkspacesClean(t, runner)
}

func TestRunnerSavesRefreshAfterFailureTimeoutAndCancellation(t *testing.T) {
	for _, scenario := range []string{"failure", "timeout", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := fakeCLIConfig{UpdatedCredential: refreshedCredential}
			if scenario == "failure" {
				fixture.ExitCode = 7
				fixture.Stderr = "private failure"
			} else {
				fixture.Hang = true
				fixture.Child = true
			}
			runner, capture := fakeRunner(t, fixture)
			if scenario == "timeout" {
				runner.Timeout = 500 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *Failure, 1)
			go func() { _, failure := runner.Run(ctx, "private prompt"); done <- failure }()
			if scenario == "cancel" {
				readCapture(t, capture)
				cancel()
			}
			failure := <-done
			if failure == nil {
				t.Fatal("request unexpectedly succeeded")
			}
			c := runner.Credentials.(*runnerCredentials)
			if c.data != refreshedCredential || c.saves != 1 || c.saveContextErr != nil || c.saveDeadline <= 0 || c.saveDeadline > 5*time.Second {
				t.Fatalf("refresh not saved with independent bounded context: %+v", c)
			}
			assertWorkspacesClean(t, runner)
		})
	}
}

func TestRunnerCredentialFailuresCloseReadiness(t *testing.T) {
	for _, stage := range []string{"restore", "save"} {
		t.Run(stage, func(t *testing.T) {
			runner, _ := fakeRunner(t, fakeCLIConfig{Stream: initEvent + resultEvent})
			var logs bytes.Buffer
			runner.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			server := NewServer(runner, "bridge-test-token")
			server.Refresh(context.Background())
			if !server.ready.Load() {
				t.Fatal("initial readiness failed")
			}
			c := runner.Credentials.(*runnerCredentials)
			// Unknown errors cannot leak their text into auth diagnostics or HTTP.
			if stage == "restore" {
				c.restoreErr = errors.New("synthetic-sensitive-token")
			} else {
				c.saveErr = errors.New("synthetic-sensitive-token")
			}
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+PublicModel+`","input":"hello","stream":true}`))
			req.Header.Set("Authorization", "Bearer bridge-test-token")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, req)
			if response.Code != 503 || server.ready.Load() || strings.Contains(response.Body.String(), "response.completed") {
				t.Fatalf("save/restore failure accepted: %d %s", response.Code, response.Body)
			}
			if strings.Contains(logs.String()+response.Body.String(), "synthetic-sensitive-token") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestReadinessSavesEveryCLIInvocation(t *testing.T) {
	runner, _ := fakeRunner(t, fakeCLIConfig{UpdatedCredential: refreshedCredential})
	if err := runner.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := runner.Credentials.(*runnerCredentials)
	if c.restores != 2 || c.saves != 2 || c.data != refreshedCredential {
		t.Fatal("health checks bypassed credential lifecycle")
	}
}
