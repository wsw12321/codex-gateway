# Database migrations

The executable migrations are embedded in the gateway binary from
`internal/store/migrations`. Keeping the canonical SQL beside the Go package
allows `go:embed` to include it without filesystem traversal.

Current migrations:

- `internal/store/migrations/0001_initial.sql` — PostgreSQL 17 initial schema.
- `internal/store/migrations/0002_billing.sql` — billing accounts, subscriptions,
  reservations, and immutable ledger entries.
- `internal/store/migrations/0003_password_credentials.sql` — Argon2id password
  credentials and recovery support.
- `internal/store/migrations/0004_subscription_period_limits.sql` — finite
  subscription period limits.
- `internal/store/migrations/0005_official_token_pricing.sql` — versioned pricing
  snapshots and ledger metadata.
- `internal/store/migrations/0006_api_key_lifecycle.sql` — encrypted API Key
  secrets, active credential lifecycle, and non-secret historical references.

Run migrations through `store.Migrate`; do not execute copied, out-of-band SQL.
The runner verifies the SHA-256 checksum of every migration that has already
been applied.
