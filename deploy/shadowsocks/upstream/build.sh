#!/bin/sh
set -eu

# Rebuild the pinned upstream with reviewed dependency fixes instead of copying
# the release image's older Go runtime and vulnerable modules.
test "$#" -eq 1 || { printf '%s\n' 'usage: build.sh OUTPUT' >&2; exit 1; }
upstream=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$upstream/source.lock.env"
export GOTOOLCHAIN=local
test "$(go env GOVERSION)" = "go$GO_VERSION" || {
    printf 'Mihomo requires the locked Go %s toolchain\n' "$GO_VERSION" >&2
    exit 1
}
mkdir -p "$(dirname -- "$1")"
output=$(CDPATH= cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
archive=$work/source.tar.gz
curl --fail --show-error --silent --location --retry 3 \
    "https://codeload.github.com/MetaCubeX/mihomo/tar.gz/$MIHOMO_COMMIT" \
    --output "$archive"
printf '%s  %s\n' "$MIHOMO_SOURCE_SHA256" "$archive" | sha256sum --check --strict -
printf '%s  %s\n' "$MIHOMO_PATCH_SHA256" "$upstream/mihomo-v1.19.32-dependencies.patch" | sha256sum --check --strict -
mkdir "$work/source"
tar --extract --gzip --file "$archive" --strip-components=1 --directory "$work/source"
cd "$work/source"
cmp LICENSE "$upstream/LICENSE"
git apply --check "$upstream/mihomo-v1.19.32-dependencies.patch"
git apply "$upstream/mihomo-v1.19.32-dependencies.patch"
# Match upstream's Docker build tag and baseline x86 instruction set. The
# module graph and checksums are committed in the patch; never resolve updates.
CGO_ENABLED=0 GOAMD64=v1 go build -mod=readonly -buildvcs=false -tags with_gvisor -trimpath \
    -ldflags "-s -w -buildid= -X github.com/metacubex/mihomo/constant.Version=$MIHOMO_VERSION-gateway.2" \
    -o "$output" .
