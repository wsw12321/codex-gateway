#!/bin/sh
set -eu
umask 077

test "$#" -eq 0 || { printf '%s\n' 'usage: apply-egress.sh' >&2; exit 1; }
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
. "$root/scripts/egress-common.sh"
egress_prepare "$root"
command -v flock >/dev/null 2>&1 || egress_fail 'flock from util-linux is required'

lock_file=$root/.egress-apply.lock
if test -e "$lock_file" || test -L "$lock_file"; then
    test -f "$lock_file" && test ! -L "$lock_file" || egress_fail 'operation lock must be a regular non-symlink file'
fi
exec 9>"$lock_file"
chmod 0600 "$lock_file"
flock -n 9 || egress_fail 'another egress switch is in progress'

work_dir=$(mktemp -d /tmp/apply-egress.XXXXXX)
trap 'rm -rf "$work_dir"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
egress_auto_oidc=true
# Freeze the fully resolved model before any container changes. Compose emits
# absolute bind paths and already escapes dollars for a second Compose load.
egress_compose config --format json > "$work_dir/rendered.json"
jq -e '.name == "codex-gateway" and .services["egress-allowlist"] != null' \
    "$work_dir/rendered.json" >/dev/null || egress_fail 'invalid Compose deployment model'
project=$(jq -r '.name' "$work_dir/rendered.json")
rendered_settings=$(python3 "$root/scripts/validate-egress.py" --root "$root" --deployment \
    < "$work_dir/rendered.json")
test "$rendered_settings" = "$egress_settings" || \
    egress_fail '.env routing settings changed during preflight; retry the operation'
cp "$work_dir/rendered.json" "$work_dir/compose.json"

apply_compose() {
    COMPOSE_REMOVE_ORPHANS=false docker compose --project-name "$project" --project-directory "$root" \
        --env-file /dev/null \
        -f "$work_dir/compose.json" "$@"
}

# Re-parse the frozen model as a final preflight. No existing container has
# been stopped, created, or removed if any of the above checks fail.
apply_compose config --quiet
if test "$egress_mode" = shadowsocks; then
    printf '%s\n' 'Building the locked Shadowsocks egress image...'
    apply_compose build ss-egress
    printf '%s\n' 'Recreating Shadowsocks egress and waiting for its local proxy...'
    apply_compose up -d --no-deps --force-recreate --wait --wait-timeout 90 ss-egress
fi
printf 'Recreating Squid with %s egress and waiting for local readiness...\n' "$egress_mode"
apply_compose up -d --no-deps --force-recreate --wait --wait-timeout 90 egress-allowlist

if test "$egress_mode" != shadowsocks; then
    # Only remove the old service after Squid is healthy on its new route.
    # Both project and service labels must match; never remove other orphans.
    old_containers=$(docker container ls -aq \
        --filter "label=com.docker.compose.project=$project" \
        --filter 'label=com.docker.compose.service=ss-egress')
    for container_id in $old_containers; do
        case "$container_id" in
            ''|*[!a-f0-9]*) egress_fail 'Docker returned an invalid container ID' ;;
        esac
        docker container rm -f "$container_id" >/dev/null
    done
fi
printf 'Egress mode %s applied.\n' "$egress_mode"
