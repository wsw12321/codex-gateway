ARG MIHOMO_IMAGE
ARG GOLANG_IMAGE
ARG RUNTIME_IMAGE
FROM ${MIHOMO_IMAGE} AS mihomo
FROM ${GOLANG_IMAGE} AS builder
WORKDIR /src
COPY deploy/shadowsocks/main.go ./main.go
RUN CGO_ENABLED=0 GO111MODULE=off go build -trimpath -ldflags="-s -w" -o /ss-egress-helper main.go

# Copy only the upstream binary; use our reviewed base and no root-home volume,
# geodata downloads, package installation, controller, or TUN setup.
FROM ${RUNTIME_IMAGE}
COPY --from=mihomo /mihomo /usr/local/bin/mihomo
COPY --from=builder /ss-egress-helper /usr/local/bin/ss-egress-helper
COPY deploy/shadowsocks/upstream/LICENSE /usr/share/licenses/mihomo/LICENSE
USER 10003:10003
ENTRYPOINT ["/usr/local/bin/ss-egress-helper"]
