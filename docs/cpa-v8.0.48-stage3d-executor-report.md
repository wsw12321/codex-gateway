# CPA v8.0.48 Gateway stage 3D — executor-safe subset

Base CPA target: `dbb70621` (v8.0.48), staged Gateway parent: `ff0b000c`.

## Scope

This patch ports only existing Claude/Antigravity executor call sites whose helper dependencies are already present in the stage-3C chain. It intentionally leaves native transport construction, HTTP/2/HTTP/1.1 negotiation, SSE/WS framing, cancellation, and tool-result translation unchanged.

- **Claude tool alias state:** `claude_executor_tool_state.go`, `claude_executor_execute.go`, and `claude_executor_stream.go` accept request metadata and scope OAuth alias keys through `helps.GatewayCacheIdentity`. The variadic metadata parameter keeps existing non-Gateway callers/tests on the native key path.
- **Antigravity account/session scope:** non-stream, Claude-compatible non-stream, and stream execution derive provider cache/session IDs through `helps.GatewayCacheIdentity`.
- **Antigravity request boundary:** `buildRequest` writes the scoped `request.sessionId`, validates Gateway Gemini thought/function-call signatures through `helps.ValidateGatewayGeminiHistory`, and removes Gateway-internal relation headers after custom attributes are applied before restoring the bearer header. Token-count requests apply the same final header protection.

No new helper, auth constructor, transport replacement, retry behavior, stream framing, or tool-result handling is included. The patch must be applied after stage-3C helpers.

## Verification

- `git apply --check` against clean CPA v8.0.48 (`dbb70621`): **PASS** (`/tmp/stage3d-executor.patch`).
- `git diff --check`: **PASS** for the generated patch and touched source files.
- Go tests are **not run**: this environment exposes `/usr/bin/go` as a desktop Go board-game binary and has no Go compiler/gofmt.
- Scope guard: patch contains only seven existing executor files; no `source.lock.json`, Dockerfile, Compose, or helper files are changed.

## Deferred executor work

Claude auth/header sanitization and Antigravity OAuth identity/transport constructor wiring remain deferred until their dedicated helper files and metadata contracts are landed. This keeps the stage independently reviewable and avoids replacing v8.0.48 native framing or cancellation semantics.
