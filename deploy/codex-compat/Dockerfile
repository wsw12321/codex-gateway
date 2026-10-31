ARG GOLANG_IMAGE=docker.io/library/golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af
ARG RUNTIME_IMAGE=docker.io/library/debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

FROM ${RUNTIME_IMAGE} AS runtime-base
# Keep the reviewed glibc runtime. Netcat powers
# the TCP probes without placing the internal API key in process arguments.
# Explicitly upgrade perl-base: the pinned base predates DLA-4821-1.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libpcre2-8-0 netcat-openbsd perl-base util-linux \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' perl-base)" ge '5.36.0-7+deb12u4' \
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

ARG CLIPROXY_PATCH_SHA256
COPY deploy/codex-compat/cliproxy-v8.0.48-gateway.patch /tmp/cliproxy-multi-account.patch
RUN printf "%s  /tmp/cliproxy-multi-account.patch\n" "$CLIPROXY_PATCH_SHA256" | sha256sum -c - \
    && git apply --check --ignore-space-change /tmp/cliproxy-multi-account.patch \
    && git apply --ignore-space-change /tmp/cliproxy-multi-account.patch \
    && git diff --check
RUN printf '%s  internal/registry/models/models.json\n' \
    '32f663de911083339cf5347be74b0b4cbf1471bc1968362897cc94ebf021c306' | sha256sum -c -

COPY deploy/codex-compat/legacy-credentials.test.txt /src/internal/watcher/synthesizer/gateway_legacy_credentials_test.go
COPY deploy/codex-compat/entrypoint.sh /tmp/gateway-entrypoint.sh
# The v8.0.48 source no longer carries the v8.0.30 testdata fixture; keep
# the deployed template syntax-checked in the immutable image build.
RUN test -s /tmp/gateway-entrypoint.sh && sh -n /tmp/gateway-entrypoint.sh

# The reviewed patch carries its security regression tests. A context mismatch
# or semantic regression therefore fails the immutable image build closed.
RUN go test -count=1 \
    ./internal/config \
    ./internal/cache \
    ./internal/api \
    ./internal/api/middleware \
    ./internal/auth/codex \
    ./internal/auth/antigravity \
    ./internal/auth/claude \
    ./internal/api/handlers/management \
    ./internal/runtime/executor \
    ./internal/runtime/executor/helps \
    ./internal/registry \
    ./internal/store \
    ./internal/thinking/... \
    ./internal/translator/... \
    ./internal/logging \
    ./internal/util \
    ./internal/watcher \
    ./internal/watcher/synthesizer \
    ./sdk/api/handlers \
    ./sdk/api/handlers/openai \
    ./sdk/api/handlers/claude \
    ./sdk/auth \
    ./sdk/cliproxy/auth \
    ./sdk/cliproxy/session \
    ./sdk/cliproxy

# Concurrency is part of the selection contract: the reverse callback must not
# hold manager locks, and simultaneous first requests must share one binding.
# Watcher revisions must also preserve canonical account identity under updates.
RUN go test -race -count=1 ./sdk/cliproxy/auth ./internal/watcher ./sdk/cliproxy
# v8.0.48 stream finalization and asynchronous usage must not retain pooled
# request state or let late interrupts affect the next response.
RUN go test -race -count=1 ./internal/api/middleware ./internal/cache \
    ./internal/runtime/executor/helps ./sdk/cliproxy/usage ./sdk/api/handlers/openai
RUN go test -list '^TestGateway' ./sdk/cliproxy/auth > /tmp/credential-regression-tests \
    && for test_name in \
        TestGatewayRefreshConcurrentCredentialAndControlChanges \
        TestGatewayControlChangeDuringCredentialSave \
        TestGatewayRefreshFailureWaitsForReauthorizationSave \
        TestGatewayLateExecutionResultsRespectAllVersions \
        TestGatewayCatalogPublicationSerializesCredentialMutation \
        TestGatewayLateUnauthorizedDoesNotRefreshReplacement \
        TestGatewayExecutionRequestUsesTrustedCallerAndAccountScope; do \
        grep -Fxq "$test_name" /tmp/credential-regression-tests; \
    done
