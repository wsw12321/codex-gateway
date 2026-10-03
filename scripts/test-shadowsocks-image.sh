#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# Synthetic credentials and an isolated network namespace; no site .env,
# production services, published ports or external upstreams are used.
exec python3 "$root/scripts/tests/shadowsocks_image.py" "$@"
