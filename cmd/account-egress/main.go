// account-egress supervises one pinned Mihomo and exposes only the private
// Gateway control API and a Squid-only CONNECT parent. It never dials an origin
// directly and never logs configuration, proxy credentials, or provider data.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	dataAddress       = "172.28.60.3:17891"
	controlAddress    = "172.28.70.3:17892"
	controllerAddress = "127.0.0.1:19090"
	controlKeyPath    = "/run/secrets/egress_control_key"
	workDirectory     = "/run/account-egress"
	bootstrapPath     = "/run/account-egress/bootstrap.json"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
		resp, err := client.Get("http://" + controlAddress + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "account-egress: unsupported command")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "account-egress:", err)
		os.Exit(1)
	}
}

func randomID() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("random source unavailable")
	}
	return hex.EncodeToString(raw[:])
}

func readControlKey(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- production caller uses only the fixed secret mount; flags and fstat enforce the file boundary.
	if err != nil {
		return "", errors.New("cannot open control credential")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0640 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return "", errors.New("control credential must be a regular mode 0640 file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Gid) != os.Getegid() {
		return "", errors.New("control credential group mismatch")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 513))
	if err != nil || len(raw) > 512 {
		return "", errors.New("invalid control credential")
	}
	key := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(key) < 32 || strings.ContainsAny(key, "\x00\r\n \t") {
		return "", errors.New("invalid control credential")
	}
	return key, nil
}

func run(ctx context.Context) error {
	key, err := readControlKey(controlKeyPath)
	if err != nil {
		return err
	}
	controller := newController("http://"+controllerAddress, randomID())
	initial, err := marshalMihomo(nil, controller.secret)
	if err != nil {
		return errors.New("cannot prepare local proxy")
	}
	if err := os.WriteFile(bootstrapPath, initial, 0600); err != nil {
		return errors.New("cannot prepare local proxy")
	}
	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	child := exec.CommandContext(childCtx, "/usr/local/bin/mihomo", "-d", workDirectory, "-f", bootstrapPath)
	// Mihomo's parse errors can include supplied values. Keep them out of logs.
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if err := child.Start(); err != nil {
		return errors.New("cannot start local proxy")
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
ready:
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			return errors.New("local proxy exited")
		case <-deadline.C:
			return errors.New("local proxy did not become ready")
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			err := controller.health(probeCtx)
			cancel()
			if err == nil {
				break ready
			}
		}
	}
	runtime := newRuntime(key, controller)
	dataListener, err := net.Listen("tcp", dataAddress)
	if err != nil {
		return errors.New("cannot listen on private data interface")
	}
	defer dataListener.Close()
	controlListener, err := net.Listen("tcp", controlAddress)
	if err != nil {
		return errors.New("cannot listen on private control interface")
	}
	defer controlListener.Close()
	dataServer := &http.Server{Handler: runtime, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	controlServer := &http.Server{Handler: runtime.controlHandler(), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	serverErrors := make(chan error, 2)
	go func() { serverErrors <- dataServer.Serve(dataListener) }()
	go func() { serverErrors <- controlServer.Serve(controlListener) }()
	go runtime.pruneLoop(ctx)
	defer dataServer.Close()
	defer controlServer.Close()
	select {
	case <-ctx.Done():
		return nil
	case <-done:
		return errors.New("local proxy exited")
	case <-serverErrors:
		return errors.New("private HTTP server stopped")
	}
}
