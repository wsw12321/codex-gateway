# CPA v8.0.48 Gateway stage 3B — Claude/Antigravity executor conflict report

Audit base: remote Gateway main `835f61ac554a44ff853507ce00b1030d360e6aee` (CPA v8.0.48 target `dbb70621`, with staged auth/watcher and stage3A Codex quota marker artifacts).

This is a **report-only** stage. No CPA source patch is included because the old v8.0.30 executor hunks do not apply safely to v8.0.48 without a helper dependency migration.

## Claude hunks audited

| Old hunk | Intended behavior | Blocking dependency / v8.0.48 risk |
| --- | --- | --- |
| `claude_executor.go`: `PrepareRequest`, `HttpRequest` | Strip Gateway-internal headers, select a sanitized Gateway auth, and fail closed on transport identity | Requires new `protectGatewayClaudeHeaders` and `gatewayClaudeAuth` (`internal/runtime/executor/claude_gateway.go`), which itself requires `util.RemoveCodexInternalHeaders`; direct copy would not compile and a broad header rewrite could alter v8.0.48 Anthropic native-profile framing. |
| `claude_executor_execute.go` / `claude_executor_stream.go` / `claude_executor_tokens.go` | Route every request through sanitized account auth; scope OAuth tool aliases by Gateway caller/account | Requires `gatewayClaudeAuth`, plus changed alias signatures and `helps.GatewayCacheIdentity`; direct copy changes tool-state lookup keys and can invalidate v8.0.48 continuation/tool-result semantics. |
| `claude_executor_tool_state.go` | Isolate tool alias state by caller/provider/account | Requires new `helps/gateway_cache.go` and metadata keys from Gateway auth; no equivalent in v8.0.48 baseline. |
| `claude_executor_auth.go` | Verify OAuth identity after refresh; disable on subject mismatch | Requires Gateway Anthropic identity metadata (`AnthropicIdentitySubject`, `gateway_anthropic_subject`) and profile semantics from staged auth files. Applying before those are present would silently disable valid native refreshes or leave an unverified identity schedulable. |
| `claude_executor_request.go` | Final header protection at native-profile assembly boundary | Same missing `protectGatewayClaudeHeaders`; replacing the v8.0.48 ordering risks losing required `anthropic-beta`, tool, and streaming headers. |

## Antigravity hunks audited

| Old hunk | Intended behavior | Blocking dependency / v8.0.48 risk |
| --- | --- | --- |
| `antigravity_executor.go` | Gateway-allocation transport isolation, redirect rejection, rejected transport fail-closed | Requires new `helps.GatewayRejectedTransport`, `helps.GatewayGoogleTransport`, and `codexRejectRedirect`; direct replacement would change v8.0.48 proxy/HTTP2 transport lifecycle and native stream framing. |
| `antigravity_executor_auth.go` | Verify Google subject/email during token refresh | Requires new `internal/auth/antigravity.FetchVerifiedIdentity` and Gateway identity metadata; adding only the call does not compile. |
| `antigravity_executor_request.go` | Enforce Gateway history signatures and strip internal headers | Requires `helps.ValidateGatewayGeminiHistory`, `util.RemoveCodexInternalHeaders`, and Gateway session metadata. Inserting the validator without the full session/provenance contract can reject valid v8.0.48 tool rounds or alter translator output. |
| `antigravity_executor_execute.go` / `antigravity_executor_stream.go` | Scope derived session/cache IDs by Gateway account | Requires `helps.GatewayCacheIdentity`; direct replacement would either compile-fail or silently reuse unscoped cache IDs. |

## Safe migration order

1. Land the small shared primitives first: internal metadata keys, `RemoveCodexInternalHeaders`, `GatewayCacheIdentity`, and redirect rejection with focused unit tests.
2. Port Claude header/auth sanitization and alias-key scoping together; preserve v8.0.48 native-profile header ordering and tool-result framing.
3. Port Antigravity rejected/Google transport wrappers and redirect policy; keep the existing v8.0.48 HTTP/2/proxy pool outside Gateway strategy unchanged.
4. Add Claude/Google identity verification at refresh boundaries only after auth metadata identity helpers are present; reject mismatch fail-closed and persist the disabled state.
5. Port account-scoped session/cache IDs and history validators, then run targeted executor transport, stream, and tool-state tests before combining with conductor changes.

## Verification and scope guard

- No source patch is intentionally included; this commit only records the conflict/dependency audit.
- No `source.lock`, Dockerfile, Compose, or active runtime files are modified.
- The environment has no usable Go compiler (`/usr/bin/go` is the desktop Go board-game binary), so targeted Go tests cannot run here.
