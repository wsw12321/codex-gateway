ARG GOLANG_IMAGE=docker.io/library/golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
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
