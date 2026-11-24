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
- `internal/store/migrations/0036_billing_model_scopes.sql` — adds the Owner's
  global cash model scope and per-group model scopes. Existing scopes are
  initialized from the current manageable catalog at gateway startup.
- `internal/store/migrations/0038_cpa_v8_0_48_model_catalog.sql` — one-time CPA
  v8.0.48 catalog cutover. Retains `gpt-5.5`, which upstream removed from every
  Codex catalog, as inactive history; adds no model. Snapshots defaults/user
  permissions/multipliers and leaves keys, balances, usage, ledger rows,
  subscription scopes, and admission snapshots untouched. The conditional
  rollback in `scripts/rollback-cpa-model-config.sql` applies only the newest
  cutover table present.
- `internal/store/migrations/0040_site_settings.sql` — Owner-edited community
  (QQ group) and manual recharge instructions, plus fixed-slot PNG/JPEG images
  re-encoded by the gateway. Both sections start disabled. No payment is
  verified or credited by this table.
- `internal/store/migrations/0041_upstream_account_alias.sql` — Owner-chosen
  upstream account aliases shown beside the synchronized name or masked email.
  Empty for existing accounts; synchronization never overwrites them, and the
  constraint rejects `@` so a full email cannot be stored as an alias.
- `internal/store/migrations/0042_group_account_usage_visibility.sql` — Owner
  opt-in per group that lets members read the five-hour and weekly usage of
  the accounts in the group's selected Codex and Claude pools. Every existing
  group starts disabled; "all accounts" pools never expose accounts.
- `internal/store/migrations/0043_billing_reservation_layers.sql` — funding
  rule 4: admission snapshots every usable funding source as an ordered layer
  (groups by priority, then personal) with its account pool, and settlement
  charges the first layer whose pool contains the account that served the
  request. Rules 1-3 settle unchanged.
- `internal/store/migrations/0044_upstream_quota_guard.sql` — Gateway-owned
  quota guard: a global safety percentage (default 5) and, per Codex or Claude
  account, the last five-hour and weekly reading, the limited state with its
  restore time (reset + 5 minutes) and the automatic check schedule. Accounts
  start without a row and are therefore available.
- `internal/store/migrations/0045_ss2022_egress_cipher.sql` — widens the
  account egress node cipher check to accept `2022-blake3-aes-128-gcm`.
  Existing nodes are unchanged. Gateway and `account-egress` validate the
  Base64 pre-shared key format before a node reaches Mihomo.
- `internal/store/migrations/0046_user_model_routes.sql` — per-user model
  routes. A request for `source_model` is admitted, priced, funded and
  forwarded as `target_model`; `usage_requests.requested_model` keeps the
  client's model. Existing rows are unchanged.
- `internal/store/migrations/0047_protocol_conversion_settings.sql` — Owner
  switch for protocol conversion, off by default. While off, `claude-*` models
  are accepted only on native `/v1/messages`; Claude through OpenAI Responses
  (including model routes to a Claude target) is rejected before admission.
- `internal/store/migrations/0048_socks5_egress.sql` — account egress nodes
  gain `protocol` (`ss` or `socks5`) and `username`. A SOCKS5 node has no
  cipher and either no authentication (empty username, no password) or a
  username with an encrypted password. Existing nodes stay `ss` and are
  unchanged.
- `internal/store/migrations/0049_model_family_scopes.sql` — billing model
  scopes (subscriptions, entitlements, plans, groups, cash) may name a whole
  family (`family:gpt`, `family:gemini`, `family:claude`), which also covers
  models priced later. One-time conversion: a row whose scope already names
  every active model of a family, in the catalog the previous binary
  synchronized, gains that family's token and keeps its exact models. Converted
  plans get a new version, converted subscriptions a new configuration version;
  plan snapshots and ledger rows are unchanged. One
  `billing.model_families_converted` audit event records the counts.

- `internal/store/migrations/0050_model_suppliers.sql` — API Key model
  suppliers. `upstream_accounts.credential_kind` distinguishes OAuth accounts
  from suppliers; `model_suppliers` stores the encrypted key, Base URL and
  accepted models. Existing accounts stay `oauth`.
- `internal/store/migrations/0051_tenant_upstream_ownership.sql` — upstream
  account ownership for the aggregation gateway. `owner_user_id` NULL is a
  platform account (every existing account); a tenant account can never join a
  group pool, an exclusive grant or a per-user account scope (enforced by
  triggers), and its owner is set once. Adds `admin_suspended`,
  `egress_registrations.tenant_user_id` and the `marketplace_settings`
  singleton, disabled by default. Platform selection paths ignore tenant
  accounts; while the marketplace is enabled, accounts unknown to the Gateway
  are not eligible.
- `internal/store/migrations/0052_tenant_provider_lifecycle.sql` — tenant
  providers (aggregation gateway stage 2). `credential_removed_at` marks an
  account whose credential was deleted: it is released, no funding path uses
  it, and the next login of it claims it (suppliers deleted earlier are
  backfilled). Ownership is no longer write-once: the replaced
  `guard_upstream_account_owner` trigger accepts a change only when the
  transaction names it a `claim` or an Owner `transfer`, still keeps tenant
  accounts out of platform-only structures, and refuses an owner whose egress
  node differs. `egress_servers.owner_user_id` lets tenants run their own
  nodes (immutable); triggers bind accounts and registrations only to nodes of
  the same owner. Adds `marketplace_settings.max_egress_servers_per_user`
  (default 5).
- `internal/store/migrations/0053_plan_provider_scopes.sql` — plan provider
  scopes, funding rule 5 and self-use (aggregation gateway stage 3).
  `billing_plans.seller_user_id` (immutable; NULL is a platform plan),
  `billing_plan_provider_scopes` (`all`/`selected`, every existing plan
  `all`) and `billing_plan_provider_accounts` (a trigger accepts only
  accounts of the plan's seller that still hold a credential).
  `billing_subscriptions.source_plan_id` is the plan a subscription was bought
  from, backfilled from `plan_id` and immutable. `billing_accounts.
  self_use_enabled` (default true). Reservations accept funding rule 5 and a
  layered reservation without personal sources; a rule 5 settlement may record
  a positive cost with nothing charged and nothing uncovered, which is exactly
  a request served by the user's own account (self-use). `billing_reservation_layers` gains `kind`
  (`group`, `subscription`, `self`, `personal`; existing rows backfilled) and
  the bound subscription period of a restricted subscription layer.

Run migrations through `store.Migrate`; do not execute copied, out-of-band SQL.
The runner verifies the SHA-256 checksum of every migration that has already
been applied.
