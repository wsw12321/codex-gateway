# CPA v8.0.48 Gateway stage 3A — Codex quota marker subset

Base CPA target: `dbb70621` (v8.0.48), with staged auth/watcher patches through Gateway commit `fbdba8b5a336fbf15ec9a7da0ab0b6526a1307e6`.

## Scope

`cliproxy-v8.0.48-gateway-stage3a-codex-quota-marker.patch` updates only:

- `internal/runtime/executor/openai_compat_executor.go`
- `internal/runtime/executor/codex_executor_terminal.go`
- `internal/runtime/executor/codex_websockets_errors.go`

It exposes a `QuotaExhausted()` signal on the shared status error, preserves unauthorized/forbidden terminal failures as authentication failures, and marks Codex HTTP/WS `usage_limit_reached` responses as quota-exhausted 429s. Existing v8.0.48 transient-capacity/rate-limit classification and Retry-After precedence remain unchanged. This allows the Gateway conductor to fail closed on real account quota without treating auth failures or model capacity as durable quota locks.

The websocket patch is deliberately limited to error classification; v8.0.48 WS/SSE framing, cancellation, and duplex ownership code are not replaced.

## Verification

- `git apply --check -p1` against clean CPA v8.0.48: **PASS**.
- Actual `git apply -p1`: **PASS**.
- No source lock, Dockerfile, Compose, or active runtime files changed in this Gateway commit; this commit carries the reviewable patch/report only.
- Go compiler is unavailable in the execution environment (`/usr/bin/go` is the desktop Go board-game binary), so targeted Go tests, vet, and race tests are **not run**.

A later executor subset may address account/version outbound markers only if its helper dependencies can be ported without replacing CPA v8.0.48 transport semantics.
