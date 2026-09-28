# Repository Guidelines

## Project Structure & Module Organization

Entrypoints are `cmd/gateway` and `cmd/antigravity-bridge`. Keep application code under `internal/`: `server` handles HTTP and embeds dashboard assets from `server/assets`; `proxy` forwards requests; `identity` manages authentication; `limit` enforces quotas; `store` accesses PostgreSQL; `antigravity` implements the CLI bridge. Tests live beside code, with fixtures in `testdata/`.

Place executable migrations in `internal/store/migrations`; top-level `migrations/` contains documentation only. Deployment definitions and image locks live in `deploy/` and `docker-compose.yml`, operational helpers in `scripts/`, and procedures in `docs/`.

## Build, Test, and Development Commands

Use Go 1.26.8, matching `go.mod` and CI.

- `go build ./cmd/gateway` builds the gateway binary.
- `go run ./cmd/gateway serve` starts the gateway after configuring a development database and required secrets; see `docs/operations.md`.
- `go test -count=1 ./...` runs uncached unit tests.
- `go test -race -count=1 ./...` checks concurrency; a C compiler is required.
- `go vet ./...` performs static analysis.
- `./scripts/validate-compose.sh` checks Compose, image locks, and security invariants.
- `./scripts/compose.sh build gateway codex-compat antigravity-bridge` builds application images.

For Compose, copy `deploy/env.example` to `.env`, configure it, and follow the README secret-bootstrap instructions with non-production credentials.

## Coding Style & Naming Conventions

Use idiomatic Go: tab indentation, lowercase package names, `PascalCase` exports, and `camelCase` internals. Run `gofmt -w` on changed Go files; `gofmt -l .` must produce no output. Wrap errors with context and preserve fail-closed authentication, quota, and proxy behavior.

Shell scripts use kebab-case filenames, their declared dialect, and `set -eu`; add `pipefail` for Bash pipelines.

## Testing Guidelines

Use Go's `testing` package, `*_test.go` files, `TestXxx` tests, and `FuzzXxx` fuzz targets. Add regression coverage for security and quota changes. No numeric coverage threshold is enforced; exercise affected paths. Install Node.js to run dashboard regressions invoked by Go tests.

PostgreSQL integration tests require a disposable database:

```sh
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store ./internal/server
```

## Commit & Pull Request Guidelines

History uses concise imperative subjects, such as `Stream native AGY Gemini responses`. Keep commits focused. PRs should explain behavior and security impact, list verification commands, link relevant issues, and include screenshots for dashboard changes.

Never commit `.env`, `deploy/secrets/*`, OAuth state, backups, or generated SBOM files.
