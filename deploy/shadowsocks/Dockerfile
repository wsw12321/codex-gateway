ARG GOLANG_IMAGE
ARG RUNTIME_IMAGE
FROM ${GOLANG_IMAGE} AS mihomo
COPY deploy/shadowsocks/upstream /src/mihomo-build
RUN sh /src/mihomo-build/build.sh /out/mihomo

FROM ${GOLANG_IMAGE} AS builder
WORKDIR /src
COPY deploy/shadowsocks/main.go ./main.go
RUN CGO_ENABLED=0 GO111MODULE=off go build -trimpath -ldflags="-s -w" -o /ss-egress-helper main.go

# Copy only the rebuilt binary; use our reviewed base and no root-home volume,
# geodata downloads, controller, or TUN setup. The only package changes are the
# zlib, musl and OpenSSL security updates below.
FROM ${RUNTIME_IMAGE}
# The locked Alpine 3.22.6 base ships zlib 1.3.2-r0 (CVE-2026-85091, HIGH),
# musl 1.2.5-r12 (CVE-2026-107891, HIGH) and OpenSSL libcrypto3/libssl3 3.5.8-r0
# (CVE-2026-124709, HIGH). Keep the reviewed base and require the fixed zlib
# 1.3.2-r1, musl/musl-utils 1.2.5-r13 and libcrypto3/libssl3 3.5.9-r2 from the
# 3.22 repository.
RUN apk upgrade --no-cache zlib musl musl-utils libcrypto3 libssl3 \
    && zlib_version="$(apk info -ev zlib)" \
    && test "$(apk version -t "${zlib_version#zlib-}" 1.3.2-r1)" != "<" \
    && musl_version="$(apk info -ev musl)" \
    && test "$(apk version -t "${musl_version#musl-}" 1.2.5-r13)" != "<" \
    && musl_utils_version="$(apk info -ev musl-utils)" \
    && test "$(apk version -t "${musl_utils_version#musl-utils-}" 1.2.5-r13)" != "<" \
    && libcrypto_version="$(apk info -ev libcrypto3)" \
    && test "$(apk version -t "${libcrypto_version#libcrypto3-}" 3.5.9-r2)" != "<" \
    && libssl_version="$(apk info -ev libssl3)" \
    && test "$(apk version -t "${libssl_version#libssl3-}" 3.5.9-r2)" != "<"
COPY --from=mihomo /out/mihomo /usr/local/bin/mihomo
COPY --from=builder /ss-egress-helper /usr/local/bin/ss-egress-helper
COPY deploy/shadowsocks/upstream/LICENSE /usr/share/licenses/mihomo/LICENSE
USER 10003:10003
ENTRYPOINT ["/usr/local/bin/ss-egress-helper"]
