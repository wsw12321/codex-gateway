package cpamigrate

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wsw/codex-gateway/internal/antigravity"
)

type processKeyring struct{ executable, proxy string }

func (p processKeyring) invoke(ctx context.Context, action, account, home string) error {
	cmd := exec.CommandContext(ctx, "/usr/local/bin/cpa-keyring-helper", p.executable, action, account, home)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	// The legacy CLI verification must use the same explicit egress proxy as
	// the Go provider checks; inherited NO_PROXY cannot create a bypass.
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		cmd.Env = append(cmd.Env, key+"="+p.proxy)
	}
	cmd.Env = append(cmd.Env, "NO_PROXY=", "no_proxy=")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 10002, Gid: 10002}, Setpgid: true}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return errors.New("keyring_helper_failed")
	}
	return nil
}
func (p processKeyring) Restore(ctx context.Context, account, home string) error {
	return p.invoke(ctx, "restore", account, home)
}
func (p processKeyring) Save(ctx context.Context, account, home string) error {
	return p.invoke(ctx, "save", account, home)
}
func (p processKeyring) Verify(ctx context.Context, account, temp string) error {
	return p.invoke(ctx, "verify", account, temp)
}

// KeyringOperation runs in a UID 10002 child of the maintenance process. Only
// paths and account names cross argv; credentials stay in private tmpfs files
// and Secret Service pipes. Raw helper output is never forwarded to the user.
func KeyringOperation(ctx context.Context, action, account, home string) error {
	if os.Geteuid() != 10002 || !antigravity.ValidAccountName(account) || !filepath.IsAbs(home) {
		return errors.New("keyring_helper_invalid")
	}
	d, err := openDirectory(home, 10002)
	if err != nil {
		return err
	}
	defer d.close()
	keys := antigravity.AccountCredentials(account)
	switch action {
	case "restore":
		return keys.Restore(ctx, home)
	case "save":
		return keys.Save(ctx, home)
	case "verify":
		return (antigravity.Runner{Binary: "/usr/local/bin/agy", TempDir: home, Credentials: keys}).AuthVerify(ctx)
	default:
		return errors.New("keyring_operation_invalid")
	}
}
