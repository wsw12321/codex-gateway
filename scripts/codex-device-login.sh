#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_cmd=$root/scripts/compose.sh
lock_file=$root/.device-login.lock
sidecar_needs_stop=0

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
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

printf '%s\n' 'Stopping the only sidecar instance before refresh-token mutation...'
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

printf '%s\n' 'Starting interactive Codex device login. OAuth values are never printed by this wrapper.'
if ! compose run --rm --no-deps codex-compat --codex-device-login; then
    printf '%s\n' 'codex-device-login: login failed; sidecar remains stopped' >&2
    exit 1
fi

if ! compose run --rm --no-deps codex-compat verify-oauth; then
    printf '%s\n' 'codex-device-login: OAuth permission validation failed; sidecar remains stopped' >&2
    exit 1
fi

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
printf '%s\n' 'Codex device login, permission validation, and upstream smoke tests succeeded'
