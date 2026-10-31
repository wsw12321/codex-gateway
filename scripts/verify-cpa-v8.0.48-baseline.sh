#!/bin/sh
# Stage-1 guard: verify the recorded CPA v8.0.48 candidate and the untouched
# v8.0.30 deployment baseline. This script is intentionally read-only.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
baseline=$root/deploy/codex-compat/cpa-v8.0.48-upgrade-baseline.json
source_lock=$root/deploy/codex-compat/source.lock.json
patch=$root/deploy/codex-compat/cliproxy-v8.0.30-gateway.patch
compose=$root/docker-compose.yml

command -v jq >/dev/null 2>&1 || { echo 'jq is required' >&2; exit 2; }
command -v sha256sum >/dev/null 2>&1 || { echo 'sha256sum is required' >&2; exit 2; }
test -f "$baseline" && test -f "$source_lock" && test -f "$patch" && test -f "$compose"

jq -e '
  .schema_version == 1 and
  .status == "preparation_only" and
  .target.tag == "v8.0.48" and
  .target.commit == "dbb706215a3afd2f0ea3c96482d4233856466caa" and
  .current_deployment.tag == "v8.0.30" and
  .rollback.required_before_cutover == true and
  .rollback.registry_digest == null and
  .rebase_probe.conflict_files == 47 and
  (.guardrails | index("Do not change deploy/codex-compat/source.lock.json, docker-compose.yml, or validate-compose.sh in this preparation commit.") != null)
' "$baseline" >/dev/null

jq -e '
  .tag == "v8.0.30" and
  .commit == "5be2cdab0869cbaa3d401891f740aebf7e15aa99" and
  .patch == "cliproxy-v8.0.30-gateway.patch" and
  .patch_sha256 == "f10e3cfd34208d912a216a414807bcc6262f14389d4738d71141d7db40a3f876"
' "$source_lock" >/dev/null
printf '%s  %s\n' f10e3cfd34208d912a216a414807bcc6262f14389d4738d71141d7db40a3f876 "$patch" | sha256sum -c - >/dev/null

grep -Fq 'CLIPROXY_VERSION: v8.0.30' "$compose"
grep -Fq 'CLIPROXY_COMMIT: 5be2cdab0869cbaa3d401891f740aebf7e15aa99' "$compose"
grep -Fq 'cliproxy-v8.0.30-gateway.patch' "$root/deploy/codex-compat/Dockerfile"

echo 'CPA v8.0.48 preparation baseline is intact; active deployment remains v8.0.30.'
