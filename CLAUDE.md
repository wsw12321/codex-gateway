# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go gateway ("水源喵中转站") that sits between AI CLI clients (Codex, agy/Gemini, Claude Code) and a version-pinned CLIProxyAPI ("CPA", service `codex-compat`) running unmodified with the Gateway plugin. The gateway owns identity (Passkey/password/OIDC), per-device API keys, quotas, billing, usage metadata, and upstream account allocation. CPA holds the upstream OAuth credentials and performs the model calls. The gateway never stores prompts or responses. The README, `docs/`, the dashboard UI, and user-facing error messages are in Chinese. Code, comments, and most commit subjects are in English.

## Commands

The toolchain is pinned to Go 1.26.11 (`go.mod` `toolchain`, CI, and every Dockerfile).

```sh
go build ./cmd/gateway
gofmt -l .                      # must print nothing (CI enforces)
go vet ./...
go test -count=1 ./...          # unit tests; no database needed
go test -race -count=1 ./...    # needs a C compiler
go test -count=1 -run '^TestName$' ./internal/server   # single test
```

**CPA plugin module.** `deploy/codex-compat/plugin/` is a separate Go module, so `./...` from the root does not reach it. Its end-to-end suite (`-tags e2e`) needs a CPA binary built with the residual patch and the compiled plugin; it runs inside `plugin.Dockerfile`. See the plugin-mode section of `docs/compatibility-upgrades.md`.

```sh
(cd deploy/codex-compat/plugin && go vet ./... && go test -race -count=1 ./...)
python3 -m unittest scripts.tests.test_cpa_plugin_deploy   # lock/patch/Dockerfile/override consistency
```

**Dashboard JS tests.** Some Go tests (e.g. `TestOverviewUI`) shell out to `node --test internal/server/testdata/*.cjs`. If `node` is not on PATH they are skipped silently, so install Node to actually exercise them. You can run one directly from the repo root:

```sh
node --test internal/server/testdata/overview_ui_test.cjs
```

The `*_browser.cjs` files are manual Playwright scripts. They are not run by `go test`. They regenerate screenshots under `docs/screenshots/`:

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright node internal/server/testdata/<name>_browser.cjs
```

**PostgreSQL integration tests** use the `//go:build integration` tag and are skipped unless `TEST_DATABASE_URL` is set. Each test creates its own schema, but the database must still be disposable:

```sh
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
```

**Deployment and script checks.** `validate-compose.sh` needs a configured `.env` copied from `deploy/env.example`:

```sh
python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
./scripts/validate-compose.sh
./scripts/compose.sh config --quiet       # always use this wrapper, never bare `docker compose`
./scripts/compose.sh build gateway codex-compat antigravity-bridge
```

`scripts/compose.sh` clears inherited env, then loads `.env` plus `deploy/images.lock.env`, so the image digest locks cannot be bypassed. CI also runs `govulncheck`, `gosec`, `go-licenses` (allowed: Apache-2.0, BSD-2/3, ISC, MIT, MPL-2.0), Trivy, and the `scripts/test-*-image.sh` container checks.

Gateway subcommands: `serve` (default; runs migrations first), `migrate`, `bootstrap-owner`, `maintenance`, `version`.

## Architecture

### Topology

cloudflared → Caddy → `gateway` → `codex-compat` (CPA v8.0.59 + the residual plugin-only selector patch + the Gateway plugin in `deploy/codex-compat/plugin/`) → Squid egress allowlist. No service publishes host ports.

`CPA_INTEGRATION` in `.env` selects the image: `plugin` (default) or `patch`, the source-patched rollback image (`deploy/codex-compat/cliproxy-v8.0.59-gateway.patch`, added by `scripts/compose.sh` as `deploy/patch-cpa.override.yml`). The Gateway's own `CPA_INTEGRATION` always matches the image. The source patch and its `/internal/*` code paths are removed once plugin mode has passed real-account acceptance (`docs/cpa-plugin-roadmap.md`, step 8).

The gateway reaches CPA at `SIDECAR_URL` with a fixed internal bearer (`SIDECAR_API_KEY`). It strips client auth, cookies, and hop-by-hop headers first.

**The plugin calls back into the gateway** at `POST /internal/{upstream,antigravity,anthropic}-accounts/{select,eligible,attempt,resolve-egress,register-egress}` to pick which OAuth account serves each request, report the decision (the request ledger in `internal/proxy/ledger.go`) and resolve each account's egress. Allocation weights, per-user account scopes, dedicated grants, and concurrency all live in the gateway (`internal/server/upstream_allocation.go`). CPA only executes. A change to the selection contract must land on both sides: the Go handler and the plugin (and the CPA patch while the rollback image exists).

