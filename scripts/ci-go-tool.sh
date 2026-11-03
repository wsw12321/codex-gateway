#!/usr/bin/env bash
# Run a version-pinned Go tool in CI. The module proxy and sum.golang.org
# occasionally reset connections, so only the checksum-verified download and
# build are retried. The tool itself runs once and keeps its exit status, so a
# real finding is never retried away.
#
# Usage: scripts/ci-go-tool.sh module/path/cmd@vX.Y.Z [-- tool arguments...]
set -euo pipefail

usage() {
    printf 'usage: %s module/path/cmd@vX.Y.Z [-- tool arguments...]\n' "$0" >&2
    exit 2
}

test "$#" -ge 1 || usage
tool=$1
shift
if test "$#" -gt 0; then
    test "$1" = -- || usage
    shift
fi
if ! [[ $tool =~ ^[A-Za-z0-9._~/-]+@v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    printf 'tool must be pinned to an exact version: %s\n' "$tool" >&2
    exit 2
fi

package=${tool%@*}
name=${package##*/}
# A major-version suffix (module/v2) is not the command name.
if [[ $name =~ ^v[0-9]+$ ]]; then
    package=${package%/*}
    name=${package##*/}
fi

GOBIN=$(mktemp -d)
export GOBIN
attempt=1
until go install "$tool"; do
    test "$attempt" -lt 3 || exit 1
    printf 'go install %s failed (attempt %d of 3); retrying\n' "$tool" "$attempt" >&2
    sleep $((attempt * 15))
    attempt=$((attempt + 1))
done
exec "$GOBIN/$name" "$@"
