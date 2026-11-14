# Codex compatibility sidecar

The sidecar runs unmodified CLIProxyAPI (CPA) with the Gateway plugin. One
process serves Codex, Claude and Antigravity; the Gateway owns identity,
account allocation, billing and egress, and CPA builds and sends every
upstream request.

`plugin.Dockerfile` fetches the release tag recorded in `plugin.lock.json`,
proves that its peeled commit is the locked one and checks out that commit.
The only source change is `residual/cliproxy-plugin-only-selector.patch`, which
adds `routing.strategy: "plugin-only"`: a selector that rejects every pick, so
a missing or misbehaving scheduler plugin fails closed instead of falling back
to a built-in strategy. Everything else the Gateway needs lives in the plugin
under `plugin/`, built as `codex-gateway-v1.0.0.so` against that checkout.
`plugin.lock.json` records the tag, the commit, the residual patch digest, the
digest of CPA's embedded model catalog (`internal/registry/models/models.json`)
and the security module versions, which are applied with `go get`.

The build verifies the commit, the residual patch digest and the model catalog
digest, applies the patch without fuzz, runs the residual patch's tests and
upstream's plugin host tests, then runs the plugin's unit tests, race detector
and its end-to-end suite against the binary it ships. Every end-to-end test is
asserted by name, and a skipped suite fails the build.
`../../scripts/test-cpa-plugin-image.sh` then checks the runtime image with
synthetic keys and no network.

`plugin-entrypoint.sh` checks the credential store (no symlinks or special
files; directories `0700`, files `0600`, all owned by UID 10001), refuses a
plugin the service user could replace, an unexpected file next to it and a
management key equal to the data-plane key, holds `.gateway-refresh.lock` in
the credential volume so a second instance cannot start, and writes a
configuration with the plugin-only strategy, no global proxy and the
management API behind its own key. It refuses command-line logins: credentials
are added on the Gateway's CPA credential page, where CPA runs the login
through the egress the Owner chose. `plugin-healthcheck.sh` is healthy only
while the plugin serves its capability route. CPA maps a plugin from a private
copy under `TMPDIR`, so `docker-compose.yml` mounts an executable tmpfs for it
(`/run/cliproxy-plugin`).

The plugin keeps `.gateway-account-state`, `.gateway-antigravity-identities`
and `.gateway-anthropic-identities` in the format the removed source-patched
image wrote, so a credential volume from that image is used as it is.

The source-patched image (`Dockerfile`, `cliproxy-v8.0.*-gateway.patch`,
`source.lock.json`), the v7 rollback image and the Antigravity CLI bridge were
removed in step 8 of [`cpa-plugin-roadmap.md`](../../docs/cpa-plugin-roadmap.md).
`cpa-v8.0.48-*.json` are audit records of that era, kept for reference.
Behavior, trust boundaries and the upgrade procedure are in
[`compatibility-upgrades.md`](../../docs/compatibility-upgrades.md).
