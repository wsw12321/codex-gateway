package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationLockID int64 = 0x4347584d494752 // "CGXMIGR"

type Migration struct {
	Name     string
	Checksum [sha256.Size]byte
	SQL      string
}

// EmbeddedMigrations returns a defensive copy of the ordered migration set.
// It is exported so deployment tooling can report exactly what the binary
// contains without reading the source tree.
func EmbeddedMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	result := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return nil, fmt.Errorf("migration %s: %w: empty SQL", entry.Name(), ErrInvalid)
		}
		result = append(result, Migration{
			Name:     entry.Name(),
			Checksum: sha256.Sum256(body),
			SQL:      string(body),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: no embedded migrations", ErrInvalid)
	}
	return result, nil
}

// Migrate applies all embedded migrations. Each file is atomic and protected
// by a PostgreSQL transaction-scoped advisory lock. Previously applied files
// are checksum-verified, making accidental migration edits fail closed.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := EmbeddedMigrations()
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			checksum BYTEA NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	known := make(map[string]struct{}, len(migrations))
	for _, migration := range migrations {
		known[migration.Name] = struct{}{}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan migration ledger: %w", err)
		}
		if _, ok := known[name]; !ok {
			_ = rows.Close()
			return fmt.Errorf("database contains unknown migration %q; refusing binary downgrade", name)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close migration ledger: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration ledger: %w", err)
	}

	for _, migration := range migrations {
		migration := migration
		err := s.withTx(ctx, &sql.TxOptions{}, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
				return fmt.Errorf("lock migrations: %w", err)
			}
			var checksum []byte
			err := tx.QueryRowContext(ctx,
				`SELECT checksum FROM schema_migrations WHERE name = $1`, migration.Name,
			).Scan(&checksum)
			switch {
			case err == nil:
				if !bytes.Equal(checksum, migration.Checksum[:]) {
					return fmt.Errorf("migration %s checksum mismatch", migration.Name)
				}
				return nil
			case !isNoRows(err):
				return fmt.Errorf("check migration %s: %w", migration.Name, err)
			}
			if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
				return fmt.Errorf("apply migration %s: %w", migration.Name, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (name, checksum, applied_at) VALUES ($1, $2, $3)`,
				migration.Name, migration.Checksum[:], time.Now().UTC(),
			); err != nil {
				return fmt.Errorf("record migration %s: %w", migration.Name, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func isNoRows(err error) bool { return err == sql.ErrNoRows }
