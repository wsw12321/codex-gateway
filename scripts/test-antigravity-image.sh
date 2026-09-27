#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${1:-}
if test -z "$image"; then
    image=$("$root/scripts/compose.sh" config --format json | jq -er '.services["antigravity-bridge"].image')
fi
volume_name=codex-antigravity-keyring-test-$(date +%s)-$$
docker volume create "$volume_name" >/dev/null
cleanup() {
    docker volume rm "$volume_name" >/dev/null
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
probe() {
    docker run --rm -i --network none --read-only --cap-drop ALL --init \
        --security-opt no-new-privileges:true \
        --mount "type=volume,source=$volume_name,target=/var/lib/antigravity/keyrings" \
        --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
        --tmpfs /run/antigravity:rw,noexec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
        --tmpfs /run/secrets:rw,noexec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
        --tmpfs /run/antigravity-probe:rw,exec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
        --env TERM=dumb --entrypoint /bin/sh "$image" -eu -s -- "$1" "$2" \
        < "$root/scripts/tests/antigravity-image-probe.sh"
}
# Only public synthetic test passwords are passed in argv. No account,
# production secret, host mount, external network or published port is used.
# The extra executable tmpfs contains only the fake CLI. Request HOME and
# secrets retain their production noexec restrictions. Every probe gets new
# tmpfs filesystems and a new HOME; only the encrypted keyring volume survives.
probe import synthetic-keyring-test-password
probe refresh synthetic-keyring-test-password
probe restored synthetic-keyring-test-password
probe wrong-password incorrect-synthetic-password
probe ciphertext synthetic-keyring-test-password
printf '%s\n' 'Antigravity synthetic credential import, refresh, fresh-container restore, encrypted persistence and incorrect-password rejection passed'
