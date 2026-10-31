# CPA v8.0.48 Gateway stage 2C — watcher/persistence boundary subset

Base CPA target: `dbb70621` (v8.0.48), with stage2A + stage2B + stage2C auth subsets applied. Gateway parent for this subset: `01d887c7695903e39ba5416640aa507d77136d91`.

## Scope

`cliproxy-v8.0.48-gateway-stage2c-watcher-existing.patch` updates only existing watcher files:

- `internal/watcher/events.go` ignores Gateway account-state files before auth-dir event handling.
- `internal/watcher/clients.go` excludes the state file from both auth-directory scans.
- `internal/watcher/synthesizer/file.go` excludes the state file during directory synthesis and direct-file synthesis.

This isolates the Gateway state file without replacing CPA v8.0.48 watcher snapshot/revision logic. The CPA v8.0.48 `conductor_persistence.go` implementation is intentionally unchanged: its auth-dir persistence, cooldown metadata, save-delta merge, and registration-epoch checks are retained as-is to avoid regressing stale snapshot semantics. No persistence hunk was required for this safe subset.

## Verification

- `git apply --check -p1` against a clean v8.0.48 checkout with stage2A + stage2B + stage2B-next baseline: **PASS**.
- Actual `git apply -p1`: **PASS**.
- No source lock, Dockerfile, Compose, or active runtime files changed in this Gateway commit; this commit carries the reviewable patch/report only.
- Go compiler is unavailable in the execution environment (`/usr/bin/go` is the desktop Go board-game binary), so `go test`, `go vet`, and race tests are **not run**.

Remaining watcher revision edge cases (outside state-file filtering) are deferred unless a later test identifies a concrete CPA v8.0.48 regression.
