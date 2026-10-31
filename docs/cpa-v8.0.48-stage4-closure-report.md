# CPA v8.0.48 Gateway — stage 4 compile-closure report

**Status: preparation-only; no deployment metadata changed.** This pass closes
four source-level dependency gaps identified by the stage 4 combined artifact.
It does not update `deploy/codex-compat/source.lock.json`, Dockerfiles,
Compose, validation locks, image labels, or third-party notices.

## Provenance

- Gateway source baseline: remote `main` `c8ae526`.
- CPA source baseline: tag `v8.0.48`, commit `dbb706215a3afd2f0ea3c96482d4233856466caa` (`dbb7062`).
- Existing stage 4 combined artifact SHA-256:
  `3635c8e83504e9394178fd0822d7b3f0ac177c27b127806a9541c3e0037972b5`.
- Closure artifact: `deploy/codex-compat/cliproxy-v8.0.48-gateway-stage4-closure.patch`.
- Closure artifact SHA-256:
  `c9648dd677588ab2e6076afb5d01628575b053eef466281f160988868946a958`.
  The patch is 119 lines and touches only four source files plus one new cache
  helper (`internal/cache/gateway_scope.go`).

Apply the existing combined artifact to a clean `dbb7062` checkout first, then
apply the closure artifact. The closure artifact is intentionally also
standalone-applicable to `dbb7062` for review of the missing definitions.

## Minimal source closures

1. **Gateway cache scope:** add `internal/cache.WithGatewayScope`. It stores a
   private request-context marker, normalizes nil contexts, and leaves empty
   scopes unchanged. No existing cache key or v8.0.48 cache behavior is changed.
2. **Executor metadata contract:** add the eight Gateway keys used by the
   combined auth/conductor code:
   `GatewayUserIDMetadataKey`, `UpstreamAccountIndexMetadataKey`,
   `UpstreamAccountIndexCallbackMetadataKey`, `ConversationHashMetadataKey`,
   `ConversationHashCallbackMetadataKey`, `ConversationRootSessionIDMetadataKey`,
   `PinnedGatewayAccountIndexMetadataKey`, and `FirstAttemptOnlyMetadataKey`.
3. **Quota marker:** add `Error.QuotaExhausted bool` with an omitempty JSON tag;
   the zero value preserves existing error behavior.
4. **Auth conductor state:** add `Result.GatewayAccountIndex` and
   `Result.GatewayAccountVersion`, durable account state path/map fields, and
   Gateway concurrency mutex/map/sequence fields. Initialize the account-state
   map in `NewManager`; runtime concurrency remains lazily allocated as before.

## Checks

- `git apply --check` and `git diff --check` pass for the closure artifact on a
  clean CPA checkout; applying combined then closure also passes textual checks.
- A real Go toolchain is unavailable in this environment (`/usr/bin/go` is the
  desktop Go board-game binary; `gofmt` is absent). The required targeted test
  command was attempted:
  `go test ./sdk/cliproxy/auth ./sdk/cliproxy/executor ./internal/cache`
  and returned `Go: Unknown option: test`. No compile, test, vet, race, or binary
  result is claimed.

## Safety decision

Keep the existing CPA v8.0.30 source lock, image, Compose configuration,
validation lock, and third-party notice set unchanged. Treat this artifact as a
source-port/dependency-closure input only; run the targeted auth, executor,
cache, watcher, transport, stream, and race tests in a real Go toolchain before
any deployment or lockfile update.
