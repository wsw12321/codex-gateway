# CPA v8.0.48 Gateway stage 3C — egress helper subset

Base CPA target: `dbb70621` (v8.0.48), staged Gateway parent `16e0676f11e3acf4e359f8de2e4d0410a59296f2`.

## Scope

`cliproxy-v8.0.48-gateway-stage3c-egress-helper.patch` adds `internal/runtime/executor/helps/gateway_egress.go`:

- `GatewayRejectedTransport` fails closed with a deterministic error when Gateway egress is unavailable.
- `GatewayGoogleTransport` permits only HTTPS requests to the fixed Google OAuth/Cloud Code host allowlist, rejects userinfo/non-443 ports/Host overrides, and requires a non-nil base transport.
- `ValidGatewayProxy` validates supported HTTP(S)/SOCKS proxy URL schemes.

This helper is dependency-only; no existing executor or transport path is replaced in this commit, preserving CPA v8.0.48 native HTTP/2, WS/SSE framing, cancellation, and tool-result behavior.

## Verification

- `git apply --check -p1` against clean CPA v8.0.48: **PASS**.
- No source lock, Dockerfile, Compose, or active runtime files changed; this Gateway commit carries the reviewable patch/report only.
- Go compiler unavailable (`/usr/bin/go` is the desktop Go board-game binary), so targeted Go tests are **not run**.
