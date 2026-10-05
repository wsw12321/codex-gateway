//go:build integration

package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestExternalRegistrationOriginalDeadlinePostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	var clock atomic.Int64
	clock.Store(at.UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	params := externalRegistrationParams(t, "registration-deadline", at)
	// The authorization began well before identity verification finished.
	// Its deadline arrives while the identity proof is still recent.
	params.ExpiresAt = at.Add(time.Minute)
	before := externalUserCount(t, ctx, s)
	blocker, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `LOCK TABLE sessions IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := s.CompleteExternalRegistration(ctx, params)
		result <- err
	}()
	billingSourcePreferenceWaitForLock(t, ctx, s, pid, 1)
	clock.Store(params.ExpiresAt.UnixNano())
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired registration was allowed to commit: %v", err)
	}
	if got := externalUserCount(t, ctx, s); got != before {
		t.Fatalf("expired registration left account: %d want %d", got, before)
	}
	var remnants int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM external_identities WHERE issuer=$1 AND subject=$2) +
		(SELECT count(*) FROM sessions WHERE token_hash=$3)`, params.Issuer, params.Subject, params.Session.TokenHash).Scan(&remnants); err != nil || remnants != 0 {
		t.Fatalf("expired registration left artifacts: %d %v", remnants, err)
	}
	// The same deadline is rejected before any writes on a fresh attempt.
	if _, _, err := s.CompleteExternalRegistration(ctx, params); !errors.Is(err, ErrNotFound) {
		t.Fatalf("already-expired registration succeeded: %v", err)
	}
}
