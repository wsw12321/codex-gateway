ARG GOLANG_IMAGE=docker.io/library/golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af
ARG RUNTIME_IMAGE=docker.io/library/alpine:3.22.6@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8

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
# The locked Alpine 3.22.6 base ships zlib 1.3.2-r0 (CVE-2026-85091, HIGH) and
# musl 1.2.5-r12 (CVE-2026-107891, HIGH). Keep the reviewed base and require the
# fixed zlib 1.3.2-r1 and musl/musl-utils 1.2.5-r13 from the 3.22 repository.
RUN apk upgrade --no-cache zlib musl musl-utils \
    && zlib_version="$(apk info -ev zlib)" \
    && test "$(apk version -t "${zlib_version#zlib-}" 1.3.2-r1)" != "<" \
    && musl_version="$(apk info -ev musl)" \
    && test "$(apk version -t "${musl_version#musl-}" 1.2.5-r13)" != "<" \
    && musl_utils_version="$(apk info -ev musl-utils)" \
    && test "$(apk version -t "${musl_utils_version#musl-utils-}" 1.2.5-r13)" != "<" \
    && addgroup -S -g 10001 gateway \
    && adduser -S -D -H -u 10001 -G gateway gateway
COPY --from=build --chown=10001:10001 /out/gateway /usr/local/bin/gateway

USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
