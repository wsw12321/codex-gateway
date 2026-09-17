package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const CLIVersion = "1.2.4"

// SafeSettings is generated afresh for every process; persisted CLI settings,
// plugins, hooks, MCP servers and conversation history are never loaded.
const SafeSettings = `{"toolPermission":"strict","allowNonWorkspaceAccess":false,"enableTelemetry":false,"useG1Credits":false,"permissions":{"deny":["read_file(*)","write_file(*)","read_url(*)","execute_url(*)","command(*)","unsandboxed(*)","mcp(*)"],"allow":[],"ask":[]}}`

type Runner struct {
	Binary  string
	TempDir string
	Timeout time.Duration
	Logger  *slog.Logger
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
	count int64
}

type countedReader struct {
	reader io.Reader
	count  int64
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.count += int64(n)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if remaining > n {
			remaining = n
		}
		_, _ = b.Buffer.Write(p[:remaining])
	}
	return n, nil
}

func (r Runner) workspace() (string, string, error) {
	root, err := os.MkdirTemp(r.TempDir, "agy-request-")
	if err != nil {
		return "", "", err
	}
	for _, dir := range []string{"work", "home/.gemini/antigravity-cli", "tmp", "config", "cache", "data"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			_ = os.RemoveAll(root)
			return "", "", err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "home/.gemini/antigravity-cli/settings.json"), []byte(SafeSettings), 0600); err != nil {
		_ = os.RemoveAll(root)
		return "", "", err
	}
	return root, filepath.Join(root, "work"), nil
}

func (r Runner) command(ctx context.Context, root, cwd string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Dir = cwd
	// Deliberately exclude gateway/bridge secrets and arbitrary CLI settings
	// from the child environment. Authentication goes through Secret Service.
	for _, key := range []string{"PATH", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+filepath.Join(root, "home"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_CACHE_HOME="+filepath.Join(root, "cache"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "TMPDIR="+filepath.Join(root, "tmp"), "AGY_CLI_DISABLE_AUTO_UPDATE=true", "TERM=dumb")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func (r Runner) Run(ctx context.Context, prompt string) (Result, *Failure) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	root, cwd, err := r.workspace()
	if err != nil {
		return Result{}, &Failure{503, "upstream_unavailable", "Antigravity workspace is unavailable"}
	}
	defer os.RemoveAll(root)
	cmd := r.command(ctx, root, cwd, "--input-format", "stream-json", "--output-format", "stream-json", "--model", CLIModel, "--print-timeout", "5m", "--disable-slash-commands", "--log-file", filepath.Join(root, "cli.log"))
	payload, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": prompt}})
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	stderr := &cappedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, protocolFailure()
	}
	if err := cmd.Start(); err != nil {
		return Result{}, &Failure{503, "upstream_unavailable", "Antigravity executable is unavailable"}
	}
	counted := &countedReader{reader: stdout}
	result, failure := parseStream(counted)
	contextErr := ctx.Err()
	if failure != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	// Reap even descendants that closed their pipes before the parent exited.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if stderr.count > 0 && r.Logger != nil {
		// Raw stderr can contain prompts, auth tokens and URLs. Redact the
		// entire diagnostic instead of relying on token-pattern heuristics.
		r.Logger.Warn("agy diagnostic redacted", "bytes", stderr.count)
	}
	if contextErr == nil && failure == nil {
		contextErr = ctx.Err()
	}
	if errors.Is(contextErr, context.DeadlineExceeded) {
		return Result{}, &Failure{504, "upstream_timeout", "Antigravity request timed out"}
	}
	if errors.Is(contextErr, context.Canceled) {
		return Result{}, &Failure{499, "request_canceled", "Request canceled"}
	}
	if failure != nil {
		// Authentication can fail before stdout emits even an init event.
		if counted.count == 0 && stderr.Len() > 0 {
			classified := classifyFailure(stderr.String())
			if classified.Status != 502 {
				return Result{}, classified
			}
		}
		return Result{}, failure
	}
	if waitErr != nil {
		return Result{}, classifyFailure(stderr.String())
	}
	return result, nil
}

func (r Runner) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, cwd, err := r.workspace()
	if err != nil {
		return errors.New("Antigravity check workspace unavailable")
	}
	defer os.RemoveAll(root)
	for _, args := range [][]string{{"--version"}, {"models"}} {
		cmd := r.command(ctx, root, cwd, args...)
		stdout := &cappedBuffer{limit: 1 << 20}
		cmd.Stdout = stdout
		cmd.Stderr = io.Discard
		runErr := cmd.Run()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		if runErr != nil {
			return errors.New("Antigravity model check failed")
		}
		if stdout.count > 1<<20 {
			return errors.New("Antigravity model check exceeds limit")
		}
		if args[0] == "--version" {
			if strings.TrimSpace(stdout.String()) != CLIVersion {
				return errors.New("Antigravity executable version differs from pin")
			}
			continue
		}
		for _, line := range strings.Split(stdout.String(), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == CLIModel {
				return nil
			}
		}
	}
	return errors.New("Antigravity target model unavailable")
}
