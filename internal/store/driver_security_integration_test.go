//go:build integration

package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestSimpleProtocolDollarQuotedParametersPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	registeredDSN := stdlib.RegisterConnConfig(config)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(registeredDSN) })

	s, err := Open(ctx, Config{DSN: registeredDSN, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, tc := range []struct {
		name      string
		delimiter string
	}{
		{name: "untagged", delimiter: "$$"},
		{name: "tagged", delimiter: "$gateway_literal$"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// GO-2026-5004: simple protocol must only substitute real parameters.
			// Substituting $1 inside this literal lets the argument close the
			// dollar quote and execute chr(88) as SQL instead of preserving data.
			const wantLiteral = "literal:$1"
			query := "SELECT " + tc.delimiter + wantLiteral + tc.delimiter + "::text, $1::text"
			argument := tc.delimiter + "::text || chr(88) || " + tc.delimiter
			var literal, value string
			if err := s.DB().QueryRowContext(ctx, query, argument).Scan(&literal, &value); err != nil {
				t.Fatalf("query dollar-quoted literal and parameter: %v", err)
			}
			if literal != wantLiteral {
				t.Errorf("dollar-quoted literal = %q, want %q", literal, wantLiteral)
			}
			if value != argument {
				t.Errorf("bound parameter = %q, want %q", value, argument)
			}
		})
	}
}
