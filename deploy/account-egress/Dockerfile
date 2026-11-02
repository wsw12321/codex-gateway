ARG MIHOMO_IMAGE
ARG GOLANG_IMAGE
ARG RUNTIME_IMAGE
FROM ${MIHOMO_IMAGE} AS mihomo
FROM ${GOLANG_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/egress ./internal/egress
COPY cmd/account-egress ./cmd/account-egress
RUN CGO_ENABLED=0 go test -count=1 ./cmd/account-egress \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /account-egress ./cmd/account-egress

FROM ${RUNTIME_IMAGE}
COPY --from=mihomo /mihomo /usr/local/bin/mihomo
COPY --from=build /account-egress /usr/local/bin/account-egress
COPY deploy/shadowsocks/upstream/LICENSE /usr/share/licenses/mihomo/LICENSE
USER 10004:10004
ENTRYPOINT ["/usr/local/bin/account-egress"]
