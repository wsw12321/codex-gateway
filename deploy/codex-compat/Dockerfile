ARG GOLANG_IMAGE=docker.io/library/golang:1.26.0-bookworm@sha256:2a0ba12e116687098780d3ce700f9ce3cb340783779646aafbabed748fa6677c
ARG RUNTIME_IMAGE=docker.io/library/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

FROM ${GOLANG_IMAGE} AS build
ARG CLIPROXY_VERSION
ARG CLIPROXY_COMMIT
ARG GEMINI_PLUGIN_COMMIT=19d9868ffa24e94a2919ea1d1a761afa634de669
WORKDIR /src

# The host uses dlopen and the plugin exposes a C ABI. Both must target musl
# so the exact built artifacts can run together in the Alpine runtime.
RUN apt-get update \
    && apt-get install -y --no-install-recommends musl-tools \
    && rm -rf /var/lib/apt/lists/*

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

COPY deploy/codex-compat/cliproxy-v7.2.150-gemini.patch /tmp/cliproxy-gemini.patch
RUN git apply --check /tmp/cliproxy-gemini.patch \
    && git apply /tmp/cliproxy-gemini.patch \
    && git diff --check

RUN git init /gemini-plugin \
    && git -C /gemini-plugin remote add origin https://github.com/router-for-me/cpa-plugin-gemini-cli.git \
    && git -C /gemini-plugin fetch --depth=1 origin "${GEMINI_PLUGIN_COMMIT}" \
    && git -C /gemini-plugin checkout --detach "${GEMINI_PLUGIN_COMMIT}" \
    && test "$(git -C /gemini-plugin rev-parse HEAD)" = "${GEMINI_PLUGIN_COMMIT}"

COPY deploy/codex-compat/gemini-cli-19d9868-gateway.patch /tmp/gemini-plugin.patch
RUN git -C /gemini-plugin apply --check /tmp/gemini-plugin.patch \
    && git -C /gemini-plugin apply /tmp/gemini-plugin.patch \
    && git -C /gemini-plugin diff --check

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

RUN go test -count=1 ./internal/translator/gemini/openai/responses ./internal/pluginhost

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

# Reuse the reviewed host dependency, including its Gemini usage fix. The
# plugin's upstream module otherwise pins an older host translator release.
WORKDIR /gemini-plugin
RUN go mod edit -replace github.com/router-for-me/CLIProxyAPI/v7=/src \
    && go mod tidy \
    && go test -count=1 ./... \
    && go test -race -count=1 ./internal/executor ./internal/auth ./internal/translator \
    && go vet ./... \
    && CGO_ENABLED=1 CC=musl-gcc GOOS=linux go build \
        -trimpath -buildmode=c-shared -ldflags="-s -w" \
        -o /out/plugins/gemini-cli.so ./cmd/geminicli

WORKDIR /src
RUN CGO_ENABLED=1 CC=musl-gcc GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.Version=${CLIPROXY_VERSION} -X main.Commit=${CLIPROXY_COMMIT}" \
    -o /out/cli-proxy-api ./cmd/server

RUN CGO_ENABLED=1 CC=musl-gcc GOOS=linux go test -c \
    -o /out/pluginhost.test ./internal/pluginhost

FROM ${RUNTIME_IMAGE} AS verify
COPY --from=build /out/ /out/
RUN --network=none /out/pluginhost.test -test.list '^TestGeminiCLISharedPluginLoads$' \
    | grep -Fx 'TestGeminiCLISharedPluginLoads'
RUN --network=none GEMINI_PLUGIN_TEST_DIR=/out/plugins /out/pluginhost.test \
    -test.run '^TestGeminiCLISharedPluginLoads$' -test.count=1 -test.v

FROM ${RUNTIME_IMAGE}
RUN addgroup -S -g 10001 cliproxy \
    && adduser -S -D -H -u 10001 -G cliproxy cliproxy \
    && install -d -o 10001 -g 10001 -m 0700 \
        /var/lib/cliproxy /var/lib/cliproxy/oauth /run/cliproxy

COPY --from=verify --chown=10001:10001 /out/cli-proxy-api /usr/local/bin/cli-proxy-api
COPY --from=verify --chown=10001:10001 /out/plugins/gemini-cli.so /usr/local/lib/cliproxy/plugins/gemini-cli.so
COPY --chown=10001:10001 deploy/codex-compat/entrypoint.sh /usr/local/bin/sidecar-entrypoint
COPY --chown=10001:10001 deploy/codex-compat/healthcheck.sh /usr/local/bin/sidecar-healthcheck
COPY --chown=10001:10001 deploy/codex-compat/smoke.sh /usr/local/bin/sidecar-smoke
RUN chmod 0555 /usr/local/bin/cli-proxy-api \
        /usr/local/lib/cliproxy/plugins/gemini-cli.so \
        /usr/local/bin/sidecar-entrypoint \
        /usr/local/bin/sidecar-healthcheck \
        /usr/local/bin/sidecar-smoke

USER 10001:10001
EXPOSE 8317
ENTRYPOINT ["/usr/local/bin/sidecar-entrypoint"]
