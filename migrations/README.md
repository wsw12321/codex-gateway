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
- `internal/store/migrations/0007_upstream_accounts.sql` — upstream account
  metadata and historical usage attribution.
- `internal/store/migrations/0008_model_access.sql` — model permissions and
  registration defaults.
- `internal/store/migrations/0009_upstream_allocation.sql` — account allocation
  weights and admission bindings.
- `internal/store/migrations/0010_billing_source_preferences.sql` — persistent
  user preferences to disable subscription and cash sources for new requests.
- `internal/store/migrations/0015_degraded_usage_state.sql` — permits terminal
  usage requests marked `degraded` when the upstream selected a different model.
- `internal/store/migrations/0031_group_period_limits.sql` — finite or unlimited
  group cycles; existing groups retain their saved period and usage as cycle 1/1.
- `internal/store/migrations/0034_multi_subscriptions.sql` — independent
  subscriptions, purchased entitlement segments, explicit model scopes,
  personal spending priorities and versioned admission-time funding snapshots.
  Detaches legacy plans once; requires model initialization after catalog sync
  and before serving. See the multi-subscription upgrade procedure in
  `docs/operations.md`.
- `internal/store/migrations/0035_cpa_v8_0_30_model_catalog.sql` — one-time CPA
  v8.0.30 catalog cutover. Retains five removed model IDs as inactive history,
  activates `claude-haiku-5-5` and `gemini-4-argon-high`, snapshots
  defaults/user permissions/multipliers,
  and leaves keys, balances, usage, ledger rows, and admission snapshots
  untouched. Conditional rollback is in `scripts/rollback-cpa-model-config.sql`.

Run migrations through `store.Migrate`; do not execute copied, out-of-band SQL.
The runner verifies the SHA-256 checksum of every migration that has already
been applied.
