ARG GOLANG_IMAGE=docker.io/library/golang:1.26.0-bookworm@sha256:2a0ba12e116687098780d3ce700f9ce3cb340783779646aafbabed748fa6677c
ARG RUNTIME_IMAGE=docker.io/library/debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

FROM ${RUNTIME_IMAGE} AS runtime-base
# Keep the reviewed glibc runtime. Netcat powers
# the TCP probes without placing the internal API key in process arguments.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates netcat-openbsd \
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

# Preserve the reviewed Debian runtime and pinned host build.

# Fetch the release tag and prove that its peeled commit is the reviewed one.
# The tag is informational; checkout is always by the full immutable commit.
RUN git init \
    && git remote add origin https://github.com/router-for-me/CLIProxyAPI.git \
    && git fetch --depth=1 origin "refs/tags/${CLIPROXY_VERSION}:refs/tags/${CLIPROXY_VERSION}" \
    && test "$(git rev-parse "${CLIPROXY_VERSION}^{commit}")" = "${CLIPROXY_COMMIT}" \
    && git checkout --detach "${CLIPROXY_COMMIT}" \
    && test "$(git rev-parse HEAD)" = "${CLIPROXY_COMMIT}"

COPY deploy/codex-compat/cliproxy-v7.2.150-multi-account.patch /tmp/cliproxy-multi-account.patch
RUN git apply --check --ignore-space-change /tmp/cliproxy-multi-account.patch \
    && git apply --ignore-space-change /tmp/cliproxy-multi-account.patch \
    && git diff --check

COPY deploy/codex-compat/legacy-credentials.test.txt /src/internal/watcher/synthesizer/gateway_legacy_credentials_test.go

# The reviewed patch carries its security regression tests. A context mismatch
# or semantic regression therefore fails the immutable image build closed.
RUN go test -count=1 \
    ./internal/api \
    ./internal/auth/codex \
    ./internal/logging \
    ./internal/watcher \
    ./internal/watcher/synthesizer \
    ./sdk/api/handlers \
    ./sdk/auth \
    ./sdk/cliproxy/auth \
    ./sdk/cliproxy

RUN go test -list '^(TestCodex|TestAuditHomeCodex426)' ./internal/runtime/executor > /tmp/codex-regression-tests \
    && for test_name in \
        TestCodexRejectRedirectReturnsOriginalResponse \
        TestCodexHttpRequestDoesNotFollowRedirect \
        TestCodexImageHTTPClientDoesNotFollowRedirect \
        TestCodexInternalAndOAuthHeadersAreProtectedOnAllTransports \
        TestCodexAstraUpgradeHTTPTransports \
        TestCodexGatewayQuotaSignal \
        TestCodexGatewayQuotaHTTPTransports \
        TestCodexGatewayControlQuotaPersistsAcrossExecutionModes \
        TestCodexGatewayControlKeepsInFlightRequestsAndRejectsStaleQuota \
        TestAuditHomeCodex426WebsocketToHTTPFreshSelection; do \
        grep -Fxq "$test_name" /tmp/codex-regression-tests; \
    done \
    && go test -count=1 \
        -run '^(TestCodex(RejectRedirectReturnsOriginalResponse|HttpRequestDoesNotFollowRedirect|ImageHTTPClientDoesNotFollowRedirect|InternalAndOAuthHeadersAreProtectedOnAllTransports|AstraUpgradeHTTPTransports|GatewayQuotaSignal|GatewayQuotaHTTPTransports|GatewayControlQuotaPersistsAcrossExecutionModes|GatewayControlKeepsInFlightRequestsAndRejectsStaleQuota)|TestAuditHomeCodex426WebsocketToHTTPFreshSelection)$' \
        ./internal/runtime/executor

WORKDIR /src
RUN CGO_ENABLED=1 CC=gcc GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.Version=${CLIPROXY_VERSION} -X main.Commit=${CLIPROXY_COMMIT}" \
    -o /out/cli-proxy-api ./cmd/server

FROM runtime-base
COPY --from=build --chown=10001:10001 /out/cli-proxy-api /usr/local/bin/cli-proxy-api
COPY --chown=10001:10001 deploy/codex-compat/entrypoint.sh /usr/local/bin/sidecar-entrypoint
COPY --chown=10001:10001 deploy/codex-compat/healthcheck.sh /usr/local/bin/sidecar-healthcheck
COPY --chown=10001:10001 deploy/codex-compat/smoke.sh /usr/local/bin/sidecar-smoke
RUN chmod 0555 /usr/local/bin/cli-proxy-api \
        /usr/local/bin/sidecar-entrypoint \
        /usr/local/bin/sidecar-healthcheck \
        /usr/local/bin/sidecar-smoke

USER 10001:10001
EXPOSE 8317
ENTRYPOINT ["/usr/local/bin/sidecar-entrypoint"]
