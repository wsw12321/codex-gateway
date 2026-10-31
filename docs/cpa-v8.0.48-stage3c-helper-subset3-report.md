# CPA v8.0.48 Gateway stage 3C — helper subset 3

Base CPA target: `dbb70621` (v8.0.48), staged Gateway parent: `cac76ba24dd63ce55293babb3fafb3031f66837a`.

## Scope

`cliproxy-v8.0.48-gateway-stage3c-helper-subset3.patch` adds only independently reviewable helper files:

- `internal/runtime/executor/helps/gateway_cache.go`: `GatewayCacheIdentity` hashes the authenticated Gateway caller, provider, stable upstream account, caller scope, and client cache suffix so provider state cannot cross account boundaries.
- `internal/runtime/executor/helps/gateway_history.go`: `ValidateGatewayGeminiHistory` rejects missing or translator-bypass Gemini thought signatures with a `new_session_required` error.
- `internal/runtime/executor/helps/gateway_security_test.go`: focused cache, egress, and history helper tests (the egress cases exercise the stage-3C egress helper already landed in `cac76ba`).
- `internal/auth/antigravity/gateway_identity.go`: strict Google OAuth transport/client construction, verified Google identity lookup (`FetchVerifiedIdentity`), and bounded Gemini quota-window lookup.
- `internal/auth/antigravity/gateway_identity_test.go`: verified-identity and OAuth endpoint/proxy rejection tests.

No existing executor or auth files are modified; constructor wiring and provider call sites remain deferred to a later stage. This preserves CPA v8.0.48 native request, HTTP/2, WS/SSE framing, cancellation, and tool-result behavior.

## Verification

- `git apply --check` against clean CPA v8.0.48 (`dbb70621`): **PASS**.
- No source lock, Dockerfile, Compose, or active runtime files changed; this commit carries only new helper/test patch artifacts and this report.
- Go compiler unavailable (`/usr/bin/go` is the desktop Go board-game binary), so targeted Go tests are **not run**.
