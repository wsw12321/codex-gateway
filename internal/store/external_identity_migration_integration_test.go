//go:build integration

package store

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestExternalIdentityMigrationPreservesLegacyPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	configuration, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*configuration)
	defer admin.Close()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "external_identity_migration_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop migration schema: %v", err)
		}
	}()
	configuration.RuntimeParams["search_path"] = schema
	s := New(stdlib.OpenDB(*configuration))
	defer s.Close()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var upgrade Migration
	for _, migration := range migrations {
		if migration.Name == "0028_external_identities.sql" {
			upgrade = migration
			break
		}
		if _, err := s.db.ExecContext(ctx, migration.SQL); err != nil {
			t.Fatalf("apply legacy %s: %v", migration.Name, err)
		}
	}
	if upgrade.Name == "" {
		t.Fatal("external identity migration missing")
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	user, _, key := billingIntegrationPrincipal(t, ctx, s, "legacy-external")
	legacy := externalIdentityTestSession(t, user.ID, at)
	var sessionID string
	if err := s.db.QueryRowContext(ctx, `INSERT INTO sessions(user_id,token_hash,csrf_secret,created_at,last_seen_at,idle_expires_at,absolute_expires_at,recently_verified_at)
		VALUES($1,$2,$3,$4,$4,$5,$6,$4) RETURNING id`, user.ID, legacy.TokenHash, legacy.CSRFSecret, at, legacy.IdleExpiresAt, legacy.AbsoluteExpiresAt).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	password := strings.Repeat("p", 80)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO password_credentials(user_id,encoded_hash) VALUES($1,$2)`, user.ID, password); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts SET balance_usd=12.5 WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, upgrade.SQL); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetActiveSession(ctx, legacy.TokenHash, at)
	if err != nil || got.ID != sessionID || got.UserID != user.ID || got.ExternalIdentityID != nil ||
		!bytes.Equal(got.CSRFSecret, legacy.CSRFSecret) || !got.IdleExpiresAt.Equal(legacy.IdleExpiresAt) ||
		!got.AbsoluteExpiresAt.Equal(legacy.AbsoluteExpiresAt) || got.RecentlyVerifiedAt == nil || !got.RecentlyVerifiedAt.Equal(at) {
		t.Fatalf("migration changed legacy session: %+v %v", got, err)
	}
	if got, err := s.GetPasswordCredential(ctx, user.ID); err != nil || got.EncodedHash != password {
		t.Fatalf("migration changed password: %v", err)
	}
	if got, err := s.LookupAPIKey(ctx, key.PublicID); err != nil || got.ID != key.ID || got.UserID != user.ID || !bytes.Equal(got.KeyHash, key.KeyHash) {
		t.Fatalf("migration changed API key: %v", err)
	}
	var balance string
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&balance); err != nil || balance != "12.500000000000" {
		t.Fatalf("migration changed balance: %q %v", balance, err)
	}
	if current, err := s.CreateSession(ctx, externalIdentityTestSession(t, user.ID, at)); err != nil || current.ExternalIdentityID != nil {
		t.Fatalf("new local login acquired external provenance: %+v %v", current, err)
	}
}
