#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_cmd=$root/scripts/compose.sh
lock_file=$root/.device-login.lock
sidecar_needs_stop=0
work_dir=
inventory_before=
inventory_after=

compose() {
    "$compose_cmd" "$@"
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if test "$sidecar_needs_stop" -eq 1; then
        if ! compose stop -t 10 codex-compat >/dev/null 2>&1; then
            printf '%s\n' 'codex-device-login: failed to stop unverified sidecar during cleanup' >&2
            test "$status" -ne 0 || status=1
        fi
    fi
    if test -n "$work_dir" && test -d "$work_dir"; then
        rm -f "$work_dir/inventory.raw" "$inventory_before" "$inventory_after"
        if ! rmdir "$work_dir"; then
            printf '%s\n' 'codex-device-login: failed to remove private inventory directory' >&2
            test "$status" -ne 0 || status=1
        fi
    fi
    if ! flock -u 9; then
        printf '%s\n' 'codex-device-login: failed to release operation lock' >&2
        test "$status" -ne 0 || status=1
    fi
    exit "$status"
}

command -v flock >/dev/null 2>&1 || {
    printf '%s\n' 'codex-device-login: flock from util-linux is required' >&2
    exit 1
}
if test -e "$lock_file" || test -L "$lock_file"; then
    test -f "$lock_file" && test ! -L "$lock_file" || {
        printf '%s\n' 'codex-device-login: lock path must be a regular file' >&2
        exit 1
    }
fi
exec 9>"$lock_file"
chmod 0600 "$lock_file"
if ! flock -n 9; then
    printf '%s\n' 'codex-device-login: another login or upgrade operation holds the lock' >&2
    exit 1
fi
work_dir=$(mktemp -d /tmp/codex-device-login.XXXXXX)
inventory_before=$work_dir/before
inventory_after=$work_dir/after
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

capture_oauth_inventory() {
    inventory_destination=$1
    inventory_raw=$work_dir/inventory.raw
    if ! compose run --rm --no-deps codex-compat oauth-inventory > "$inventory_raw"; then
        printf '%s\n' 'codex-device-login: failed to inventory OAuth accounts' >&2
        return 1
    fi
    if ! awk '
        NF != 2 || length($1) != 64 || length($2) != 64 ||
            $1 !~ /^[0-9a-f]+$/ || $2 !~ /^[0-9a-f]+$/ || seen[$1]++ { exit 1 }
    ' "$inventory_raw"; then
        printf '%s\n' 'codex-device-login: invalid OAuth inventory response' >&2
        return 1
    fi
    LC_ALL=C sort "$inventory_raw" > "$inventory_destination"
    rm -f "$inventory_raw"
}

verify_single_account_mutation() {
    if ! awk '
        FILENAME == ARGV[1] { before[$1] = $2; next }
        { after[$1] = $2; if (!($1 in before)) added++; else if (before[$1] != $2) changed++ }
        END {
            for (key in before) if (!(key in after)) removed++
            if (removed != 0 || added + changed != 1) exit 1
        }
    ' "$inventory_before" "$inventory_after"; then
        printf '%s\n' 'codex-device-login: expected exactly one added or refreshed account and no removed accounts' >&2
        return 1
    fi
}

printf '%s\n' 'Stopping the only sidecar instance before one-account OAuth mutation...'
compose stop -t 30 codex-compat

cid=$(compose ps -q codex-compat || true)
if test -n "$cid" && test "$(docker inspect -f '{{.State.Running}}' "$cid")" = true; then
    printf '%s\n' 'codex-device-login: sidecar is still running; refusing login' >&2
    exit 1
fi

# A stopped container keeps its static Compose IP. Remove only that container
# (the named OAuth volume remains) so the single temporary login container can
# use the service network without an address collision.
compose rm -f codex-compat

compose up -d egress-allowlist
capture_oauth_inventory "$inventory_before"

printf '%s\n' 'Starting interactive Codex device login. OAuth values are never printed by this wrapper.'
if ! compose run --rm --no-deps codex-compat --codex-device-login; then
    printf '%s\n' 'codex-device-login: login failed; sidecar remains stopped' >&2
    exit 1
fi

if ! compose run --rm --no-deps codex-compat verify-oauth; then
    printf '%s\n' 'codex-device-login: OAuth permission validation failed; sidecar remains stopped' >&2
    exit 1
fi
capture_oauth_inventory "$inventory_after"
verify_single_account_mutation

sidecar_needs_stop=1
compose up -d --no-deps codex-compat

attempt=0
while test "$attempt" -lt 30; do
    cid=$(compose ps -q codex-compat || true)
    if test -n "$cid"; then
        health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid")
        test "$health" = healthy && break
        test "$health" = unhealthy && {
            printf '%s\n' 'codex-device-login: restarted sidecar is unhealthy' >&2
            exit 1
        }
    fi
    attempt=$((attempt + 1))
    sleep 2
done
test "${health:-}" = healthy || {
    printf '%s\n' 'codex-device-login: timed out waiting for sidecar health' >&2
    exit 1
}

if ! compose exec -T codex-compat /usr/local/bin/sidecar-smoke; then
    printf '%s\n' 'codex-device-login: upstream smoke test failed; sidecar has been stopped' >&2
    exit 1
fi

sidecar_needs_stop=0
printf '%s\n' 'One Codex account was added or refreshed; all other accounts, permissions, health, and upstream smoke checks passed'
