#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# Use only the pinned Squid image and its isolated loopback namespace.
exec python3 "$root/scripts/tests/oidc_egress_image.py" "$@"
