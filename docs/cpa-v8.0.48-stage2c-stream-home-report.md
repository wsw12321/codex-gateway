# CPA v8.0.48 Gateway stage 2C — stream/home execution subset

Base CPA target: `dbb70621` (v8.0.48), with stage2A preparation and stage2B auth patches already applied. Gateway parent: `3bc83deb02442f0a33dda30fb181579bec4777e7`.

## Scope

`cliproxy-v8.0.48-gateway-stage2c-stream-home.patch` is a minimal, independently reviewable diff for:

- `sdk/cliproxy/auth/conductor_stream.go`
- `sdk/cliproxy/auth/conductor_home_execution.go`

It preserves CPA v8.0.48 stream cancellation, cooldown and retry behavior while adding Gateway account/version stamping, Gateway stream-attempt routing, fail-closed credential switching for bootstrap errors, and first-attempt probe handling. Empty-stream bootstrap errors remain retryable request-scoped errors so cancellation and upstream retry semantics are not broadened.

## Verification

- `git apply --check -p1` against a clean v8.0.48 checkout with stage2A + stage2B + stage2B-next baseline: **PASS**.
- Actual `git apply -p1`: **PASS**.
- No source lock, Dockerfile, Compose, or active runtime files changed in this Gateway commit; this commit carries the reviewable patch/report only.
- Go compiler is unavailable in the execution environment (`/usr/bin/go` is the desktop Go board-game binary), so `go test`, `go vet`, and race tests are **not run**.

Remaining stage2C work (home dispatch selection, lifecycle/persistence, scheduler, watcher existing-file filters) is intentionally deferred to separate subsets to keep this change auditable.
