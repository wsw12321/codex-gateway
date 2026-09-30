package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wsw/codex-gateway/internal/cpamigrate"
)

func main() {
	var o cpamigrate.Options
	flag.StringVar(&o.Direction, "direction", "forward", "forward or reverse")
	flag.StringVar(&o.Registry, "registry", "/var/lib/antigravity/keyrings/accounts.json", "legacy account registry")
	flag.StringVar(&o.OAuthDir, "oauth-dir", "/oauth", "private CPA credential directory")
	flag.StringVar(&o.TempDir, "temp-dir", "/run/cpa-migrate/staging", "UID 10002 private tmpfs staging directory")
	flag.StringVar(&o.ProxyURL, "egress-proxy", "", "required explicit HTTP egress proxy URL")
	flag.StringVar(&o.OAuthClientFile, "google-oauth-client-file", "/run/secrets/cpa_migration_google_oauth", "private JSON file containing the CPA Google OAuth client_id and client_secret")
	flag.StringVar(&o.Account, "account", "", "optional legacy account name")
	helper := flag.String("keyring-operation", "", "internal UID 10002 helper")
	home := flag.String("staging-home", "", "internal private staging path")
	flag.Parse()
	if flag.NArg() != 0 {
		fail("arguments_invalid")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 30*time.Minute)
	defer timeout()
	if *helper != "" {
		if err := cpamigrate.KeyringOperation(ctx, *helper, o.Account, *home); err != nil {
			fail(err.Error())
		}
		return
	}
	reports, err := cpamigrate.Run(ctx, o)
	if err != nil {
		fail(err.Error())
	}
	if err := json.NewEncoder(os.Stdout).Encode(reports); err != nil {
		fail("report_write_failed")
	}
	for _, report := range reports {
		if report.Status == "quarantined" {
			os.Exit(2)
		}
	}
}

func fail(category string) { fmt.Fprintln(os.Stderr, "cpa-credentials-migrate:", category); os.Exit(1) }
