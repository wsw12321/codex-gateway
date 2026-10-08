# Pinned CPA build

The default image builds CPA `v8.0.20` at
`0f96f568e4dbf6f84ad7399a74b78344c5eac7e6`. The Dockerfile verifies the peeled
release tag, the full commit and `CLIPROXY_PATCH_SHA256` before applying
`cliproxy-v8.0.20-gateway.patch`. Go 1.26.8 and the security dependency updates
from the prior build are retained. The old v7 patch is retained only for the
controlled compatibility rollback described in the operations documentation.

The local catalog is the unchanged `internal/registry/models/models.json` from
the same pinned CPA commit (SHA256
`3a97eea65c1df3ea8ad4edac838b37f7714868d1e784b3723d0650b6e848aa9a`).
It already contains `gpt-6.1-sol` for Codex plus/pro/team with a 272000-token
context, so the three local additions have been removed. Gateway's public model
authorization and pricing remain unchanged. Larger channel capacity requires
separate real-account validation. `-local-model` disables catalog downloads.
The management panel and provider/plugin discovery auto-updates remain disabled.

Gateway selects accounts for all three providers. The internal account interfaces
are `/internal/upstream-accounts/*`, `/internal/antigravity-accounts/*`, and
`/internal/anthropic-accounts/*`.
Candidate providers determine the corresponding Gateway callback; request
headers cannot override the provider. Eligibility is rechecked before affinity
reuse. The Gateway selector limits one request to two credentials, with no
outer retry rounds; streaming bootstrap retries remain zero.

Antigravity credentials carry a verified `google_subject`. The separate
`.gateway-antigravity-identities` document maps that subject to the existing
16-character Gateway account ID. The mapping and `.gateway-account-state`
control document use atomic mode-0600 writes. Duplicate subjects are quarantined.
The entrypoint holds `.gateway-refresh.lock` for the process lifetime. Migration
tools must acquire the same lock. `CPA_ANTIGRAVITY_ENABLED=false` prevents
Antigravity allocation, management operations and background refresh during a
bridge rollback while allowing Codex service to continue.

The narrow `/internal/gateway-management/*` facade reads the separate
`CPA_MANAGEMENT_KEY_FILE` server-side. The unrestricted CPA management API stays
disabled. Never publish CPA's listening port or pass its keys to a browser.

Provider caches are scoped by authenticated caller, provider and stable account.
Native Gemini tool histories require real Google signatures; unrecoverable
legacy continuations receive `new_session_required`. Antigravity model discovery
is account-specific and permits only the eight reviewed Gemini IDs. Google
requests use the configured egress proxy, fixed hosts, and no redirects.

The Docker build runs the full affected API, authentication, watcher, session,
management, configuration, cache, thinking and protocol packages, the Gateway
credential and Claude cache race regressions, and the explicit prior Codex
transport/quota regression list. Run
`scripts/test-sidecar-image.sh IMAGE` afterward for synthetic, networkless
runtime checks, including credential permissions, provider capabilities,
management-key separation and exclusive refresh locking. These checks do not
replace the maintenance-window tests with real accounts and a Gateway key.
The v8.0.20 upgrade delivers source and configuration changes only; local checks
and pending CI and real-account acceptance are recorded in
[`cpa-v8.0.20-validation.md`](../../docs/cpa-v8.0.20-validation.md).

Claude subscriptions use Gateway provider `anthropic`; the adapter maps it to
CPA executor `claude` and OAuth flow `anthropic`. The independent mode-0600
`.gateway-anthropic-identities` registry accepts only account UUID + organization
UUID pairs verified by the fixed upstream OAuth profile endpoint. Imports rotate
the refresh token when present and re-verify the profile; access-only exports
also require a fresh profile verification. Uploaded policy, plan and identity
fields are ignored. Reauthorization keeps the existing account ID and filename.
Duplicates are quarantined. A changed or unavailable profile after token rotation
persists the rotated credential disabled, requiring verified reauthorization.

Native `/v1/messages` and `/v1/messages/count_tokens` requests cannot select a
non-Claude executor in gateway-allocation mode. Native SSE ordering, tool inputs,
thinking/signatures and protocol headers retain CPA's tested upstream behavior.
Claude transport overrides are removed; OAuth and inference use only the fixed
`platform.claude.com` and `api.anthropic.com` authorities through the configured
proxy, with redirects rejected. Account quota refresh reads the latest observed
five-hour/seven-day headers and their observation time. Missing percentages and
subscription plans remain unknown; native timed cooldown recovery stays enabled.

The Anthropic capability endpoint returns both `upstream_account_access_v1` and
`anthropic_messages_v1`. Run real-account acceptance only after database,
Gateway, and this CPA image are upgraded. Cover reauthorization, refresh then
restart, streaming tool continuation and billing attribution. Before rollback,
drain Claude traffic and pending settlements; retain the identity/control files.
