ARG GOLANG_IMAGE=docker.io/library/golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0
ARG RUNTIME_IMAGE=docker.io/library/debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

# Unmodified CPA plus the Gateway plugin. The build carries no feature patch:
# the only source change is the residual fail-closed selector, and everything
# the Gateway needs lives in the plugin.

FROM ${RUNTIME_IMAGE} AS runtime-base
# Keep the reviewed glibc runtime: the plugin is a cgo shared library loaded by
# a cgo host. Netcat powers the probes without placing a key in process
# arguments. Explicitly upgrade perl-base (DLA-4821-1) and APT (CVE-2026-109518,
# fixed in bookworm-security 2.6.2): the pinned base predates both updates.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libpcre2-8-0 netcat-openbsd perl-base util-linux apt libapt-pkg6.0 \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' perl-base)" ge '5.36.0-7+deb12u4' \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' apt)" ge '2.6.2' \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' libapt-pkg6.0)" ge '2.6.2' \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 cliproxy \
    && useradd --system --no-create-home --uid 10001 --gid 10001 --shell /usr/sbin/nologin cliproxy \
    && install -d -o 10001 -g 10001 -m 0700 \
        /var/lib/cliproxy /var/lib/cliproxy/oauth /run/cliproxy
USER 10001:10001

FROM ${GOLANG_IMAGE} AS build
ENV CGO_ENABLED=1 CC=gcc
ARG CLIPROXY_VERSION
ARG CLIPROXY_COMMIT
WORKDIR /src

# Fetch the release tag and prove that its peeled commit is the reviewed one.
# The tag is informational; checkout is always by the full immutable commit.
RUN git init \
    && git remote add origin https://github.com/router-for-me/CLIProxyAPI.git \
    && git fetch --depth=1 origin "refs/tags/${CLIPROXY_VERSION}:refs/tags/${CLIPROXY_VERSION}" \
    && test "$(git rev-parse "${CLIPROXY_VERSION}^{commit}")" = "${CLIPROXY_COMMIT}" \
    && git checkout --detach "${CLIPROXY_COMMIT}" \
    && test "$(git rev-parse HEAD)" = "${CLIPROXY_COMMIT}"

# The residual patch adds routing.strategy "plugin-only": a selector that
# rejects every pick, so a missing or misbehaving scheduler plugin fails closed
# instead of falling back to a built-in strategy. It must apply without fuzz.
ARG CLIPROXY_RESIDUAL_PATCH_SHA256
COPY deploy/codex-compat/residual/cliproxy-plugin-only-selector.patch /tmp/cliproxy-residual.patch
RUN printf "%s  /tmp/cliproxy-residual.patch\n" "$CLIPROXY_RESIDUAL_PATCH_SHA256" | sha256sum -c - \
    && git apply --check /tmp/cliproxy-residual.patch \
    && git apply /tmp/cliproxy-residual.patch \
    && git diff --check \
    && test "$(git status --porcelain | wc -l)" = 4
# The model catalog is the reviewed one embedded in the pinned source.
RUN printf '%s  internal/registry/models/models.json\n' \
    'e47744d81d04b5b7c8eee17057fd4d4110a8b8b5867d4015a0718e05d98b616c' | sha256sum -c -

