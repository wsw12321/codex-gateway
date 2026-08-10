# Database migrations

The executable migrations are embedded in the gateway binary from
`internal/store/migrations`. Keeping the canonical SQL beside the Go package
allows `go:embed` to include it without filesystem traversal.

Current migration:

- `internal/store/migrations/0001_initial.sql` — PostgreSQL 17 initial schema.

Run migrations through `store.Migrate`; do not execute copied, out-of-band SQL.
The runner verifies the SHA-256 checksum of every migration that has already
been applied.
