# CPA v8.0.48 Gateway auth rebase — execution/refresh/selection block

This is the next incremental stage after stage 2B's cooldown/types patch.  The
Gateway worktree base is remote `main` commit
`4c885760e433525de97cd6065a3d3ab81e50f2b6` (parent
`6b09c58c04a734182f9f9626338efb49907f161b`).  No source lock, Dockerfile,
Compose, image label or deployment file is changed.

## Applied semantic block

`cliproxy-v8.0.48-gateway-stage2b-auth-execution-refresh-selection.patch` ports
three existing CPA auth files while retaining CPA v8.0.48's surrounding
cancellation and refresh machinery:

- `conductor_execution.go`: marks Gateway-managed requests, keeps conversation
  metadata/header isolation, routes attempts through the Gateway admission
  helper, stamps account index/version on execution results, and applies the
  Gateway fail-closed credential-switch policy for request/transport errors.
- `conductor_refresh.go`: refreshes from an auth snapshot (registration epoch,
  credential version and Gateway control version), prevents late rejected-token
  writes after replacement credentials, and preserves Gateway identity/version
  across an unauthorized retry.
- `conductor_selection.go`: lets Gateway allocation see all eligible priority
  tiers, disables plugin scheduler overrides for Gateway allocation, filters
  blocked/disabled accounts and refreshes the selected allocation snapshot.

These are additive/guard hunks; the target CPA implementation remains the
source of truth for cancellation propagation, cooldown timing and refresh error
handling.

## Validation

Using a clean CPA checkout at
`dbb706215a3afd2f0ea3c96482d4233856466caa`, applying stage 2A, stage 2B
cooldown/types, then this patch gives:

```text
$ git apply --check stage2A-preparation.patch   # 0
$ git apply --check stage2B-auth-min.patch      # 0
$ git apply --check this patch                  # 0
```

The Go compiler is unavailable in this environment (`/usr/bin/go` is the
system Go board-game binary), so no `go test`, race or vet result is claimed.
The patch is therefore reviewable and cleanly applicable, but not a
source-lock/production cutover.

## Deferred work

The remaining Gateway auth integration hunks (stream result account stamping,
Home execution/lifecycle, persistence, watcher/management and scheduler edge
cases) remain intentionally deferred.  They must be ported in later semantic
blocks rather than copying v8.0.30 files wholesale.
