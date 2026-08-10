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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/maintenance"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/server"
	"github.com/wsw/codex-gateway/internal/store"
)

var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("gateway stopped", "error", security.RedactText(err.Error()))
		os.Exit(1)
	}
}

func run(arguments []string, logger *slog.Logger) error {
	command := "serve"
	if len(arguments) > 0 {
		command = arguments[0]
	}
	if command == "version" || command == "--version" {
		fmt.Printf("codex-gateway %s (%s)\n", version, revision)
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	repository, err := store.Open(ctx, store.Config{
		DriverName: "pgx", DSN: cfg.DatabaseURL, MaxOpenConns: 20, MaxIdleConns: 5,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	})
	if err != nil {
		return err
	}
	defer repository.Close()

	switch command {
	case "serve":
		return serve(ctx, cfg, repository, logger)
	case "migrate":
		return repository.Migrate(ctx)
	case "bootstrap-owner":
		if err := repository.Migrate(ctx); err != nil {
			return err
		}
		return bootstrapOwner(ctx, cfg, repository)
	case "maintenance":
		if err := repository.Migrate(ctx); err != nil {
			return err
		}
		maintenance.Runner{Store: repository, Logger: logger, Timezone: "UTC"}.RunOnce(ctx)
		return nil
	default:
		return fmt.Errorf("unknown command %q (expected serve, migrate, bootstrap-owner, maintenance, or version)", command)
	}
}

func serve(ctx context.Context, cfg config.Config, repository *store.Store, logger *slog.Logger) error {
	migrationContext, cancelMigration := context.WithTimeout(ctx, 2*time.Minute)
	if err := repository.Migrate(migrationContext); err != nil {
		cancelMigration()
		return fmt.Errorf("migrate database: %w", err)
	}
	cancelMigration()

	handler, err := server.New(cfg, repository, logger)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr: cfg.ListenAddress, Handler: handler.Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}
	maintenanceContext, cancelMaintenance := context.WithCancel(ctx)
	defer cancelMaintenance()
	go maintenance.Runner{Store: repository, Logger: logger, Timezone: "UTC"}.Run(maintenanceContext)

	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "address", cfg.ListenAddress, "version", version, "revision", revision)
		serveErrors <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("graceful HTTP shutdown: %w", err)
		}
		err := <-serveErrors
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func bootstrapOwner(ctx context.Context, cfg config.Config, repository *store.Store) error {
	var unavailable bool
	err := repository.DB().QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM users WHERE role = 'owner' AND status = 'active'
			UNION ALL
			SELECT 1 FROM invitations
			WHERE kind = 'owner_bootstrap' AND used_at IS NULL
			  AND revoked_at IS NULL AND expires_at > now()
		)`).Scan(&unavailable)
	if err != nil {
		return fmt.Errorf("check owner bootstrap state: %w", err)
	}
	if unavailable {
		return errors.New("an active owner or unexpired owner bootstrap invitation already exists")
	}
	generated, err := security.GenerateOpaqueToken(security.InvitationToken)
	if err != nil {
		return err
	}
	digest, err := security.PepperTokenDigest(cfg.TokenPepper, generated.Digest)
	if err != nil {
		return err
	}
	invitation, err := repository.CreateInvitation(ctx, store.CreateInvitationParams{
		Kind: store.InvitationOwnerBootstrap, TokenHash: digest[:],
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		return err
	}
	link := strings.TrimRight(cfg.PublicURL.String(), "/") + "/join#token=" + generated.Token
	// This is the sole intentional plaintext-token output. It goes to the
	// invoking SSH terminal, never through structured application logging.
	fmt.Printf("Owner 初始化链接（仅显示一次，%s 到期）：\n%s\n", invitation.ExpiresAt.Format(time.RFC3339), link)
	return nil
}
