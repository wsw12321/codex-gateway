#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

fail() {
    printf 'pull-ci-images: %s\n' "$*" >&2
    exit 1
}

test "$#" -eq 1 || fail 'usage: scripts/pull-ci-images.sh deployment-images.json'
manifest=$1
test -f "$manifest" || fail "manifest not found: $manifest"
command -v jq >/dev/null 2>&1 || fail 'jq is required'
command -v docker >/dev/null 2>&1 || fail 'Docker is required'

# Accept only the three digest-pinned application images from one GHCR repo.
jq -e '
  .schema_version == 1 and
  (.revision | type == "string" and test("^[0-9a-f]{40}$")) and
  (.repository | type == "string" and test("^[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9_.-]*$")) and
  .platform == "linux/amd64" and
  (.images | keys | sort) == ["antigravity-bridge", "codex-compat", "gateway"] and
  (.images | all(.[]; type == "string"))
' "$manifest" >/dev/null || fail 'invalid deployment manifest'

revision=$(jq -er '.revision' "$manifest")
repository=$(jq -er '.repository' "$manifest")
for service in gateway codex-compat antigravity-bridge; do
    jq -e --arg service "$service" --arg prefix "ghcr.io/$repository/$service@sha256:" '
      .images[$service] | startswith($prefix) and
      (ltrimstr($prefix) | test("^[0-9a-f]{64}$"))
    ' "$manifest" >/dev/null || fail "invalid digest reference for $service"
done

test "$(git -C "$root" rev-parse HEAD)" = "$revision" || \
    fail "check out commit $revision before importing its images"

config=$(mktemp)
trap 'rm -f "$config"' EXIT HUP INT TERM
"$root/scripts/compose.sh" config --format json > "$config"
jq -e --arg revision "$revision" '
  .services.gateway.build.args.REVISION == $revision and
  .services.gateway.build.args.VERSION == $revision and
  .services.gateway.image == ("codex-gateway-gateway:" + $revision)
' "$config" >/dev/null || \
    fail "set GATEWAY_IMAGE_TAG, GATEWAY_VERSION and GATEWAY_REVISION in .env to $revision"

# Finish every pull before changing the local tags used by Compose. Running
# containers are unaffected; deployment remains an explicit separate command.
for service in gateway codex-compat antigravity-bridge; do
    image=$(jq -er --arg service "$service" '.images[$service]' "$manifest")
    docker pull --platform linux/amd64 "$image"
    platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
    test "$platform" = linux/amd64 || fail "unexpected platform for $service: $platform"
done

for service in gateway codex-compat antigravity-bridge; do
    image=$(jq -er --arg service "$service" '.images[$service]' "$manifest")
    local_image=$(jq -er --arg service "$service" '.services[$service].image' "$config")
    docker tag "$image" "$local_image"
    printf '%s -> %s\n' "$image" "$local_image"
done

printf '\nImages for %s are ready. Follow docs/ci-image-deployment.md to deploy.\n' "$revision"