`cmd/antigravity-bridge` with `internal/antigravity` is the legacy AGY transport. It is kept only for the `legacy-bridge` Compose profile and rollback. The default is `ANTIGRAVITY_TRANSPORT=cpa`.

### Request lifecycle (`internal/server/api.go` `executeAPIRequest`)

All generation endpoints share one path:

- `/v1/responses`
- `/v1/responses/compact`
- `/v1/messages` (Claude only; non-`claude-*` models are rejected)
- `claude-*` models may also use `/v1/responses` (CPA translates it to Claude; `prepared.claude` routes it through the Anthropic client, provider and billing mode), but only while the Owner switch `protocol_conversion_settings.claude_responses_enabled` is on. It is off by default; `requireClaudeResponsesConversion` (`internal/server/protocol_conversion.go`) reads it only for this request shape and fails closed. `/v1/responses/compact` is rejected for Claude. CPA's translated usage omits `cache_write_tokens`, so settlement falls back to billing all uncached input at the higher cache-write price.
- `/v1beta/models/*` (Gemini native)

The steps are:

1. `prepareModelBody` stream-scans the top-level `model` and `service_tier`, spooling large bodies to disk. On the Responses and Messages routes, `applyModelRoute` (`internal/server/model_routes.go`) then looks up the user's model route (`user_model_routes`) and, on a hit, rewrites only the top-level `model` value in the spooled body. Every later step (permission, key allowlist, pricing, provider, funding) sees the target model; `usage_requests.requested_model` keeps the client's model. A lookup error fails closed with `503 model_route_unavailable`; protocol translation stays in CPA.
2. Snapshot the pricing for the exact model from `config.UsagePricing`.
3. `store.AdmitRequest` runs **one transaction** that covers quota reservation (RPM/concurrency/daily), the usage row, the model-permission check, and the billing reservation with its fixed funding-source order.
4. Forward through `proxy.Router` or the provider client. A lease-renewal goroutine keeps long SSE streams alive.
5. `CompleteUsageRequest` writes usage, then `SettleRequest` settles the ledger.

Settlement always runs, even on failure or client disconnect. Usage is settled exactly once, and missing usage must never settle as a successful zero-cost request. Money is exact decimal strings handled by `internal/billing` (`math/big`), never `float64`.

### Packages

- `internal/server`: all HTTP routes are in `routes()` in `server.go`. Middleware wrappers set route policy:
  - `requireSession` and `ownerOnly` for dashboard access.
  - `requireRecentVerification` (5-minute re-auth) for sensitive writes.
  - `browserOrigin` for the Origin check.
  - `requireAPIKey` and `geminiAPI` for the data plane.

  Handlers depend on narrow `*Repository` interfaces (fields on `Server`), so unit tests substitute fakes.
- `internal/store`: all SQL (pgx through `database/sql`), quota, billing, usage aggregation, and migrations.
- `internal/proxy`: upstream HTTP clients, SSE usage/TTFT parsing, account attribution, and Gemini/Anthropic adapters.
- `internal/config`: env loading. Every secret also accepts `NAME_FILE`. This package also holds the pricing schema/validator and `IsAntigravityModel`.
- `internal/httpx`: OpenAI-style error JSON (`WriteError`: stable `type`/`code`, Chinese message, `request_id`), security headers/CSP, and trusted-proxy request context.
- `internal/security`: API key format `cgk_v1_<id>_<secret>`, peppered HMAC, AES-GCM key cipher, and `RedactText`.
- `internal/identity`: WebAuthn, passwords (Argon2id), sessions, and OIDC.
- `internal/modelid`: upstream model fingerprinting against embedded, SHA-256-pinned reference data. It fails closed if an asset changes.
- `cmd/cpa-pricing-migrate` and `cmd/cpa-credentials-migrate`: offline stdin/stdout conversion tools. They never touch a live deployment.

### Startup (`cmd/gateway/main.go`)

`serve` runs migrations first. It then syncs the model-access catalog and initializes subscription and billing model scopes from `UsagePricing.ManageableModelNames()`, then recovers interrupted model-identification runs. Only after all of that does it listen. Code that depends on the model catalog must account for this ordering.

### Dashboard

The dashboard is one vanilla-JS file, `internal/server/assets/app.js`, plus `index.html`, `style.css`, and `theme.js`, all embedded via `go:embed` in `assets.go`. There is no build step and no bundler. The CSP is `script-src 'self'`, so there are no inline scripts or handlers.

