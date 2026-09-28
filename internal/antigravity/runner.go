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

	"github.com/wsw/codex-gateway/internal/config"
)

const CLIVersion = "1.2.4"

// SafeSettings is generated afresh for every process; persisted CLI settings,
// plugins, hooks, MCP servers and conversation history are never loaded.
const SafeSettings = `{"toolPermission":"strict","allowNonWorkspaceAccess":false,"enableTelemetry":false,"useG1Credits":false,"permissions":{"deny":["read_file(*)","write_file(*)","read_url(*)","execute_url(*)","command(*)","unsandboxed(*)","mcp(*)"],"allow":[],"ask":[]}}`

type Runner struct {
	Binary      string
	TempDir     string
	Timeout     time.Duration
	Logger      *slog.Logger
	Credentials Credentials
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

// Override bytes.Buffer.ReadFrom: os/exec's io.Copy otherwise bypasses Write,
// losing both output limits and the redacted diagnostic byte count.
func (b *cappedBuffer) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{b}, reader)
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
	// #nosec G204 -- Binary comes from deployment configuration; arguments are internal, and prompts use stdin without a shell.
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Dir = cwd
	// Deliberately exclude gateway/bridge secrets and arbitrary CLI settings
	// from the child environment. Only the validated authentication file is
	// restored from Secret Service into this private HOME.
	for _, key := range []string{"PATH", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "DBUS_SESSION_BUS_ADDRESS", "GNOME_KEYRING_CONTROL", "XDG_RUNTIME_DIR"} {
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

// restore runs before every CLI invocation, including health checks.
func (r Runner) restore(ctx context.Context, root string) *AuthError {
	if err := r.credentials().Restore(ctx, filepath.Join(root, "home")); err != nil {
		return credentialAuthError("credential_restore", err)
	}
	return nil
}

func (r Runner) credentials() Credentials {
	if r.Credentials != nil {
		return r.Credentials
	}
	return &KeyringCredentials{}
}

// save is independent of the request context so a completed token refresh
// survives client cancellation and upstream timeouts. Call only after Wait and
// process-group cleanup, while the server still owns its serialization slot.
func (r Runner) save(root string) *AuthError {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.credentials().Save(ctx, filepath.Join(root, "home")); err != nil {
		return credentialAuthError("credential_save", err)
	}
	return nil
}

func (r Runner) Run(ctx context.Context, model, prompt string) (Result, *Failure) {
	result, failure, diagnostic := r.run(ctx, model, prompt)
	if diagnostic != nil && r.Logger != nil {
		r.Logger.Warn("agy operation failed", "stage", diagnostic.Stage, "category", diagnostic.Category, "exit_code", diagnostic.ExitCode)
	}
	return result, failure
}

func credentialFailure() *Failure {
	return &Failure{503, "upstream_unavailable", "Antigravity credentials are unavailable"}
}

func (r Runner) run(ctx context.Context, model, prompt string) (Result, *Failure, *AuthError) {
	if !config.IsAntigravityModel(model) {
		return Result{}, unsupported("model"), nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	root, cwd, err := r.workspace()
	if err != nil {
		return Result{}, &Failure{503, "upstream_unavailable", "Antigravity workspace is unavailable"}, &AuthError{"generation", "io_failed", 1}
	}
	defer os.RemoveAll(root)
	if err := r.restore(ctx, root); err != nil {
		return Result{}, credentialFailure(), err
	}
	cmd := r.command(ctx, root, cwd, "--input-format", "stream-json", "--output-format", "stream-json", "--model", model, "--print-timeout", "5m", "--disable-slash-commands", "--log-file", filepath.Join(root, "cli.log"))
	payload, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": prompt}})
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	stderr := &cappedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, protocolFailure(), &AuthError{"generation", "io_failed", 1}
	}
	if err := cmd.Start(); err != nil {
		return Result{}, &Failure{503, "upstream_unavailable", "Antigravity executable is unavailable"}, commandAuthError("generation", ctx.Err(), err)
	}
	counted := &countedReader{reader: stdout}
	result, failure := parseStream(model, counted)
	contextErr := ctx.Err()
	if failure != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	// Reap even descendants that closed their pipes before the parent exited.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if stderr.count > 0 && r.Logger != nil {
		// Raw stderr can contain prompts, auth tokens and URLs.
		r.Logger.Warn("agy diagnostic redacted", "bytes", stderr.count)
	}
	if err := r.save(root); err != nil {
		return Result{}, credentialFailure(), err
	}
	if contextErr == nil && failure == nil {
		contextErr = ctx.Err()
	}
	if errors.Is(contextErr, context.DeadlineExceeded) {
		return Result{}, &Failure{504, "upstream_timeout", "Antigravity request timed out"}, commandAuthError("generation", contextErr, waitErr)
	}
	if errors.Is(contextErr, context.Canceled) {
		return Result{}, &Failure{499, "request_canceled", "Request canceled"}, commandAuthError("generation", contextErr, waitErr)
	}
	if failure != nil {
		// Authentication can fail before stdout emits even an init event.
		if counted.count == 0 && stderr.Len() > 0 {
			classified := classifyFailure(stderr.String())
			if classified.Status != 502 {
				return Result{}, classified, commandAuthError("generation", nil, waitErr)
			}
		}
		return Result{}, failure, &AuthError{"generation", "invalid_response", 1}
	}
	if waitErr != nil {
		return Result{}, classifyFailure(stderr.String()), commandAuthError("generation", nil, waitErr)
	}
	return result, nil, nil
}

// probe uses no input and never exposes CLI output. Each invocation has a new
// HOME and a credential restore/save cycle, just like a generation request.
func (r Runner) probe(ctx context.Context, stage string, args ...string) ([]byte, *AuthError) {
	root, cwd, err := r.workspace()
	if err != nil {
		return nil, &AuthError{stage, "io_failed", 1}
	}
	defer os.RemoveAll(root)
	if err := r.restore(ctx, root); err != nil {
		return nil, err
	}
	cmd := r.command(ctx, root, cwd, args...)
	stdout := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout, cmd.Stderr = stdout, io.Discard
	// A nil Stdin connects the child to /dev/null, never the login terminal.
	runErr := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err := r.save(root); err != nil {
			return nil, err
		}
	}
	if runErr != nil || ctx.Err() != nil {
		return nil, commandAuthError(stage, ctx.Err(), runErr)
	}
	if stdout.count > 1<<20 {
		return nil, &AuthError{stage, "invalid_response", 1}
	}
	return stdout.Bytes(), nil
}

func (r Runner) Check(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	version, err := r.probe(ctx, "models", "--version")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(version)) != CLIVersion {
		return nil, &AuthError{"models", "invalid_response", 1}
	}
	models, err := r.probe(ctx, "models", "models")
	if err != nil {
		return nil, err
	}
	available := map[string]bool{}
	for _, line := range strings.Split(string(models), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && config.IsAntigravityModel(fields[0]) {
			available[fields[0]] = true
		}
	}
	ordered := orderedModels(available)
	if len(ordered) == 0 {
		return nil, &AuthError{"models", "invalid_response", 1}
	}
	return ordered, nil
}

// Keep discovery and selection deterministic, and never expose unrecognized
// provider IDs or let CLI display ordering choose the verification model.
func orderedModels(available map[string]bool) []string {
	models := []string{}
	for _, model := range config.AntigravityModels() {
		if available[model] {
			models = append(models, model)
		}
	}
	return models
}

func modelSet(models []string) map[string]bool {
	available := map[string]bool{}
	for _, model := range models {
		if config.IsAntigravityModel(model) {
			available[model] = true
		}
	}
	return available
}
