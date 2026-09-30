#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${CPA_MIGRATION_TEST_IMAGE:-codex-gateway-cpa-migrate:validation}

# Synthetic credentials only. No production volumes or external network.
docker run --rm --pull=never --network none --read-only --user 0:0 \
    --cap-drop ALL --cap-add SETUID --cap-add SETGID --cap-add CHOWN \
    --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add KILL \
    --security-opt no-new-privileges:true \
    --tmpfs /run/cpa-migrate:rw,noexec,nosuid,nodev,mode=0700 \
    --tmpfs /run/secrets:rw,noexec,nosuid,nodev,mode=0755 \
    --tmpfs /oauth:rw,noexec,nosuid,nodev,mode=0700 \
    --tmpfs /var/lib/antigravity/keyrings:rw,noexec,nosuid,nodev,mode=0700,uid=10002,gid=10002 \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=1777 \
    --mount "type=bind,source=$root/scripts/tests/cpa-migration-image-probe.sh,target=/probe.sh,readonly" \
    --entrypoint /bin/sh "$image" /probe.sh
