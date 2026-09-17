package main

import (
	"context"
	"errors"
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
		logger.Error("bridge stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	path := os.Getenv("ANTIGRAVITY_BRIDGE_API_KEY_FILE")
	if path == "" {
		return errors.New("ANTIGRAVITY_BRIDGE_API_KEY_FILE is required")
	}
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
	binary := os.Getenv("AGY_BINARY")
	if binary == "" {
		binary = "/usr/local/bin/agy"
	}
	address := os.Getenv("ANTIGRAVITY_BRIDGE_LISTEN")
	if address == "" {
		address = ":8318"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	handler := antigravity.NewServer(antigravity.Runner{Binary: binary, Timeout: 5 * time.Minute, Logger: logger}, token)
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 6 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: nil}
	// Bind request lifetimes to shutdown as well as client cancellation.
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCtx, stop := context.WithCancel(r.Context())
		stopShutdown := context.AfterFunc(ctx, stop)
		defer stopShutdown()
		defer stop()
		handler.ServeHTTP(w, r.WithContext(requestCtx))
	})
	go func() {
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
	go func() {
		<-ctx.Done()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("Antigravity bridge listening", "address", address, "cli_version", antigravity.CLIVersion)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