# Catalog publication is implemented by the gateway auth manager, while the
# service package owns the dynamic catalog/fencing paths.  Keep this inventory
# tied to the tests that are actually present in those packages rather than
# asserting stale names from an earlier patch series.
RUN go test -list '^TestGatewayCatalog' ./sdk/cliproxy/auth > /tmp/gateway-catalog-regression-tests \
    && grep -Fxq TestGatewayCatalogPublicationSerializesCredentialMutation /tmp/gateway-catalog-regression-tests \
    && go test -list '^(TestAntigravityDynamicRegistrationDropsStaleFullCatalog|TestAntigravityUnknownCatalogPreservesRestoredCooldown|TestServiceCatalogStartupAndConfigReload)$' ./sdk/cliproxy > /tmp/catalog-regression-tests \
    && for test_name in \
        TestAntigravityDynamicRegistrationDropsStaleFullCatalog \
        TestAntigravityUnknownCatalogPreservesRestoredCooldown \
        TestServiceCatalogStartupAndConfigReload; do \
        grep -Fxq "$test_name" /tmp/catalog-regression-tests; \
    done

RUN go test -list '^(TestCodex|TestAuditHomeCodex426)' ./internal/runtime/executor > /tmp/codex-regression-tests \
    && for test_name in \
        TestCodexRejectRedirectReturnsOriginalResponse \
        TestCodexHttpRequestDoesNotFollowRedirect \
        TestCodexImageHTTPClientDoesNotFollowRedirect \
        TestCodexInternalAndOAuthHeadersAreProtectedOnAllTransports \
        TestCodexCountTokensDoesNotSendPrivateHeaders \
        TestCodexAstraUpgradeHTTPTransports \
        TestCodexDiagnosticProbeIsolatedRequestAndNoUsage \
        TestCodexDiagnosticProbeResponsesLiteContract \
        TestCodexDiagnosticProbeMissingContentType \
        TestCodexDiagnosticProbeUpstreamErrorsAndNoRetry \
        TestCodexDiagnosticProbeSSEValidation \
        TestCodexDiagnosticProbeIdentityNetworkAndCancellation \
        TestCodexDiagnosticProbePreservesEgressProxy \
        TestCodexDiagnosticProbeRejectsInvalidProxy \
        TestCodexDiagnosticProbeCancelsStalledBody \
        TestCodexDiagnosticProbeDoesNotDuplicateStreamingText \
        TestCodexGatewayQuotaSignal \
        TestCodexGatewayQuotaHTTPTransports \
        TestCodexGatewayQuotaWebsocketSignal \
        TestCodexGatewayTerminalAuthErrorDoesNotLockQuota \
        TestCodexGatewayDuplexQuotaPersists \
        TestCodexGatewayControlQuotaPersistsAcrossExecutionModes \
        TestCodexGatewayControlKeepsInFlightRequestsAndRejectsStaleQuota \
        TestAuditHomeCodex426WebsocketToHTTPFreshSelection; do \
        grep -Fxq "$test_name" /tmp/codex-regression-tests; \
    done \
    && go test -count=1 \
        -run '^(TestCodex(DiagnosticProbe.*|RejectRedirectReturnsOriginalResponse|HttpRequestDoesNotFollowRedirect|ImageHTTPClientDoesNotFollowRedirect|InternalAndOAuthHeadersAreProtectedOnAllTransports|CountTokensDoesNotSendPrivateHeaders|AstraUpgradeHTTPTransports|GatewayQuotaSignal|GatewayQuotaHTTPTransports|GatewayQuotaWebsocketSignal|GatewayTerminalAuthErrorDoesNotLockQuota|GatewayDuplexQuotaPersists|GatewayControlQuotaPersistsAcrossExecutionModes|GatewayControlKeepsInFlightRequestsAndRejectsStaleQuota)|TestAuditHomeCodex426WebsocketToHTTPFreshSelection)$' \
        ./internal/runtime/executor

RUN go test -list '^TestGateway(Claude|Anthropic)' ./internal/runtime/executor > /tmp/claude-cache-regression-tests \
    && for test_name in \
        TestGatewayClaudeToolAliasesIsolateCallerProviderAndAccount \
        TestGatewayClaudeToolAliasesSurviveExecuteAndStreamingContinuation \
        TestGatewayClaudeToolAliasesDoNotPersistInterruptedOrCancelledStreams \
        TestGatewayClaudeToolAliasesMissingStateDoesNotRotateAccounts \
        TestGatewayAnthropicNativeAndCloakedHeaderBoundaries; do \
        grep -Fxq "$test_name" /tmp/claude-cache-regression-tests; \
    done \
    && go test -count=1 -run '^(TestClaude|TestGatewayAnthropic|TestGatewayClaude)' ./internal/runtime/executor
RUN go test -race -count=1 -run '^TestGatewayClaude' ./internal/runtime/executor

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
