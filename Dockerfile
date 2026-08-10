ARG GOLANG_IMAGE=docker.io/library/golang:1.24.6-bookworm@sha256:ab1d1823abb55a9504d2e3e003b75b36dbeb1cbcc4c92593d85a84ee46becc6c
ARG RUNTIME_IMAGE=docker.io/library/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

FROM ${GOLANG_IMAGE} AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG REVISION=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
    -o /out/gateway ./cmd/gateway

FROM ${RUNTIME_IMAGE}
RUN addgroup -S -g 10001 gateway \
    && adduser -S -D -H -u 10001 -G gateway gateway
COPY --from=build --chown=10001:10001 /out/gateway /usr/local/bin/gateway

USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