The Node tests load `app.js` by slicing it at the last `\nstart().catch(` and evaluating it in a `vm` context with stub DOM objects. Keep that bootstrap call as the final statement of the file, and keep top-level functions reachable from that context.

## Change checklists

**New migration**
- Add `internal/store/migrations/NNNN_name.sql`. The top-level `migrations/` folder is documentation only; add an entry to `migrations/README.md`.
- Bump the hardcoded count in `TestEmbeddedMigrationsCoverRequiredSchema` (`internal/store/store_test.go`).
- Upgrade integration tests replay migration subsets by file name (e.g. `group_priority_upgrade_integration_test.go`). Check whether they need the new name.
- Migrations are forward-only and checksum-verified. Never edit an applied migration. A database with an unknown migration refuses older binaries.
- `.gitignore` ignores `*.sql` except this directory and `scripts/rollback-cpa-model-config.sql`.

**Model prices.** Edit `deploy/pricing-v2.example.json`. Two other places must match it:
- the single-line `GATEWAY_USAGE_PRICING_JSON` copy in **both** `deploy/env.example` and `deploy/env.gpt-5.6.example` (enforced by `internal/config/pricing_test.go`);
- `internal/config/data/cpa-v8-model-prices.json`.

`scripts/tests/test_pricing_catalog.py` asserts exact prices and `catalog_as_of`, so update it as well. Price changes never rewrite historical ledger rows.

**Go or base-image version bump.** These must agree, and the CI step "Verify Go toolchain sources agree" checks them:
- `go.mod` `toolchain`
- `GO_VERSION` in `ci.yml`
- `deploy/images.sources`
- `deploy/images.lock.env`
- the `ARG GOLANG_IMAGE=` line in all six Dockerfiles that build Go (including `deploy/codex-compat/plugin.Dockerfile`)
- `go_version` in `deploy/codex-compat/source.lock.json` and `deploy/codex-compat/plugin.lock.json`

Regenerate or check the locks with `./scripts/lock-images.sh [--check]`.

**CPA version bump.** Follow `docs/compatibility-upgrades.md`. The pinned commit is also asserted in `THIRD_PARTY_NOTICES` and `docker-compose.yml` by CI. Plugin mode pins the same commit in `deploy/codex-compat/plugin.lock.json`, `docker-compose.yml` and the plugin's `go.mod`; the rollback pins it in `deploy/patch-cpa.override.yml` and `deploy/codex-compat/source.lock.json`. `scripts/tests/test_cpa_plugin_deploy.py` checks that they agree. Remaining work is in `docs/cpa-plugin-roadmap.md`.

## Conventions

- **Fail closed.** Authentication, quota, model permission, account eligibility, and missing usage all reject rather than allow. Never add automatic fallback to a paid Platform API.
- **CPA builds every upstream request.** Inference, token refresh, OAuth, model listing and quota data come from CPA's own code paths. The plugin and the Gateway never construct provider requests (no provider URLs, headers, User-Agents or TLS clients of their own), and no patch may change how CPA builds them. Leave a capability out rather than reimplement it. The only CPA patch left in plugin mode is the residual plugin-only selector, which does not touch request content.
- **Account egress never falls back.** When an account's egress is unavailable, that account's request, refresh or login fails; it never switches to the global egress or a direct connection. The scheduler may still try another authorized account through that account's own egress.
- **Logging.** Never log request or response content, tokens, or raw error text from upstreams. Existing code logs `fmt.Sprintf("%T", err)` (see `internalError` in `middleware.go`) or passes errors through `security.RedactText`.
- **gosec suppressions** must be targeted and justified: `// #nosec G115 -- <reason>`.
- **Shell scripts** use one of the shebangs CI accepts (`#!/bin/sh`, `#!/usr/bin/env bash`, `#!/sbin/openrc-run`), `set -eu`, and `pipefail` for Bash pipelines.
- **Commits** use concise imperative subjects, e.g. `Add billing model scopes`. Feature work typically also updates the README and adds or updates a `docs/*-validation.md` record of what was verified and how. PRs should state behavior and security impact and list verification commands. Dashboard changes need screenshots.
- **Never commit** `.env`, `deploy/secrets/*`, OAuth files, backups, `*.age`/`*.dump`, or SBOMs.
- **Operations** (deploy, upgrade order, backup/restore, rollback) live in `docs/operations.md`. Trust boundaries are in `docs/threat-model.md`.
