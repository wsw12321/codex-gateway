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
# The locked Alpine 3.22.6 base ships zlib 1.3.2-r0 (CVE-2026-85091, HIGH) and
# musl 1.2.5-r12 (CVE-2026-107891, HIGH). Keep the reviewed base and require the
# fixed zlib 1.3.2-r1 and musl/musl-utils 1.2.5-r13 from the 3.22 repository.
RUN apk upgrade --no-cache zlib musl musl-utils \
    && zlib_version="$(apk info -ev zlib)" \
    && test "$(apk version -t "${zlib_version#zlib-}" 1.3.2-r1)" != "<" \
    && musl_version="$(apk info -ev musl)" \
    && test "$(apk version -t "${musl_version#musl-}" 1.2.5-r13)" != "<" \
    && musl_utils_version="$(apk info -ev musl-utils)" \
    && test "$(apk version -t "${musl_utils_version#musl-utils-}" 1.2.5-r13)" != "<"
COPY --from=mihomo /out/mihomo /usr/local/bin/mihomo
COPY --from=build /account-egress /usr/local/bin/account-egress
COPY deploy/shadowsocks/upstream/LICENSE /usr/share/licenses/mihomo/LICENSE
USER 10004:10004
ENTRYPOINT ["/usr/local/bin/account-egress"]
