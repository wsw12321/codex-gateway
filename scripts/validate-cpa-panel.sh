#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
sha256sum -c deploy/cpa-panel/upstream.sha256
sha256sum -c deploy/cpa-panel/assets.sha256
node --input-type=module - <<'JS'
import fs from 'node:fs';
const pin = JSON.parse(fs.readFileSync('deploy/cpa-panel/UPSTREAM.json', 'utf8'));
if (pin.tag !== 'v1.25.0' || pin.commit !== 'b87b9487f63e08ad97b1fb4e7c17b4adb811b922') throw Error('unexpected panel source pin');
const html = fs.readFileSync('internal/server/assets/cpa/index.html', 'utf8');
if (/<script(?![^>]*\bsrc=)[^>]*>|<style\b|\bon[a-z]+=/i.test(html)) throw Error('inline panel script/style rejected by CSP');
const js = fs.readFileSync('internal/server/assets/cpa/panel.js', 'utf8');
for (const capability of ['localStorage', '/v0/management', '/v8/management', 'config.yaml', 'auth-files/download', 'api-call', 'managementKey']) {
  if (js.includes(capability)) throw Error(`forbidden panel capability: ${capability}`);
}
console.log('Restricted CPA panel source pin, asset digests and CSP verified');
JS
