#!/bin/sh
# Runtime checks of the plugin-mode CPA image. Synthetic keys and one
# synthetic credential only: no site secret, OAuth volume, external network or
# published port is used.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
lock=$root/deploy/codex-compat/plugin.lock.json
image=${1:-}
if test -z "$image"; then
    image=$(jq -er .image "$lock")
fi
# The probe checks that the binary reports the locked version and commit.
version=$(jq -er .tag "$lock")
commit=$(jq -er .commit "$lock")

# probe MODE PLUGIN_TMPFS_OPTIONS runs the probe under the deployment's mounts.
# The plugin's temporary directory is the only mount that differs.
probe() {
    docker run --rm --network none --read-only --cap-drop ALL \
        --security-opt no-new-privileges:true \
        --tmpfs /var/lib/cliproxy/oauth:rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001 \
        --tmpfs /run/cliproxy:rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001 \
        --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m,mode=0700,uid=10001,gid=10001 \
        --tmpfs "/run/cliproxy-plugin:$2" \
        --env TMPDIR=/run/cliproxy-plugin \
        --env CPA_PLUGIN_PROBE_VERSION="$version" \
        --env CPA_PLUGIN_PROBE_COMMIT="$commit" \
        --mount "type=bind,source=$root/scripts/tests/cpa-plugin-image-probe.sh,target=/probe.sh,readonly" \
        --entrypoint /bin/sh "$image" /probe.sh "$1"
}

# The mount from docker-compose.yml.
probe serving rw,exec,nosuid,nodev,size=64m,mode=0700,uid=10001,gid=10001
# Without it CPA cannot map the plugin; the image must then stay closed.
probe no-exec-tmp rw,noexec,nosuid,nodev,size=64m,mode=0700,uid=10001,gid=10001
