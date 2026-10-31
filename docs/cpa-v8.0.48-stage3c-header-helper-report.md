# CPA v8.0.48 Gateway stage 3C — header helper subset

Base CPA target: `dbb70621` (v8.0.48), staged Gateway parent `5b775374fb70231367d6777d26147adfa19893e1`.

## Scope

`cliproxy-v8.0.48-gateway-stage3c-header-sanitization.patch` updates only `internal/util/header_helpers.go`:

- defines the Gateway routing/account header names;
- prevents custom attributes from injecting those internal headers;
- adds `RemoveCodexInternalHeaders` for final executor assembly;
- removes diagnostic, routing, attribution, and cross-turn relation headers while retaining provider cache/turn metadata.

No executor calls this helper yet; this commit is a dependency-only step for later Claude/Antigravity/Codex transport patches. Existing v8.0.48 native request and stream framing are therefore untouched.

## Verification

- `git apply --check -p1` against clean CPA v8.0.48: **PASS**.
- No source lock, Dockerfile, Compose, or active runtime files changed; this Gateway commit carries the reviewable patch/report only.
- Go compiler is unavailable (`/usr/bin/go` is the desktop Go board-game binary), so targeted Go tests are **not run**.
