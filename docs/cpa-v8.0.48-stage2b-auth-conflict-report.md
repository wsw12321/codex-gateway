# CPA v8.0.48 Gateway auth rebase — stage 2B minimum

This stage is an incremental, local-only rebase on Gateway commit
`6b09c58c04a734182f9f9626338efb49907f161b` (parent
`46094728d1bdc3a1e35af51d092582ee715360d2`).  It does not update
`source.lock.json`, Dockerfiles, Compose, image labels or deployment state.

## Minimum applied subset

`deploy/codex-compat/cliproxy-v8.0.48-gateway-stage2b-auth-min.patch` ports the
Gateway changes for the two highest-leverage CPA auth files:

- `sdk/cliproxy/auth/conductor_cooldown.go`
  - reject stale/blocked Gateway account results and persisted cooldown records;
  - carry `QuotaExhausted` through error equality/cloning and result conversion;
  - convert explicit quota failures into durable Gateway account quarantine while
    keeping 401/403 on the normal auth-refresh path.
- `sdk/cliproxy/auth/types.go`
  - carry Gateway account block/version metadata on `Auth`;
  - preserve `QuotaExhausted` when cloning model errors.

The delta contains two modified-file diffs (120 patch lines).  With the stage 2A
new-file patch already present on the CPA v8.0.48 checkout, it passes:

```text
$ git apply --check cliproxy-v8.0.48-gateway-stage2a-preparation.patch
$ git apply --check cliproxy-v8.0.48-gateway-stage2b-auth-min.patch
# both status 0
```

The v8.0.48 target implementation remains authoritative for cooldown timing,
refresh cancellation and stream lifecycle; the added checks are fail-closed
account gates around those transitions.

## Remaining conflicts

The old v8.0.30 hunks for the remaining conductor files do not apply directly
to CPA v8.0.48 and are intentionally not included in this minimum patch:

```text
sdk/cliproxy/auth/conductor_execution.go
sdk/cliproxy/auth/conductor_home.go
sdk/cliproxy/auth/conductor_home_execution.go
sdk/cliproxy/auth/conductor_lifecycle.go
sdk/cliproxy/auth/conductor_persistence.go
sdk/cliproxy/auth/conductor_refresh.go
sdk/cliproxy/auth/conductor_selection.go
sdk/cliproxy/auth/conductor_stream.go
sdk/cliproxy/auth/scheduler.go
```

A scoped `git apply --reject` pass applied non-overlapping hunks in those files,
but left semantic rejects where v8.0.48 changed request cancellation, refresh
signatures, Home dispatch lifecycle and selection bookkeeping.  Those hunks
must be ported individually; do not resolve by replacing the v8.0.48 files with
v8.0.30 versions.  Watcher/management conflicts remain outside this stage.

## Validation boundary

The clean-apply checks above were run against a checkout at CPA
`dbb706215a3afd2f0ea3c96482d4233856466caa` after applying stage 2A.  No Go
compiler was available in the execution environment (`/usr/bin/go` is the
system Go board-game binary), so this stage claims no `go test`, race or vet
result.  The patch is therefore reviewable but not a production/source-lock
cutover; retain the v8.0.30 rollback baseline until the remaining conflicts are
resolved and tested.
