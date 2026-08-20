// Package store contains the PostgreSQL persistence layer for the gateway.
// It deliberately accepts and returns metadata only; request and response
// bodies must never cross this package boundary.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNotFound              = errors.New("store: not found")
	ErrConflict              = errors.New("store: conflict")
	ErrInvalid               = errors.New("store: invalid input")
	ErrInvitationUnavailable = errors.New("store: invitation unavailable")
	ErrQuotaExceeded         = errors.New("store: quota exceeded")
	ErrAPIKeyExpired         = errors.New("store: API key expired")
)

// Config controls the database/sql pool. DriverName defaults to pgx. The
// executable is responsible for importing github.com/jackc/pgx/v5/stdlib.
type Config struct {
	DriverName      string
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// Store owns a database/sql pool. New may be used when the caller owns the
// pool (for example in integration tests); Close always closes that pool.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, fmt.Errorf("%w: database DSN is empty", ErrInvalid)
	}
	driverName := cfg.DriverName
	if driverName == "" {
		driverName = "pgx"
	}
	db, err := sql.Open(driverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return New(db), nil
}

func New(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: nil database", ErrInvalid)
	}
	return s.db.PingContext(ctx)
}

func (s *Store) withTx(ctx context.Context, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// newUUID returns an RFC 4122 version 4 UUID without adding a UUID package to
// the persistence layer.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:]), nil
}

func randomBytes(size int) ([]byte, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: random byte size must be positive", ErrInvalid)
	}
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return nil, fmt.Errorf("generate random bytes: %w", err)
	}
	return value, nil
}

type sqlStateError interface {
	SQLState() string
}

func mapDBError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, ErrNotFound)
	}
	var state sqlStateError
	if errors.As(err, &state) {
		switch state.SQLState() {
		case "23505": // unique_violation
			return fmt.Errorf("%s: %w", operation, ErrConflict)
		case "23502", "23503", "23514", "22P02", "22001", "22003":
			return fmt.Errorf("%s: %w", operation, ErrInvalid)
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func valueOrNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func timeOrNil(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}
