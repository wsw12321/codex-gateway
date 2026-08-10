# Repository Guidelines

## Project Structure & Module Organization

The Go 1.24 gateway entry point is in `cmd/gateway`. Keep application code under `internal/`: HTTP routing and the embedded dashboard live in `internal/server`, forwarding logic in `internal/proxy`, identity and WebAuthn flows in `internal/identity`, quota logic in `internal/limit`, and PostgreSQL access in `internal/store`. Canonical migrations belong beside the store package in `internal/store/migrations`; do not add executable SQL to the top-level `migrations/` documentation directory. Deployment definitions and pinned images are in `deploy/` and `docker-compose.yml`; operational helpers are in `scripts/`, and longer procedures are in `docs/`.

## Build, Test, and Development Commands

- `go build ./cmd/gateway` builds the gateway locally.
- `go test -count=1 ./...` runs the unit suite without cached results.
- `go test -race -count=1 ./...` checks concurrent code with the race detector.
- `go vet ./...` performs Go static analysis.
- `gofmt -w path/to/file.go` formats changed Go files; CI requires `gofmt -l .` to produce no output.
- `./scripts/validate-compose.sh` checks Compose syntax, image locks, and network/security invariants. Copy `deploy/env.example` to `.env` and bootstrap non-production secrets first.
- `./scripts/compose.sh build gateway codex-compat` builds the deployable images.

## Coding Style & Naming Conventions

Follow standard Go formatting and idioms: tabs as emitted by `gofmt`, short lowercase package names, exported identifiers in `PascalCase`, and internal identifiers in `camelCase`. Wrap errors with context and preserve fail-closed behavior at authentication, quota, and proxy boundaries. Shell scripts must use the declared `sh` or Bash dialect, start with `set -eu` (plus `pipefail` for Bash pipelines), and use kebab-case filenames.

## Testing Guidelines

Place tests beside code as `*_test.go`; name unit tests `TestXxx` and fuzz targets `FuzzXxx`. Add regression coverage for security and quota changes. PostgreSQL tests are opt-in and must use a disposable database:

```sh
TEST_DATABASE_URL='postgres://gateway:password@127.0.0.1:5432/gateway_test?sslmode=disable' \
  go test -count=1 -tags=integration ./internal/store
```

No numeric coverage threshold is enforced; all affected paths should be exercised.

## Commit & Pull Request Guidelines

This checkout contains no Git history from which to infer an established convention. Use concise, imperative commit subjects (for example, `Harden SSE lease cleanup`) and keep each commit focused. Pull requests should explain behavior and security impact, list verification commands, link relevant issues, and include dashboard screenshots for UI changes. Never commit `.env`, `deploy/secrets/*`, OAuth state, backups, or generated SBOM files.
