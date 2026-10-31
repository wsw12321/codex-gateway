# CPA v8.0.48 Gateway stage 2C — home/lifecycle/scheduler subset

Base CPA target: `dbb70621` (v8.0.48), with stage2A preparation and stage2B/2C-stream-home patches applied. Gateway parent for the preceding subset: `5f2a936142d501b2411d37bb9f62f81447deef29`.

## Scope

`cliproxy-v8.0.48-gateway-stage2c-home-lifecycle-scheduler.patch` is an independently reviewable diff for:

- `sdk/cliproxy/auth/conductor_home.go`
- `sdk/cliproxy/auth/conductor_lifecycle.go`
- `sdk/cliproxy/auth/scheduler.go`

It prevents home dispatch from entering Gateway allocation mode, stamps account identity/version on home credit results, applies Gateway OAuth identity and deduplication during registration/replacement, rejects duplicate Antigravity/Claude account indices, blocks stale refresh replacement for Gateway-managed auth, and keeps scheduler membership fail-closed when an account is unavailable. Existing CPA cooldown/refresh/cancellation flow is left intact; this subset only gates identity/lifecycle and scheduling boundaries.

## Verification

- `git apply --check -p1` against a clean v8.0.48 checkout with stage2A + stage2B + stage2B-next baseline: **PASS**.
- Actual `git apply -p1`: **PASS**.
- No source lock, Dockerfile, Compose, or active runtime files changed in this Gateway commit; this commit carries the reviewable patch/report only.
- Go compiler is unavailable in the execution environment (`/usr/bin/go` is the desktop Go board-game binary), so `go test`, `go vet`, and race tests are **not run**.

Persistence and watcher existing-file filters remain deferred to the next subset to avoid mixing filesystem semantics with lifecycle logic.
