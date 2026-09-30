ARG GOLANG_IMAGE=docker.io/library/golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
# Supply a reviewed registry digest or the commit-tagged bridge built in CI.
# A bare local sha256 image ID is not a valid FROM image reference.
# It provides the pinned AGY binary and Secret Service packages for rollback.
ARG LEGACY_BRIDGE_IMAGE
FROM ${GOLANG_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY cmd/cpa-credentials-migrate ./cmd/cpa-credentials-migrate
COPY cmd/cpa-pricing-migrate ./cmd/cpa-pricing-migrate
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cpa-credentials-migrate ./cmd/cpa-credentials-migrate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cpa-pricing-migrate ./cmd/cpa-pricing-migrate

FROM ${LEGACY_BRIDGE_IMAGE}
USER 0:0
COPY --from=build /out/cpa-credentials-migrate /out/cpa-pricing-migrate /usr/local/bin/
COPY deploy/cpa-migrate/keyring-helper.sh /usr/local/bin/cpa-keyring-helper
COPY deploy/cpa-migrate/entrypoint.sh /usr/local/bin/cpa-migrate-entrypoint
RUN chmod 0555 /usr/local/bin/cpa-credentials-migrate /usr/local/bin/cpa-pricing-migrate /usr/local/bin/cpa-keyring-helper /usr/local/bin/cpa-migrate-entrypoint
ENTRYPOINT ["/usr/local/bin/cpa-migrate-entrypoint"]
