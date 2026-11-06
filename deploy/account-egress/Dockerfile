ARG GOLANG_IMAGE
ARG RUNTIME_IMAGE
FROM ${GOLANG_IMAGE} AS mihomo
COPY deploy/shadowsocks/upstream /src/mihomo-build
RUN sh /src/mihomo-build/build.sh /out/mihomo

FROM ${GOLANG_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/egress ./internal/egress
COPY cmd/account-egress ./cmd/account-egress
RUN CGO_ENABLED=0 go test -count=1 ./cmd/account-egress \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /account-egress ./cmd/account-egress

FROM ${RUNTIME_IMAGE}
# The locked Alpine 3.22.6 base ships zlib 1.3.2-r0 (CVE-2026-85091, HIGH).
# Keep the reviewed base and require the fixed 1.3.2-r1 from the 3.22 repository.
RUN apk upgrade --no-cache zlib \
    && zlib_version="$(apk info -ev zlib)" \
    && test "$(apk version -t "${zlib_version#zlib-}" 1.3.2-r1)" != "<"
COPY --from=mihomo /out/mihomo /usr/local/bin/mihomo
COPY --from=build /account-egress /usr/local/bin/account-egress
COPY deploy/shadowsocks/upstream/LICENSE /usr/share/licenses/mihomo/LICENSE
USER 10004:10004
ENTRYPOINT ["/usr/local/bin/account-egress"]
