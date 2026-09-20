#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose=$root/scripts/compose.sh

"$compose" exec -T gateway wget -q -O /dev/null http://127.0.0.1:8080/readyz || {
    printf '%s\n' 'smoke-sidecar: start the matching Gateway and wait for database readiness first' >&2
    exit 1
}

"$compose" \
    exec -T codex-compat /usr/local/bin/sidecar-smoke "$@"
