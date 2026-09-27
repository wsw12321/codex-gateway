//go:build linux

package antigravity

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Run authorization in a separate session with a real controlling PTY. A
// background process group would receive SIGTTIN when the synthetic CLI reads.
func TestAuthLoginControllingTerminal(t *testing.T) {
	for _, mode := range []string{"success", "failure", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			ptyNumber, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptyNumber), os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			dir := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "synthetic-agy")
			// Both values are test-created paths; quote shell metacharacters.
			quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
			shim := "#!/bin/sh\nexec " + quoted + " -test.run='^TestAuthTTYHelperProcess$' -- cli " + mode + "\n"
			if err := os.WriteFile(binary, []byte(shim), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestAuthTTYHelperProcess$", "--", "login", mode, binary, dir)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			cmd.Cancel = func() error {
				// The CLI has a distinct foreground process group. Stop both
				// groups if the test catches a regression that hangs on read.
				if group, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP); err == nil && group > 0 {
					_ = syscall.Kill(-group, syscall.SIGKILL)
				}
				return cmd.Process.Kill()
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = slave.Close()
			output := make(chan string, 1)
			go func() {
				data, _ := io.ReadAll(master)
				output <- string(data)
			}()
			if _, err := io.WriteString(master, "synthetic authorization\n"); err != nil {
				t.Fatal(err)
			}
			waitErr := cmd.Wait()
			var captured string
			select {
			case captured = <-output:
			case <-time.After(time.Second):
				_ = master.Close()
				t.Fatal("terminal remained open after authorization")
			}
			if waitErr != nil || !strings.Contains(captured, "AUTH_TTY_OK") {
				t.Fatalf("authorization did not finish with terminal restored: %v; %s", waitErr, captured)
			}
		})
	}
}

func TestAuthTTYHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	args := os.Args[separator+1:]
	if len(args) < 2 {
		t.Fatal("missing helper mode")
	}
	mode := args[1]
	if args[0] == "cli" {
		group, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
		if err != nil || group != syscall.Getpgrp() {
			t.Fatalf("CLI is not the terminal foreground group: %d, %v", group, err)
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil || line != "synthetic authorization\n" {
			t.Fatalf("interactive input unavailable: %q, %v", line, err)
		}
		path := filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "antigravity-oauth-token")
		if err := os.WriteFile(path, []byte(refreshedCredential), 0600); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "failure":
			os.Exit(7)
		case "timeout":
			time.Sleep(30 * time.Second)
		}
		return
	}
	if args[0] != "login" || len(args) != 4 {
		t.Fatal("invalid helper arguments")
	}
	credentials := &runnerCredentials{data: testCredential}
	runner := Runner{Binary: args[2], TempDir: args[3], Credentials: credentials}
	ctx := context.Background()
	if mode == "timeout" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Second)
		defer cancel()
	}
	err := runner.AuthLogin(ctx, os.Stdin, os.Stdout)
	if mode == "success" {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		var authErr *AuthError
		expectedCode := 7
		if mode == "timeout" {
			expectedCode = 124
		}
		if !errors.As(err, &authErr) || authErr.ExitCode != expectedCode {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
	group, groupErr := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	if groupErr != nil || group != syscall.Getpgrp() {
		t.Fatalf("caller foreground group was not restored: %d, %v", group, groupErr)
	}
	if credentials.data != refreshedCredential || credentials.saves != 1 {
		t.Fatal("interactive credential refresh was not saved")
	}
	fmt.Fprintln(os.Stdout, "AUTH_TTY_OK")
}
