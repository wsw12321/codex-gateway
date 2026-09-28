package antigravity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// AuthError contains only fixed, non-sensitive diagnostics. Raw CLI stderr,
// credential bytes and provider error strings must never enter these fields.
type AuthError struct {
	Stage    string
	Category string
	ExitCode int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("antigravity: stage=%s category=%s exit_code=%d", e.Stage, e.Category, e.ExitCode)
}

func credentialAuthError(stage string, err error) *AuthError {
	return &AuthError{stage, CredentialCategory(err), 1}
}

func commandAuthError(stage string, ctxErr, err error) *AuthError {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return &AuthError{stage, "timeout", 124}
	}
	if errors.Is(ctxErr, context.Canceled) {
		return &AuthError{stage, "canceled", 130}
	}
	code := 1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
	}
	return &AuthError{stage, "command_failed", code}
}

// AuthLogin runs only the interactive authorization and encrypted credential
// save. Verification deliberately happens in a different container/session.
func (r Runner) AuthLogin(ctx context.Context, stdin *os.File, stdout io.Writer) error {
	root, cwd, err := r.workspace()
	if err != nil {
		return &AuthError{"authorization", "io_failed", 1}
	}
	defer os.RemoveAll(root)
	// A first login has no stored credential; other restore failures are fatal.
	if err := r.credentials().Restore(ctx, filepath.Join(root, "home")); err != nil && !errors.Is(err, ErrCredentialsMissing) {
		return credentialAuthError("credential_restore", err)
	}
	cmd := r.command(ctx, root, cwd)
	cmd.Env = append(cmd.Env, "SSH_CONNECTION=127.0.0.1 1 127.0.0.1 22")
	if term := os.Getenv("TERM"); term != "" {
		cmd.Env = append(cmd.Env, "TERM="+term)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, io.Discard
	// Hand the real terminal to the isolated process group so reads cannot
	// receive SIGTTIN. Restore the caller's foreground group before returning.
	if stdin != nil {
		fd := int(stdin.Fd())
		if group, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP); err == nil {
			signal.Ignore(syscall.SIGTTOU)
			defer signal.Reset(syscall.SIGTTOU)
			defer func() { _ = unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, group) }()
			cmd.SysProcAttr.Foreground = true
			cmd.SysProcAttr.Ctty = fd
		}
	}
	runErr := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err := r.save(root); err != nil {
			return err
		}
	}
	if runErr != nil || ctx.Err() != nil {
		return commandAuthError("authorization", ctx.Err(), runErr)
	}
	return nil
}

// AuthVerify restores into fresh HOME directories before every stage. Its
// success is only one prerequisite for the later serving-container HTTP smoke.
func (r Runner) AuthVerify(ctx context.Context) error {
	models, err := r.Check(ctx)
	if err != nil {
		return err
	}
	usageCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	usage, probeErr := r.probe(usageCtx, "usage", "--print", "/usage", "--print-timeout", "30s")
	cancel()
	if probeErr != nil {
		return probeErr
	}
	if strings.TrimSpace(string(usage)) == "" {
		return &AuthError{"usage", "invalid_response", 1}
	}
	result, failure, diagnostic := r.run(ctx, models[0], "Reply with exactly OK.")
	if diagnostic != nil {
		return diagnostic
	}
	if ctx.Err() != nil {
		return commandAuthError("generation", ctx.Err(), nil)
	}
	if failure != nil || strings.TrimSpace(result.Response) == "" {
		return &AuthError{"generation", "invalid_response", 1}
	}
	return nil
}
