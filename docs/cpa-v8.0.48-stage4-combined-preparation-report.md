# CPA v8.0.48 Gateway — stage 4 combined preparation report

**Status: preparation-only; not buildable or deployable.** This stage combines the
already-reviewed stage 2A/2B/2C/3A/3C/3D patch artifacts for dependency and
application analysis. It intentionally does **not** change
`deploy/codex-compat/source.lock.json`, Dockerfiles, Compose, validation locks,
image labels, or `THIRD_PARTY_NOTICES`.

## Provenance and order

- Requested Gateway source baseline: remote `main` `946d1c5` (catalog-audit
  documentation-only advance; source content is unchanged relative to the
  locally available `origin/main` `8ffae3a`).
- CPA target: tag `v8.0.48`, commit
  `dbb706215a3afd2f0ea3c96482d4233856466caa` (`dbb7062`).
- The combined artifact was generated from a clean, full checkout of that CPA
  commit, applying in this order:
  1. stage 2A preparation;
  2. stage 2B auth-minimum, then auth execution/refresh/selection;
  3. stage 2C stream/home, home lifecycle/scheduler, then watcher;
  4. stage 3A Codex quota marker;
  5. stage 3C header sanitization, egress helper, then helper subset 3;
  6. stage 3D executor-safe subset.
- Stage 3B is report-only and contributes no source patch.

Artifact: `deploy/codex-compat/cliproxy-v8.0.48-gateway-combined-preparation.patch`

- SHA-256: `3635c8e83504e9394178fd0822d7b3f0ac177c27b127806a9541c3e0037972b5`
- Diff: **56 files, +7,040/−124 lines** (8,367 patch lines).

## Application checks

Every listed patch passed `git apply --check` and then applied to the clean
CPA checkout. The resulting combined tree passes `git diff --check`. These are
textual-application results only; no compiler is available in this environment
(`/usr/bin/go` is the desktop Go board-game binary), so no `go test`, `go vet`,
race, or binary build result is claimed.

## Dependency-closure blockers

The combined tree is not a closed CPA v8.0.48 source tree. At minimum, the
following symbols/files must be ported or the affected code removed before any
build or deployment claim:

1. **Manager/Result state fields are absent from `sdk/cliproxy/auth/conductor.go`.**
   Combined code uses these fields, but the CPA baseline still lacks them:
   - `Manager.gatewayAccountStatePath`, `Manager.gatewayAccountStates`;
   - `Manager.gatewayConcurrencyMu`, `Manager.gatewayConcurrency`,
     `Manager.gatewayConcurrencySeq`;
   - `Result.GatewayAccountIndex`, `Result.GatewayAccountVersion`.
2. **Error quota marker is absent from `sdk/cliproxy/auth/errors.go`.**
   Stage 2B/3A code constructs and reads `Error.QuotaExhausted`, but the CPA
   `Error` struct has no such field.
3. **Gateway metadata contract is absent from `sdk/cliproxy/executor/types.go`.**
   The combined auth/conductor tests and code reference these missing keys:
   `GatewayUserIDMetadataKey`, `UpstreamAccountIndexMetadataKey`,
   `UpstreamAccountIndexCallbackMetadataKey`, `ConversationHashMetadataKey`,
   `ConversationHashCallbackMetadataKey`, `ConversationRootSessionIDMetadataKey`,
   `PinnedGatewayAccountIndexMetadataKey`, and `FirstAttemptOnlyMetadataKey`.
4. **Cache scoping helper is absent.**
   `sdk/cliproxy/auth/gateway_concurrency.go:287` calls
   `internal/cache.WithGatewayScope`; CPA v8.0.48 has no
   `internal/cache/gateway_scope.go` (or equivalent API).
5. **Semantic integration remains incomplete.** The stage 2A report's 17
   context-conflict files (management OAuth handler, watcher dispatcher/
   synthesizer, and conductor scheduling/refresh/persistence/selection/stream
   files) require CPA-specific porting rather than wholesale v8.0.30 replacement.
   The combined artifact carries only the staged subsets listed above.

These blockers are compile/dependency failures, not whitespace or patch-context
issues. Do not resolve them by updating deployment metadata or by claiming the
combined patch is production-ready.

## Deployment safety decision

Keep the existing v8.0.30 source lock, image, Compose configuration,
validation lock, and third-party notice set unchanged. Continue with a
source-port/dependency-closure pass, then run targeted auth, watcher, executor,
transport, stream, and race tests in a real Go toolchain before considering a
lockfile or image update.
