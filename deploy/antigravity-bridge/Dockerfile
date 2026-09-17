ARG GOLANG_IMAGE=docker.io/library/golang:1.24.6-bookworm@sha256:ab1d1823abb55a9504d2e3e003b75b36dbeb1cbcc4c92593d85a84ee46becc6c
ARG RUNTIME_IMAGE=docker.io/library/debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

FROM ${GOLANG_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/antigravity-bridge ./cmd/antigravity-bridge
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/antigravity-bridge ./cmd/antigravity-bridge

FROM ${RUNTIME_IMAGE} AS agy
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl jq \
    && rm -rf /var/lib/apt/lists/*
COPY deploy/antigravity-bridge/agy.lock.json /tmp/agy.lock.json
# Never run the mutable upstream installer. Both URL and checksum are reviewed
# inputs; the upstream manifest is provenance only and is not fetched at build.
RUN test "$(dpkg --print-architecture)" = amd64 \
    && curl --proto '=https' --tlsv1.2 -fsSL "$(jq -er .url /tmp/agy.lock.json)" -o /tmp/agy.tar.gz \
    && printf '%s  %s\n' "$(jq -er .sha512 /tmp/agy.lock.json)" /tmp/agy.tar.gz | sha512sum -c - \
    && mkdir /out && tar -xzf /tmp/agy.tar.gz -C /out antigravity \
    && mv /out/antigravity /out/agy && chmod 0555 /out/agy \
    && test "$(AGY_CLI_DISABLE_AUTO_UPDATE=true /out/agy --version)" = "$(jq -er .version /tmp/agy.lock.json)"

FROM ${RUNTIME_IMAGE}
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates dbus-daemon dbus-bin gnome-keyring libglib2.0-bin curl jq \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10002 antigravity \
    && useradd --system --no-create-home --uid 10002 --gid 10002 --shell /usr/sbin/nologin antigravity \
    && install -d -o 10002 -g 10002 -m 0700 /var/lib/antigravity /var/lib/antigravity/keyrings /run/antigravity
COPY --from=build /out/antigravity-bridge /usr/local/bin/antigravity-bridge
COPY --from=agy /out/agy /usr/local/bin/agy
COPY deploy/antigravity-bridge/entrypoint.sh /usr/local/bin/antigravity-entrypoint
COPY deploy/antigravity-bridge/smoke.sh /usr/local/bin/antigravity-smoke
COPY deploy/antigravity-bridge/settings.json /etc/antigravity/settings.json
RUN chmod 0555 /usr/local/bin/antigravity-bridge /usr/local/bin/agy /usr/local/bin/antigravity-entrypoint /usr/local/bin/antigravity-smoke
ENV HOME=/tmp/antigravity-home XDG_DATA_HOME=/var/lib/antigravity XDG_RUNTIME_DIR=/run/antigravity \
    AGY_CLI_DISABLE_AUTO_UPDATE=true
USER 10002:10002
EXPOSE 8318
ENTRYPOINT ["/usr/local/bin/antigravity-entrypoint"]
