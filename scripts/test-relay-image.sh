#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# No site environment, credentials, external network or published port is used.
exec python3 "$root/scripts/tests/relay_image.py" "$@"
