#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose=$root/scripts/compose.sh
lock_file=$root/.antigravity-login.lock
test "$#" -eq 0 || { printf '%s\n' 'usage: antigravity-login.sh' >&2; exit 1; }
command -v flock >/dev/null 2>&1 || { printf '%s\n' 'antigravity-login: flock is required' >&2; exit 1; }
if test -e "$lock_file" || test -L "$lock_file"; then
    test -f "$lock_file" && test ! -L "$lock_file" || { printf '%s\n' 'antigravity-login: unsafe lock file' >&2; exit 1; }
fi
exec 9>"$lock_file"
chmod 0600 "$lock_file"
flock -n 9 || { printf '%s\n' 'antigravity-login: another login holds the lock' >&2; exit 1; }
bridge_started=0
cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if test "$bridge_started" -eq 1; then
        "$compose" stop -t 10 antigravity-bridge >/dev/null 2>&1 || status=1
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
# A single Secret Service owns the volume. Failed login leaves the bridge
# stopped; Codex and Gateway continue serving their other models.
"$compose" stop -t 10 antigravity-bridge
container_id=$("$compose" ps -q antigravity-bridge)
if test -n "$container_id"; then
    test "$(docker inspect -f '{{.State.Running}}' "$container_id")" = false || {
        printf '%s\n' 'antigravity-login: bridge still running; refusing shared keyring access' >&2
        exit 1
    }
fi
"$compose" up -d egress-allowlist
printf '%s\n' 'Complete the official agy remote login, then use /exit. Models, /usage and a text request are checked next.'
"$compose" run --rm --no-deps antigravity-bridge login
# A second container verifies that encrypted credentials survive a new D-Bus
# session and a new ephemeral HOME before the serving container is restarted.
"$compose" run --rm --no-deps antigravity-bridge verify-login
bridge_started=1
"$compose" up -d --no-deps antigravity-bridge
attempt=0
while test "$attempt" -lt 30; do
    if "$compose" exec -T antigravity-bridge curl -fsS --max-time 4 http://127.0.0.1:8318/readyz >/dev/null 2>&1; then
        if "$compose" exec -T antigravity-bridge /usr/local/bin/antigravity-smoke; then
            bridge_started=0
            printf '%s\n' 'Antigravity login persisted; readiness, JSON and SSE passed.'
            exit 0
        fi
        break
    fi
    attempt=$((attempt + 1))
    sleep 2
done
printf '%s\n' 'antigravity-login: readiness or HTTP smoke failed; bridge stopped' >&2
exit 1
