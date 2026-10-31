# CPA v8.0.48 model-catalog audit (preparation only)

**Audit target:** Gateway commit `8ffae3a` (`main`) against CPA `v8.0.48`
(`dbb706215a3afd2f0ea3c96482d4233856466caa`). **Decision: blocked; no
pricing, permission, source-lock, Compose, migration, or credential change is
authorized by this audit.** The machine-readable record is
[`deploy/codex-compat/cpa-v8.0.48-model-catalog-audit.json`](../deploy/codex-compat/cpa-v8.0.48-model-catalog-audit.json).

## Inputs and reproducibility

| catalog | source | SHA-256 |
| --- | --- | --- |
| target | `internal/registry/models/models.json` from CPA tag `v8.0.48` / `dbb70621` | `32f663de911083339cf5347be74b0b4cbf1471bc1968362897cc94ebf021c306` |
| current | `internal/registry/models/models.json` from pinned CPA `v8.0.30` / `5be2cdab` | `2a4bd5fd9a72ef5c42c746274a5eec13c65c7e0768b6f1d510e8e9a8994c76c8` |

The target catalog was fetched from the upstream tag and compared by provider
and model ID. No active Gateway file was replaced.

## ID delta

### Added in v8.0.48

* `gpt-6.1-terra` appears under `codex-free`, `codex-team`, `codex-plus`, and
  `codex-pro` (one logical model ID).
* `kimi-k3.1-pro` and `kimi-k3.1-pro-highspeed` appear under `kimi`.
* `grok-imagine-image`, `grok-imagine-image-2.0`,
  `grok-imagine-image-quality`, `grok-imagine-video`,
  `grok-imagine-video-1.5`, and `grok-imagine-video-1.5-lite` appear under
  `xai`.

### Removed in v8.0.48

`gpt-5.5` is removed from all four Codex account catalogs (`free`, `team`,
`plus`, and `pro`). The ID remains in the Gateway example pricing catalog, so
it is a stale active-model risk until explicitly retired.

### Thinking metadata changes (IDs unchanged)

The upstream `thinking` object changes for six Claude IDs:

* `claude-fable-5` and `claude-fable-5-1`: `zero_allowed` changes `true` to
  `false`; `dynamic_allowed` becomes `true`; old `min`/`max` fields disappear.
* `claude-opus-4-7` and `claude-opus-4-8`: `dynamic_allowed` becomes `true`.
* `claude-opus-5-5`: `zero_allowed` changes `true` to `false`.
* `claude-sonnet-5-5`: adds `disable_type: "between_tools"`.

These flags are upstream request/executor metadata. Gateway model-access and
pricing tables do not store them; they need rebase and request-level tests,
not a SQL permission toggle.

## Pricing and permission audit

The current offline pricing migration (`MigrateCPAV8Pricing`) merges only the
seven reviewed rules in `internal/config/data/cpa-v8-model-prices.json` and
removes only the retired Antigravity aliases. The deployment example
(`deploy/pricing-v2.example.json`) contains 21 models, including `gpt-5.5`.
None of the nine unique v8.0.48 additions has a reviewed price rule in either
file. Prices must not be inferred from a neighboring model: each accepted ID
needs an owner-reviewed service-tier, short/long-context, cache, and maximum
input matrix.

At startup, `cmd/gateway serve` calls
`Store.SyncModelAccessCatalog(cfg.UsagePricing.ManageableModelNames())`.
Therefore:

* an ID absent from `GATEWAY_USAGE_PRICING_JSON` cannot be manageable or
  authorized, even if a SQL migration inserts an access row;
* every priced ID (except internal `codex-auto-review`) is synchronized into
  `model_access_defaults` and missing user snapshots default to enabled;
* leaving `gpt-5.5` in pricing keeps it active despite its upstream removal;
  removing it from the reviewed pricing input deactivates it on the next
  startup, while historical usage and admission snapshots remain intact.

Migrations `0025_cpa_v8_native_models.sql` and
`0035_cpa_v8_0_30_model_catalog.sql` do not cover this delta: `0035` only adds
`claude-haiku-5-5`/`gemini-4-argon-high` and retires five Antigravity IDs. It
cannot be reused as a v8.0.48 migration without changing its backup table,
model set, and rollback identity.

The Gateway router currently exposes Codex, Antigravity, and Anthropic
adapters. The Kimi and XAI registry additions have no Gateway route/adapter or
reviewed billing policy; they must remain unavailable rather than being added
to the model-access catalog. `gpt-6.1-terra` is the only candidate that is in
a currently routed provider family, but it remains blocked until its exact
price and Codex executor compatibility are reviewed.

## Follow-up gate (design only; not an executable migration)

A future `0038_cpa_v8_0_48_model_catalog.sql` may be prepared only after the
source patch has been cleanly rebased and the pricing/provider decisions are
approved. It should:

1. snapshot default, per-user, and multiplier rows for `gpt-5.5` and each
   approved new Codex ID in a v8.0.48-specific backup table;
2. set `gpt-5.5` `catalog_active = false`; add only IDs with an approved price
   and route, preserving explicit owner choices and backfilling missing user
   snapshots with the catalog default;
3. leave API-key allowlists, balances, usage, ledger rows, admission
   snapshots, OAuth credentials, and historical rows untouched; and
4. include conditional rollback checks against the applied snapshot.

Before any such migration, the following gates must pass: clean patch apply on
v8.0.48; full build/test/race/vet and Gateway/CPA smoke matrix; pinned target
registry manifest digest; exact owner-approved prices; acceptance tests for
the six Claude thinking changes; and a reviewed rollback runbook. Until then,
this record is preparation-only and the v8.0.30 lock remains authoritative.
