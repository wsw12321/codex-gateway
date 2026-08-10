ARG GOLANG_IMAGE=docker.io/library/golang:1.26.0-bookworm@sha256:2a0ba12e116687098780d3ce700f9ce3cb340783779646aafbabed748fa6677c
ARG RUNTIME_IMAGE=docker.io/library/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

FROM ${GOLANG_IMAGE} AS build
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

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.Version=${CLIPROXY_VERSION} -X main.Commit=${CLIPROXY_COMMIT}" \
    -o /out/cli-proxy-api ./cmd/server

FROM ${RUNTIME_IMAGE}
RUN addgroup -S -g 10001 cliproxy \
    && adduser -S -D -H -u 10001 -G cliproxy cliproxy \
    && install -d -o 10001 -g 10001 -m 0700 \
        /var/lib/cliproxy /var/lib/cliproxy/oauth /run/cliproxy

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
