# CPA v8.0.48 Gateway patch rebase — stage 2A report

This is a **preparation-only** stage.  It is intentionally not a replacement
for `deploy/codex-compat/source.lock.json`, the Compose image, or the production
v8.0.30 patch.

## Bases and scope

- Gateway worktree base: remote `main` `46094728d1bdc3a1e35af51d092582ee715360d2`
  (the worktree was created from `origin/main` at `4609472`).
- CPA rebase target: `v8.0.48` `dbb706215a3afd2f0ea3c96482d4233856466caa`.
- Legacy input: `deploy/codex-compat/cliproxy-v8.0.30-gateway.patch`,
  SHA-256 `f10e3cfd34208d912a216a414807bcc6262f14389d4738d71141d7db40a3f876`.
- Scope: account identity/state, allocation/selection, concurrency and
  conversation persistence under `sdk/cliproxy/auth`, plus watcher account-state
  tests.  `source.lock.json`, Dockerfiles and Compose files are untouched.

## Minimum cleanly applicable subset

`cliproxy-v8.0.48-gateway-stage2a-preparation.patch` contains only the 26
**new-file** diffs from the legacy patch in this scope (6,254 lines):

- `sdk/cliproxy/auth`: gateway account state, allocation selector, concurrency,
  catalog and provider identity/quota helpers; conversation-root/session helpers;
  Codex OAuth dedupe; focused tests.
- `internal/watcher`: account-state/revision tests.

Against a clean CPA checkout at the target commit:

```text
$ git -C /tmp/CLIProxyAPI-v8.0.48 apply --check \
    cliproxy-v8.0.48-gateway-stage2a-preparation.patch
# status 0
```

The patch is kept as a reviewable artifact, but is marked `preparation_only`:
it is not asserted to build in isolation.  The new auth implementation imports
additional cross-domain changes from the legacy patch (notably
`internal/cache/gateway_scope.go` and additions/field changes in existing auth,
service, watcher and API files).  Those dependencies are deliberately excluded
from this minimum subset until their CPA v8.0.48 context can be reconciled.
The Go toolchain was unavailable in this environment (`/usr/bin/go` is the
system Go board-game binary), so no `go test` or race test result is claimed.

## Conflicts requiring the next pass

Filtering the legacy patch to the requested auth/watcher/management paths and
running `git apply --check` against CPA `dbb7062` produced 17 context failures:

```text
internal/api/handlers/management/auth_files_provider_oauth.go
internal/watcher/clients.go
internal/watcher/dispatcher.go
internal/watcher/synthesizer/file.go
internal/watcher/synthesizer/file_test.go
sdk/cliproxy/auth/conductor_cooldown.go
sdk/cliproxy/auth/conductor_execution.go
sdk/cliproxy/auth/conductor_home.go
sdk/cliproxy/auth/conductor_home_execution.go
sdk/cliproxy/auth/conductor_lifecycle.go
sdk/cliproxy/auth/conductor_persistence.go
sdk/cliproxy/auth/conductor_refresh.go
sdk/cliproxy/auth/conductor_selection.go
sdk/cliproxy/auth/conductor_stream.go
sdk/cliproxy/auth/conductor_unauthorized_refresh_test.go
sdk/cliproxy/auth/scheduler.go
sdk/cliproxy/auth/types.go
```

These are semantic conflicts, not whitespace-only failures: CPA changed
conductor scheduling/refresh/persistence and watcher snapshot APIs after the
v8.0.30 baseline.  Do not resolve them by taking either side wholesale; the
next pass must port each hunk while retaining v8.0.48 cancellation, cooldown,
refresh and snapshot behavior.

## Safe-use boundary

This commit does **not** update the active lock, image labels, Compose, source
version, model catalog or deployment scripts.  Keep the v8.0.30 patch and
rollback image as the active baseline until the dependency closure above is
ported and tested on CPA v8.0.48.
