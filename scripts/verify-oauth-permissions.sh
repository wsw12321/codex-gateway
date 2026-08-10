#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose=$root/scripts/compose.sh

"$compose" \
    exec -T codex-compat /usr/local/bin/sidecar-entrypoint verify-oauth

printf '%s\n' 'OAuth ownership and mode checks passed'
