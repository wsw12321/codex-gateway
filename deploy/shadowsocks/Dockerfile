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
# geodata downloads, package installation, controller, or TUN setup.
FROM ${RUNTIME_IMAGE}
COPY --from=mihomo /out/mihomo /usr/local/bin/mihomo
COPY --from=builder /ss-egress-helper /usr/local/bin/ss-egress-helper
COPY deploy/shadowsocks/upstream/LICENSE /usr/share/licenses/mihomo/LICENSE
USER 10003:10003
ENTRYPOINT ["/usr/local/bin/ss-egress-helper"]
