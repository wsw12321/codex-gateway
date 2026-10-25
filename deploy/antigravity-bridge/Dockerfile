ARG GOLANG_IMAGE=docker.io/library/golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af
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
# Debian now publishes the fixed Expat binary. Require the security updates
# for Expat, Brotli and jq as well as the inherited Perl package (DLA-4821-1).
RUN apt-get update \
    && apt-get install -y --no-install-recommends libexpat1 libbrotli1 ca-certificates dbus-daemon dbus-bin gnome-keyring libsecret-tools libglib2.0-bin libpcre2-8-0 curl jq libjq1 perl-base util-linux \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' libexpat1)" ge '2.5.0-1+deb12u5' \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' libbrotli1)" ge '1.0.9-2+deb12u1' \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' jq)" ge '1.6-2.1+deb12u3' \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' libjq1)" ge '1.6-2.1+deb12u3' \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' perl-base)" ge '5.36.0-7+deb12u4' \
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
