# Pinned CPA build

The default image builds CPA `v8.0.4` at
`d33f63f8e3d98428440ebca5a5b6a981a61ff71e`. The Dockerfile verifies the peeled
release tag, the full commit and `CLIPROXY_PATCH_SHA256` before applying
`cliproxy-v8.0.4-gateway.patch`. Go 1.26.8 and the security dependency updates
from the prior build are retained. The old v7 patch is retained only for the
controlled compatibility rollback described in the operations documentation.

The local catalog receives only `gpt-6.1-sol` from
`router-for-me/models@690c37fdbe62dc05f609f3a3e609d07ea4d16bf1` (`models.json`
SHA256 `35efe922ff4061d959e6d8632bd34bbe2fdca18804e0b338f9d7ec2a3aa40b81`).
Its CPA catalog context is 272000 tokens; larger channel capacity requires
separate real-account validation. `-local-model` disables catalog downloads.
The management panel and provider/plugin discovery auto-updates remain disabled.

Gateway selects accounts for both providers. The internal account interfaces
are `/internal/upstream-accounts/*` and `/internal/antigravity-accounts/*`.
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
management, cache and protocol packages, the Gateway race regressions, and the
explicit prior Codex transport/quota regression list. Run
`scripts/test-sidecar-image.sh IMAGE` afterward for synthetic, networkless
runtime checks, including credential permissions, provider capabilities,
management-key separation and exclusive refresh locking. These checks do not
replace the maintenance-window tests with real accounts and a Gateway key.
