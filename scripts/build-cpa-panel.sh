#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/gateway-cpa-panel.XXXXXXXX")
trap 'rm -rf "$build_dir"' EXIT HUP INT TERM
cp -R "$root/deploy/cpa-panel/." "$build_dir/"
cd "$build_dir"
bun_bin=${CPA_PANEL_BUN:-bun}
test "$("$bun_bin" --version)" = '1.3.14' || { echo 'CPA panel requires Bun 1.3.14' >&2; exit 1; }
"$bun_bin" install --frozen-lockfile
"$bun_bin" ./node_modules/typescript/bin/tsc
"$bun_bin" ./node_modules/vite/bin/vite.js build --outDir "$root/internal/server/assets/cpa"
cd "$root"
sha256sum internal/server/assets/cpa/index.html internal/server/assets/cpa/panel.js internal/server/assets/cpa/panel.css > deploy/cpa-panel/assets.sha256
