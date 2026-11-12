# Pinned CPA build

The default image builds CPA `v8.0.59` at
`c5251b57b3fa5dcbc689f514f4aa166daacb4481`. The Dockerfile verifies the peeled
release tag, the full commit and `CLIPROXY_PATCH_SHA256` before applying
`cliproxy-v8.0.59-gateway.patch`. The build uses Go 1.26.11 and retains the security
dependency updates from the prior build. The old v7 patch is retained only for the
controlled compatibility rollback described in the operations documentation.

The local catalog is the exact `internal/registry/models/models.json` from
the same pinned CPA commit; its SHA256 and provenance are recorded in
`source.lock.json`. Gateway keeps remote catalog downloads disabled and exposes
only the reviewed model and pricing policy. Larger channel capacity requires
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
is account-specific and permits only the five reviewed Antigravity IDs:
`gemini-3-flash`, `gemini-3.8-flash-high`, `gemini-3.1-flash-lite`,
`gemini-3.5-flash-lite`, and `gemini-4-argon-high`. The base `gemini-4-argon`
belongs to upstream provider `gemini`; Muse belongs to `meta`. Neither provider
is integrated, and those models have no Gateway prices, grants, or routes. Google
requests use the configured egress proxy, fixed hosts, and no redirects.

The Docker build runs the full affected API, authentication, watcher, session,
management, configuration, cache, thinking and protocol packages, the Gateway
credential and Claude cache race regressions, and the explicit prior Codex
transport/quota regression list. Run
`scripts/test-sidecar-image.sh IMAGE` afterward for synthetic, networkless
runtime checks, including credential permissions, provider capabilities,
management-key separation and exclusive refresh locking. These checks do not
replace the maintenance-window tests with real accounts and a Gateway key.
The v8.0.48 upgrade rebased the complete v8.0.30 gateway patch, kept
`klauspost/compress` at v1.18.7, disabled downstream response compression on the
internal Gateway hop, retired `gpt-5.5` and required the `0038` catalog migration
plus the offline pricing merge. The v8.0.59 upgrade adopts upstream's Codex
refresh journal: each refresh grant is claimed under `.codex-refresh/` in the
OAuth volume before it is sent, and an uncertain result is reported as
`refresh_outcome_unknown` instead of being retried. It also restores Gateway
behavior that the v8.0.58 rebase lost; that image never passed CI. Local CI
replication and pending real-account acceptance are recorded in
[`cpa-v8.0.59-validation.md`](../../docs/cpa-v8.0.59-validation.md).

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

## Plugin mode (optional)

`plugin.Dockerfile` builds the same pinned CPA commit without the feature
patch. The only source change is
`residual/cliproxy-plugin-only-selector.patch`, which adds
`routing.strategy: "plugin-only"`: a selector that rejects every pick, so a
missing or misbehaving scheduler plugin fails closed instead of falling back
to a built-in strategy. Everything else the Gateway needs lives in the plugin
under `plugin/`, built as `codex-gateway-v1.0.0.so` against that checkout.
`plugin.lock.json` records the commit, the residual patch digest and the
security module versions, which are applied with `go get` instead of `go.mod`
hunks.

The build verifies the commit and the residual patch digest, applies the patch
without fuzz, runs the residual patch's tests and upstream's plugin host tests,
then runs the plugin's unit tests, race detector and its end-to-end suite
against the binary it ships. Every end-to-end test is asserted by name, and a
skipped suite fails the build. `../../scripts/test-cpa-plugin-image.sh` then
checks the runtime image with synthetic keys and no network.

`plugin-entrypoint.sh` reuses `entrypoint.sh verify-oauth` for the credential
store checks, refuses a plugin the service user could replace, an unexpected
file next to it and a management key equal to the data-plane key, and writes a
configuration with the plugin-only strategy and the management API behind its
own key. `plugin-healthcheck.sh` is healthy only while the plugin serves its
capability route. CPA maps a plugin from a private copy under `TMPDIR`, so
`../cpa-plugin.override.yml` adds an executable tmpfs for it.

The plugin keeps `.gateway-account-state` and both identity documents in the
format described above, so either image can take over the other's credential
volume. Behavior differences, the features plugin mode does not yet serve and
the upgrade procedure are in
[`compatibility-upgrades.md`](../../docs/compatibility-upgrades.md).
