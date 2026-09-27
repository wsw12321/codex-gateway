package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wsw/codex-gateway/internal/antigravity"
	"github.com/wsw/codex-gateway/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		var authErr *antigravity.AuthError
		if errors.As(err, &authErr) {
			fmt.Fprintln(os.Stderr, authErr)
			os.Exit(authErr.ExitCode)
		}
		logger.Error("bridge stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	binary := os.Getenv("AGY_BINARY")
	if binary == "" {
		binary = "/usr/local/bin/agy"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	runner := antigravity.Runner{Binary: binary, Timeout: 5 * time.Minute, Logger: logger}
	registryPath := os.Getenv("ANTIGRAVITY_ACCOUNT_REGISTRY_PATH")
	if registryPath == "" {
		registryPath = antigravity.DefaultAccountRegistryPath
	}
	if len(os.Args) > 1 {
		if len(os.Args) > 3 {
			return &antigravity.AuthError{Stage: "authorization", Category: "configuration", ExitCode: 2}
		}
		name := "default"
		if len(os.Args) == 3 {
			name = os.Args[2]
		}
		if !antigravity.ValidAccountName(name) || (os.Args[1] != "auth-login" && os.Args[1] != "auth-verify") {
			return &antigravity.AuthError{Stage: "authorization", Category: "configuration", ExitCode: 2}
		}
		registry, err := antigravity.OpenAccountRegistry(ctx, registryPath, runner)
		if err != nil {
			return &antigravity.AuthError{Stage: "credential_restore", Category: antigravity.CredentialCategory(err), ExitCode: 1}
		}
		runner.Credentials = antigravity.AccountCredentials(name)
		switch os.Args[1] {
		case "auth-login":
			if err := runner.AuthLogin(ctx, os.Stdin, os.Stdout); err != nil {
				return err
			}
			if err := registry.RegisterLogin(ctx, name, runner); err != nil {
				return &antigravity.AuthError{Stage: "credential_save", Category: antigravity.CredentialCategory(err), ExitCode: 1}
			}
			return nil
		case "auth-verify":
			return runner.AuthVerify(ctx)
		default:
			return &antigravity.AuthError{Stage: "authorization", Category: "configuration", ExitCode: 2}
		}
	}
	path := os.Getenv("ANTIGRAVITY_BRIDGE_API_KEY_FILE")
	if path == "" {
		return errors.New("ANTIGRAVITY_BRIDGE_API_KEY_FILE is required")
	}
	// #nosec G304 -- The deployment operator supplies the mounted secret path at startup.
	secret, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read bridge API key file")
	}
	token := strings.TrimSpace(string(secret))
	if len(token) < 32 {
		return errors.New("bridge API key must contain at least 32 bytes")
	}
	routes, err := config.ParseAntigravityModelRoutes(os.Getenv("ANTIGRAVITY_MODEL_ROUTES_JSON"))
	if err != nil {
		return err
	}
	for public, cli := range routes {
		if public != antigravity.PublicModel || cli != antigravity.CLIModel {
			return errors.New("unsupported Antigravity model mapping")
		}
	}
	address := os.Getenv("ANTIGRAVITY_BRIDGE_LISTEN")
	if address == "" {
		address = ":8318"
	}
	registry, err := antigravity.OpenAccountRegistry(ctx, registryPath, runner)
	if err != nil {
		return &antigravity.AuthError{Stage: "credential_restore", Category: antigravity.CredentialCategory(err), ExitCode: 1}
	}
	handler, err := antigravity.NewAccountServer(runner, registry, token, os.Getenv("ANTIGRAVITY_GATEWAY_URL"))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 6 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: nil}
	// Bind request lifetimes to shutdown as well as client cancellation.
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCtx, stop := context.WithCancel(r.Context())
		stopShutdown := context.AfterFunc(ctx, stop)
		defer stopShutdown()
		defer stop()
		handler.ServeHTTP(w, r.WithContext(requestCtx))
	})
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		handler.Refresh(ctx)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				handler.Refresh(ctx)
			}
		}
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
		defer stop()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("Antigravity bridge listening", "address", address, "cli_version", antigravity.CLIVersion)
	err = server.ListenAndServe()
	// Wait for active requests and health checks to save refreshed credentials
	// after cancellation; ListenAndServe returns before Shutdown finishes.
	cancel()
	<-shutdownDone
	<-refreshDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
