#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# This is trusted repository code, never a shell interpretation of .env.
. "$root/scripts/egress-common.sh"
egress_prepare "$root"
egress_compose "$@"