# Security dependency floors, as ordinary module upgrades. A module upstream
# already requires at or above its floor is left alone: go get with an older
# version would downgrade it.
RUN set -eu; \
    for pin in \
        github.com/klauspost/compress@v1.18.7 \
        golang.org/x/crypto@v0.58.0 \
        golang.org/x/net@v0.62.0; do \
        module=${pin%@*}; floor=${pin#*@}; \
        current=$(go list -m -f '{{.Version}}' "$module"); \
        if test "$(printf '%s\n%s\n' "$current" "$floor" | sort -V | head -n 1)" != "$floor"; then \
            go get "$pin"; \
        fi; \
        test "$(printf '%s\n%s\n' "$(go list -m -f '{{.Version}}' "$module")" "$floor" | sort -V | head -n 1)" = "$floor"; \
    done; \
    go mod verify

RUN go test -count=1 -run '^TestGatewayPluginOnly' ./sdk/cliproxy/auth ./sdk/cliproxy \
    && go test -list '^TestGatewayPluginOnly' ./sdk/cliproxy/auth ./sdk/cliproxy > /tmp/residual-tests \
    && for test_name in \
        TestGatewayPluginOnlySelectorAlwaysRejects \
        TestGatewayPluginOnlySelectorIsNotBuiltIn \
        TestGatewayPluginOnlyRoutingStrategy; do \
        grep -Fxq "$test_name" /tmp/residual-tests; \
    done
# The plugin interface and the scheduler path it relies on must still pass
# upstream's own tests at this commit.
RUN go test -count=1 ./internal/pluginhost ./sdk/pluginapi ./sdk/pluginabi ./sdk/cliproxy/auth

RUN CGO_ENABLED=1 CC=gcc GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.Version=${CLIPROXY_VERSION} -X main.Commit=${CLIPROXY_COMMIT}" \
    -o /out/cli-proxy-api ./cmd/server

# Build the plugin against exactly this checkout, so its request and response
# types are the ones this CPA binary speaks.
COPY deploy/codex-compat/plugin /plugin
WORKDIR /plugin
RUN go mod edit -replace github.com/router-for-me/CLIProxyAPI/v8=/src \
    && test -z "$(gofmt -l .)" \
    && go vet ./... \
    && go vet -tags e2e ./... \
    && go test -race -count=1 ./... \
    && go build -trimpath -buildmode=c-shared -ldflags="-s -w" \
        -o /out/plugins/codex-gateway-v1.0.0.so .

# Run the shipped binary with the shipped plugin: OAuth credential files on
# disk, a fake Gateway and a TLS-intercepted provider, all on loopback. The
# suite must not skip, so a build without it cannot pass.
RUN CPA_E2E_BINARY=/out/cli-proxy-api CPA_E2E_PLUGIN_DIR=/out/plugins \
        go test -tags e2e -count=1 -v ./e2e > /tmp/e2e.log 2>&1 \
    || { tail -n 200 /tmp/e2e.log; exit 1; } \
    && ! grep -q -- '--- SKIP' /tmp/e2e.log \
    && for test_name in \
        TestAllocation \
        TestServerErrorMovesToNextAuthorizedAccount \
        TestRequestErrorIsNotReplayed \
        TestUsageLimitLocksAccountDurably \
        TestConcurrencyLimit \
        TestFailsClosed \
        TestPerAccountEgress \
        TestDeadEgressNeverFallsBack \
        TestCredentialOperations \
        TestModelIdentification \
        TestAccountManagement \
        TestManagementBoundary \
        TestCodexPassiveQuota \
        TestClaudePassiveQuota \
        TestAntigravityModelCatalog \
        TestProviderSessionsAreCallerScoped; do \
        grep -Fq -- "--- PASS: $test_name " /tmp/e2e.log; \
    done

FROM runtime-base
COPY --from=build --chown=10001:10001 --chmod=0555 /out/cli-proxy-api /usr/local/bin/cli-proxy-api
# Root-owned and read-only: the service user can load the plugin but cannot
# replace it, and CPA cannot install another one next to it.
COPY --from=build --chown=0:0 --chmod=0444 /out/plugins/codex-gateway-v1.0.0.so /usr/local/lib/cliproxy/plugins/codex-gateway-v1.0.0.so
# The entrypoint checks the credential store and writes CPA's configuration.
COPY --chown=10001:10001 --chmod=0555 deploy/codex-compat/plugin-entrypoint.sh /usr/local/bin/sidecar-entrypoint
COPY --chown=10001:10001 --chmod=0555 deploy/codex-compat/plugin-healthcheck.sh /usr/local/bin/sidecar-healthcheck

USER 10001:10001
EXPOSE 8317
ENTRYPOINT ["/usr/local/bin/sidecar-entrypoint"]
