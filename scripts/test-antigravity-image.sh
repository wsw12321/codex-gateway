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
trap 'docker volume rm "$volume_name" >/dev/null' EXIT HUP INT TERM
probe() {
    docker run --rm --network none --read-only --cap-drop ALL --init \
        --security-opt no-new-privileges:true \
        --mount "type=volume,source=$volume_name,target=/var/lib/antigravity/keyrings" \
        --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
        --tmpfs /run/antigravity:rw,noexec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
        --tmpfs /run/secrets:rw,noexec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
        --entrypoint /bin/sh "$image" -eu -c '
            umask 077
            printf "%s" "$1" > /run/secrets/antigravity_keyring_password
            exec /usr/local/bin/antigravity-entrypoint verify-keyring
        ' sh "$1"
}
# Only public synthetic test passwords are passed in argv. No account,
# production secret, host mount, external network or published port is used.
probe synthetic-keyring-test-password
probe synthetic-keyring-test-password
if probe incorrect-synthetic-password >/dev/null 2>&1; then
    printf '%s\n' 'Bridge accepted an incorrect keyring password' >&2
    exit 1
fi
printf '%s\n' 'Antigravity keyring creation, container restart and incorrect-password rejection passed'
